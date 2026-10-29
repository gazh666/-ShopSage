package extensions

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"shopsage/backend/src/domain"
)

// AccountLookup 只暴露扩展需要的鉴权读取能力，便于单元测试替身实现。
type AccountLookup interface {
	GetAccountByToken(ctx context.Context, token string) (domain.Account, bool)
}

// TelemetryStore 是扩展埋点读写能力。
type TelemetryStore interface {
	InsertExtTelemetryEvents(ctx context.Context, events []domain.ExtTelemetryEvent) error
	ListExtTelemetryEvents(ctx context.Context, since time.Time, limit int) ([]domain.ExtTelemetryEvent, error)
}

// ConfigCenter 只需提供配置字典，用于复用原有的 CORS 白名单配置。
type ConfigCenter interface {
	GetMap(ctx context.Context) map[string]string
}

type Deps struct {
	Accounts  AccountLookup
	Telemetry TelemetryStore
	Configs   ConfigCenter
	Logger    *slog.Logger
}

// Wrap 在原始路由外层叠加扩展能力。
//
// EXT_ENABLED=false（默认）时直接返回 base，不注册任何扩展路由、不启动埋点协程、
// 不访问数据库，保证关闭开关后的行为与原始版本完全一致。
func Wrap(base http.Handler, deps Deps) http.Handler {
	cfg := LoadConfig()
	if !cfg.Enabled || deps.Accounts == nil {
		return base
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	wrapper := &Wrapper{
		base:    base,
		cfg:     cfg,
		deps:    deps,
		logger:  logger,
		limiter: newRateLimiter(120, time.Minute),
		client: &http.Client{
			Timeout: cfg.FitlabTimeout,
		},
	}
	wrapper.mux = wrapper.buildMux()
	if cfg.TelemetryEnabled && deps.Telemetry != nil {
		wrapper.recorder = newTelemetryRecorder(deps.Telemetry, logger)
	}
	logger.Info("extensions enabled",
		"fitlab_enabled", cfg.FitlabEnabled,
		"fitlab_base_urls", cfg.FitlabBaseURLs,
		"telemetry_enabled", cfg.TelemetryEnabled,
	)
	return wrapper
}

type Wrapper struct {
	base     http.Handler
	cfg      Config
	deps     Deps
	logger   *slog.Logger
	mux      *http.ServeMux
	recorder *telemetryRecorder
	limiter  *rateLimiter
	client   *http.Client

	fitlabMu   sync.RWMutex
	favoredURL string
}

type extContextKey string

const (
	extRequestIDKey extContextKey = "ext_request_id"
	extAccountKey   extContextKey = "ext_account"
)

func isExtensionPath(path string) bool {
	switch {
	case strings.HasPrefix(path, "/api/v1/fitlab/"):
		return true
	case strings.HasPrefix(path, "/api/v1/ext/"):
		return true
	case strings.HasPrefix(path, "/api/v1/admin/ext/"):
		return true
	default:
		return false
	}
}

func (e *Wrapper) buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/fitlab/health", e.handleFitlabHealth)
	mux.HandleFunc("/api/v1/fitlab/tryon", e.handleFitlabTryon)
	mux.HandleFunc("/api/v1/ext/telemetry/events", e.handleTelemetryIngest)
	mux.HandleFunc("/api/v1/admin/ext/overview", e.handleExtOverview)
	mux.HandleFunc("/api/v1/admin/ext/eval/reports", e.handleExtEvalReports)
	mux.HandleFunc("/api/v1/admin/ext/eval/reports/detail", e.handleExtEvalReportDetail)
	mux.HandleFunc("/api/v1/fitlab/", e.handleExtNotFound)
	mux.HandleFunc("/api/v1/ext/", e.handleExtNotFound)
	mux.HandleFunc("/api/v1/admin/ext/", e.handleExtNotFound)
	return mux
}

func (e *Wrapper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !isExtensionPath(r.URL.Path) {
		e.serveBase(w, r)
		return
	}
	started := time.Now()
	requestID := newExtRequestID()
	w.Header().Set("X-Request-ID", requestID)
	r = r.WithContext(context.WithValue(r.Context(), extRequestIDKey, requestID))

	e.writeCORSHeaders(w, r)

	recorder := &extResponseRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		if r.Method == http.MethodOptions || e.recorder == nil {
			return
		}
		account, _ := exAccountFromContext(r.Context())
		e.recorder.enqueue(domain.ExtTelemetryEvent{
			EventID:      newExtRequestID(),
			EventType:    "ext_http_request",
			Path:         r.URL.Path,
			Method:       r.Method,
			Status:       recorder.status,
			DurationMS:   time.Since(started).Milliseconds(),
			AccountID:    account.AccountID,
			Role:         string(account.Role),
			Source:       "server",
			MetadataJSON: `{"extension":true}`,
			CreatedAt:    time.Now(),
		})
	}()

	if r.Method == http.MethodOptions {
		recorder.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		// 与原始路由保持一致的请求体上限，避免扩展端点绕过保护。
		r.Body = http.MaxBytesReader(recorder, r.Body, 12<<20)
	}

	account, hasAccount := e.accountFromRequest(r)
	if hasAccount {
		r = r.WithContext(context.WithValue(r.Context(), extAccountKey, account))
	}
	required, roles := extRoutePolicy(r.Method, r.URL.Path)
	if required {
		if !hasAccount {
			writeExtError(recorder, r, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		if !extRoleAllowed(account.Role, roles) {
			writeExtError(recorder, r, http.StatusForbidden, "forbidden", "当前账号无权访问")
			return
		}
	}
	key := clientIP(r)
	if hasAccount {
		key = string(account.Role) + ":" + account.AccountID
	}
	if !e.limiter.allow(key, time.Now()) {
		writeExtError(recorder, r, http.StatusTooManyRequests, "rate_limited", "请求过于频繁，请稍后再试")
		return
	}

	e.mux.ServeHTTP(recorder, r)
}

func (e *Wrapper) serveBase(w http.ResponseWriter, r *http.Request) {
	if e.recorder == nil {
		e.base.ServeHTTP(w, r)
		return
	}
	started := time.Now()
	recorder := &extResponseRecorder{ResponseWriter: w, status: http.StatusOK}
	e.base.ServeHTTP(recorder, r)
	if r.Method == http.MethodOptions {
		return
	}
	e.recorder.enqueue(domain.ExtTelemetryEvent{
		EventID:      newExtRequestID(),
		EventType:    "http_request",
		Path:         r.URL.Path,
		Method:       r.Method,
		Status:       recorder.status,
		DurationMS:   time.Since(started).Milliseconds(),
		Source:       "server",
		MetadataJSON: `{"extension":false}`,
		CreatedAt:    time.Now(),
	})
}

func (e *Wrapper) accountFromRequest(r *http.Request) (domain.Account, bool) {
	if account, ok := exAccountFromContext(r.Context()); ok {
		return account, true
	}
	token := bearerToken(r)
	if token == "" {
		return domain.Account{}, false
	}
	return e.deps.Accounts.GetAccountByToken(r.Context(), token)
}

func (e *Wrapper) writeCORSHeaders(w http.ResponseWriter, r *http.Request) {
	allowed := e.allowedCORSOrigin(r.Context(), r.Header.Get("Origin"))
	if allowed != "" {
		w.Header().Set("Access-Control-Allow-Origin", allowed)
		w.Header().Set("Vary", "Origin")
	}
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, Authorization")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
}

func (e *Wrapper) allowedCORSOrigin(ctx context.Context, origin string) string {
	allowed := "*"
	if e.deps.Configs != nil {
		if value := strings.TrimSpace(e.deps.Configs.GetMap(ctx)["http.cors.allowed_origins"]); value != "" {
			allowed = value
		}
	}
	if allowed == "*" {
		return "*"
	}
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return ""
	}
	for _, item := range strings.Split(allowed, ",") {
		if strings.TrimSpace(item) == origin {
			return origin
		}
	}
	return ""
}

func (e *Wrapper) handleExtNotFound(w http.ResponseWriter, r *http.Request) {
	writeExtError(w, r, http.StatusNotFound, "not_found", "扩展接口不存在")
}

func extRoutePolicy(method string, path string) (bool, []domain.AccountRole) {
	if method == http.MethodGet && path == "/api/v1/fitlab/health" {
		return false, nil
	}
	if method == http.MethodPost && path == "/api/v1/ext/telemetry/events" {
		// 前端公开页面也要能上报曝光/点击；该端点只写扩展埋点表，不触达业务数据。
		return false, nil
	}
	if strings.HasPrefix(path, "/api/v1/admin/ext/") {
		return true, []domain.AccountRole{domain.AccountRoleAdmin}
	}
	return true, []domain.AccountRole{domain.AccountRoleUser, domain.AccountRoleMerchant, domain.AccountRoleAdmin}
}

func extRoleAllowed(role domain.AccountRole, allowed []domain.AccountRole) bool {
	for _, item := range allowed {
		if item == role {
			return true
		}
	}
	return false
}

func exAccountFromContext(ctx context.Context) (domain.Account, bool) {
	account, ok := ctx.Value(extAccountKey).(domain.Account)
	return account, ok
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if token == "" || token == header {
		return ""
	}
	return token
}

func clientIP(r *http.Request) string {
	remote := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	if remote == "" {
		return "unknown"
	}
	return remote
}

func newExtRequestID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return time.Now().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(bytes[:])
}

func requestIDFromExtContext(ctx context.Context) string {
	requestID, _ := ctx.Value(extRequestIDKey).(string)
	return requestID
}

func writeExtJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeExtError(w http.ResponseWriter, r *http.Request, status int, code string, message string) {
	writeExtJSON(w, status, map[string]any{
		"code":       code,
		"message":    message,
		"request_id": requestIDFromExtContext(r.Context()),
	})
}

// extResponseRecorder 在保留原有 Flush/Hijack 能力的前提下记录状态码，
// 保证 SSE 与 WebSocket 等长连接经过扩展层时行为不变。
type extResponseRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *extResponseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *extResponseRecorder) Write(payload []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(payload)
}

func (r *extResponseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *extResponseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

type rateLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	limit   int
	buckets map[string]*rateBucket
}

type rateBucket struct {
	resetAt time.Time
	count   int
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		window:  window,
		limit:   limit,
		buckets: map[string]*rateBucket{},
	}
}

func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if bucket, ok := l.buckets[key]; ok {
		if now.Before(bucket.resetAt) {
			if bucket.count >= l.limit {
				return false
			}
			bucket.count++
			return true
		}
	}
	if len(l.buckets) > 20000 {
		for bucketKey, bucket := range l.buckets {
			if now.After(bucket.resetAt) {
				delete(l.buckets, bucketKey)
			}
		}
	}
	l.buckets[key] = &rateBucket{resetAt: now.Add(l.window), count: 1}
	return true
}

// projectPath 与原始评测接口保持相同的“相对仓库根目录”查找逻辑，
// 只在扩展包内实现一份，避免改动原有 handler。
func projectPath(parts ...string) string {
	path := filepath.Join(parts...)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	current, err := os.Getwd()
	if err != nil {
		return filepath.Join(append([]string{".."}, parts...)...)
	}
	for {
		candidate := filepath.Join(append([]string{current}, parts...)...)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return filepath.Join(append([]string{".."}, parts...)...)
}
