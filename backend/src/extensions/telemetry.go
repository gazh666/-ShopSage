package extensions

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"shopsage/backend/src/domain"
)

const (
	telemetryBatchSize     = 64
	telemetryFlushInterval = 5 * time.Second
	telemetryMaxEvents     = 50
)

type telemetryRecorder struct {
	store TelemetryStore
	ch    chan domain.ExtTelemetryEvent
	done  chan struct{}
	once  sync.Once
}

func newTelemetryRecorder(store TelemetryStore, logger *slog.Logger) *telemetryRecorder {
	recorder := &telemetryRecorder{
		store: store,
		ch:    make(chan domain.ExtTelemetryEvent, 1024),
		done:  make(chan struct{}),
	}
	go recorder.loop(logger)
	return recorder
}

func (r *telemetryRecorder) loop(logger *slog.Logger) {
	ticker := time.NewTicker(telemetryFlushInterval)
	defer ticker.Stop()
	batch := make([]domain.ExtTelemetryEvent, 0, telemetryBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := r.store.InsertExtTelemetryEvents(ctx, batch); err != nil {
			logger.Warn("ext telemetry flush failed, events dropped", "count", len(batch), "error", err)
		}
		cancel()
		batch = batch[:0]
	}
	for {
		select {
		case event := <-r.ch:
			batch = append(batch, event)
			if len(batch) >= telemetryBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-r.done:
			flush()
			return
		}
	}
}

// enqueue 采用非阻塞写入，队列满时丢弃并继续服务请求，避免埋点拖垮主链路。
func (r *telemetryRecorder) enqueue(event domain.ExtTelemetryEvent) {
	select {
	case r.ch <- event:
	default:
	}
}

func (r *telemetryRecorder) Stop() {
	r.once.Do(func() {
		close(r.done)
	})
}

// Close 仅用于测试优雅结束埋点协程，生产进程随服务生命周期退出。
func (e *Wrapper) Close() {
	if e.recorder != nil {
		e.recorder.Stop()
	}
}

type clientTelemetryEvent struct {
	EventType  string          `json:"event_type"`
	Path       string          `json:"path"`
	Method     string          `json:"method"`
	Status     int             `json:"status"`
	DurationMS int64           `json:"duration_ms"`
	Metadata   json.RawMessage `json:"metadata"`
}

type clientTelemetryPayload struct {
	Events []clientTelemetryEvent `json:"events"`
}

func (e *Wrapper) handleTelemetryIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeExtError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST")
		return
	}
	if e.deps.Telemetry == nil || !e.cfg.TelemetryEnabled {
		writeExtError(w, r, http.StatusServiceUnavailable, "telemetry_disabled", "埋点能力未启用")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, e.cfg.TelemetryMaxBody)
	var payload clientTelemetryPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeExtError(w, r, http.StatusBadRequest, "bad_request", "埋点 JSON 不合法或超过大小限制")
		return
	}
	if len(payload.Events) == 0 {
		writeExtJSON(w, http.StatusAccepted, map[string]any{"accepted": 0})
		return
	}
	if len(payload.Events) > telemetryMaxEvents {
		payload.Events = payload.Events[:telemetryMaxEvents]
	}
	account, _ := exAccountFromContext(r.Context())
	now := time.Now()
	accepted := 0
	for _, item := range payload.Events {
		eventType := trimLimit(item.EventType, 64)
		if eventType == "" {
			eventType = "client_event"
		}
		metadata := "{}"
		if len(item.Metadata) > 0 && json.Valid(item.Metadata) {
			metadata = trimLimit(string(item.Metadata), 2000)
		}
		e.recorder.enqueue(domain.ExtTelemetryEvent{
			EventID:      newExtRequestID(),
			EventType:    eventType,
			Path:         trimLimit(item.Path, 255),
			Method:       trimLimit(item.Method, 16),
			Status:       item.Status,
			DurationMS:   item.DurationMS,
			AccountID:    account.AccountID,
			Role:         string(account.Role),
			Source:       "client",
			MetadataJSON: metadata,
			CreatedAt:    now.Add(time.Duration(accepted) * time.Microsecond),
		})
		accepted++
	}
	writeExtJSON(w, http.StatusAccepted, map[string]any{"accepted": accepted})
}

type extConfigView struct {
	FitlabEnabled    bool     `json:"fitlab_enabled"`
	TelemetryEnabled bool     `json:"telemetry_enabled"`
	FitlabBaseURLs   []string `json:"fitlab_base_urls"`
	EvalDir          string   `json:"eval_dir"`
}

type telemetryPathStat struct {
	Path      string  `json:"path"`
	Count     int     `json:"count"`
	Errors    int     `json:"errors"`
	ErrorRate float64 `json:"error_rate"`
	AverageMS int64   `json:"average_ms"`
	P95MS     int64   `json:"p95_ms"`
}

type telemetryEventView struct {
	EventID    string    `json:"event_id"`
	EventType  string    `json:"event_type"`
	Path       string    `json:"path"`
	Method     string    `json:"method"`
	Status     int       `json:"status"`
	DurationMS int64     `json:"duration_ms"`
	AccountID  string    `json:"account_id,omitempty"`
	Role       string    `json:"role,omitempty"`
	Source     string    `json:"source"`
	CreatedAt  time.Time `json:"created_at"`
}

type telemetrySummary struct {
	WindowHours    int                  `json:"window_hours"`
	Enabled        bool                 `json:"enabled"`
	Events         int                  `json:"events"`
	HTTPRequests   int                  `json:"http_requests"`
	ClientEvents   int                  `json:"client_events"`
	Errors         int                  `json:"errors"`
	ErrorRate      float64              `json:"error_rate"`
	AverageMS      int64                `json:"average_ms"`
	P95MS          int64                `json:"p95_ms"`
	UniqueAccounts int                  `json:"unique_accounts"`
	ByPath         []telemetryPathStat  `json:"by_path"`
	Recent         []telemetryEventView `json:"recent"`
	Error          string               `json:"error,omitempty"`
}

func (e *Wrapper) handleExtOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeExtError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET")
		return
	}
	window := e.cfg.TelemetryWindow
	if raw := strings.TrimSpace(r.URL.Query().Get("hours")); raw != "" {
		if hours, err := strconv.Atoi(raw); err == nil && hours > 0 && hours <= 24*30 {
			window = time.Duration(hours) * time.Hour
		}
	}
	writeExtJSON(w, http.StatusOK, map[string]any{
		"enabled": true,
		"config": extConfigView{
			FitlabEnabled:    e.cfg.FitlabEnabled,
			TelemetryEnabled: e.cfg.TelemetryEnabled,
			FitlabBaseURLs:   e.cfg.FitlabBaseURLs,
			EvalDir:          e.cfg.EvalDir,
		},
		"fitlab":    e.probeFitlab(r.Context()),
		"telemetry": e.telemetrySummary(r.Context(), window),
		"eval":      e.evalSummary(),
	})
}

func (e *Wrapper) telemetrySummary(ctx context.Context, window time.Duration) telemetrySummary {
	summary := telemetrySummary{
		WindowHours: int(window / time.Hour),
		Enabled:     e.cfg.TelemetryEnabled && e.deps.Telemetry != nil,
		ByPath:      []telemetryPathStat{},
		Recent:      []telemetryEventView{},
	}
	if !summary.Enabled {
		summary.Error = "telemetry_disabled"
		return summary
	}
	events, err := e.deps.Telemetry.ListExtTelemetryEvents(ctx, time.Now().Add(-window), 20000)
	if err != nil {
		summary.Error = "telemetry_store_unavailable"
		return summary
	}
	return summarizeTelemetry(summary.WindowHours, events)
}

func summarizeTelemetry(windowHours int, events []domain.ExtTelemetryEvent) telemetrySummary {
	summary := telemetrySummary{
		WindowHours: windowHours,
		Enabled:     true,
		Events:      len(events),
		ByPath:      []telemetryPathStat{},
		Recent:      []telemetryEventView{},
	}
	type pathAgg struct {
		count  int
		errors int
		dur    []int64
	}
	byPath := map[string]*pathAgg{}
	accounts := map[string]bool{}
	durations := make([]int64, 0, len(events))
	for _, event := range events {
		if event.Source == "client" {
			summary.ClientEvents++
		} else {
			summary.HTTPRequests++
		}
		if event.Status >= 400 {
			summary.Errors++
		}
		if event.Path != "" {
			aggregate := byPath[event.Path]
			if aggregate == nil {
				aggregate = &pathAgg{}
				byPath[event.Path] = aggregate
			}
			aggregate.count++
			if event.Status >= 400 {
				aggregate.errors++
			}
			if event.DurationMS > 0 {
				aggregate.dur = append(aggregate.dur, event.DurationMS)
			}
		}
		if event.DurationMS > 0 {
			durations = append(durations, event.DurationMS)
		}
		if event.AccountID != "" {
			accounts[event.AccountID] = true
		}
	}
	summary.UniqueAccounts = len(accounts)
	if summary.Events > 0 {
		summary.ErrorRate = float64(summary.Errors) / float64(summary.Events)
	}
	summary.AverageMS = averageInt64(durations)
	summary.P95MS = percentileInt64(durations, 0.95)

	for path, aggregate := range byPath {
		stat := telemetryPathStat{
			Path:      path,
			Count:     aggregate.count,
			Errors:    aggregate.errors,
			AverageMS: averageInt64(aggregate.dur),
			P95MS:     percentileInt64(aggregate.dur, 0.95),
		}
		if aggregate.count > 0 {
			stat.ErrorRate = float64(aggregate.errors) / float64(aggregate.count)
		}
		summary.ByPath = append(summary.ByPath, stat)
	}
	sort.Slice(summary.ByPath, func(i, j int) bool {
		if summary.ByPath[i].Count == summary.ByPath[j].Count {
			return summary.ByPath[i].Path < summary.ByPath[j].Path
		}
		return summary.ByPath[i].Count > summary.ByPath[j].Count
	})
	if len(summary.ByPath) > 12 {
		summary.ByPath = summary.ByPath[:12]
	}
	limit := len(events)
	if limit > 10 {
		limit = 10
	}
	for index := 0; index < limit; index++ {
		event := events[index]
		summary.Recent = append(summary.Recent, telemetryEventView{
			EventID:    event.EventID,
			EventType:  event.EventType,
			Path:       event.Path,
			Method:     event.Method,
			Status:     event.Status,
			DurationMS: event.DurationMS,
			AccountID:  event.AccountID,
			Role:       event.Role,
			Source:     event.Source,
			CreatedAt:  event.CreatedAt,
		})
	}
	return summary
}

func trimLimit(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func averageInt64(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	var sum int64
	for _, value := range values {
		sum += value
	}
	return sum / int64(len(values))
}

func percentileInt64(values []int64, quantile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(float64(len(sorted)-1) * quantile)
	return sorted[index]
}
