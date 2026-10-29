package extensions

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type evalReportSummary struct {
	ID          string    `json:"id"`
	RunID       string    `json:"run_id"`
	Kind        string    `json:"kind"`
	Path        string    `json:"path"`
	Type        string    `json:"type"`
	Dataset     string    `json:"dataset"`
	Total       int       `json:"total"`
	Evaluated   int       `json:"evaluated"`
	Hits        int       `json:"hits"`
	PassRate    float64   `json:"pass_rate"`
	GeneratedAt time.Time `json:"generated_at"`
}

type evalSummaryView struct {
	Dir     string              `json:"dir"`
	Reports int                 `json:"reports"`
	Latest  []evalReportSummary `json:"latest"`
	Error   string              `json:"error,omitempty"`
}

func (e *Wrapper) evalDir() string {
	if strings.TrimSpace(e.cfg.EvalDir) != "" {
		return e.cfg.EvalDir
	}
	return projectPath("quality", "reports")
}

func (e *Wrapper) evalSummary() evalSummaryView {
	dir := e.evalDir()
	reports, err := listExtEvalReports(dir)
	view := evalSummaryView{Dir: dir, Latest: []evalReportSummary{}}
	if err != nil {
		view.Error = "eval_reports_unavailable"
		return view
	}
	view.Reports = len(reports)
	limit := len(reports)
	if limit > 5 {
		limit = 5
	}
	view.Latest = append(view.Latest, reports[:limit]...)
	return view
}

func (e *Wrapper) handleExtEvalReports(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeExtError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET")
		return
	}
	dir := e.evalDir()
	reports, err := listExtEvalReports(dir)
	if err != nil {
		writeExtJSON(w, http.StatusOK, map[string]any{
			"dir":     dir,
			"reports": []evalReportSummary{},
			"error":   "eval_reports_unavailable",
		})
		return
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("kind")); raw != "" {
		filtered := make([]evalReportSummary, 0, len(reports))
		for _, report := range reports {
			if report.Kind == raw || report.Type == raw {
				filtered = append(filtered, report)
			}
		}
		reports = filtered
	}
	writeExtJSON(w, http.StatusOK, map[string]any{
		"dir":     dir,
		"reports": reports,
	})
}

func (e *Wrapper) handleExtEvalReportDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeExtError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeExtError(w, r, http.StatusBadRequest, "missing_id", "缺少报告 id")
		return
	}
	reports, err := listExtEvalReports(e.evalDir())
	if err != nil {
		writeExtError(w, r, http.StatusServiceUnavailable, "eval_reports_unavailable", "评测报告目录不可用")
		return
	}
	for _, report := range reports {
		if report.ID != id {
			continue
		}
		raw, err := readExtEvalFile(report.Path)
		if err != nil {
			writeExtError(w, r, http.StatusNotFound, "report_not_found", "报告文件不可读")
			return
		}
		writeExtJSON(w, http.StatusOK, map[string]any{
			"report": report,
			"raw":    raw,
		})
		return
	}
	writeExtError(w, r, http.StatusNotFound, "report_not_found", "报告不存在")
}

func listExtEvalReports(root string) ([]evalReportSummary, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	items := make([]evalReportSummary, 0)
	for _, runDir := range entries {
		if !runDir.IsDir() {
			continue
		}
		reportDir := filepath.Join(root, runDir.Name())
		files, err := os.ReadDir(reportDir)
		if err != nil {
			continue
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
				continue
			}
			path := filepath.Join(reportDir, file.Name())
			summary, ok := parseExtEvalReport(path, runDir.Name(), strings.TrimSuffix(file.Name(), ".json"))
			if ok {
				items = append(items, summary)
			}
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].GeneratedAt.After(items[j].GeneratedAt)
	})
	if len(items) > 200 {
		items = items[:200]
	}
	return items, nil
}

func parseExtEvalReport(path string, runID string, kind string) (evalReportSummary, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return evalReportSummary{}, false
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return evalReportSummary{}, false
	}
	if summary, ok := payload["summary"].(map[string]any); ok {
		for key, value := range summary {
			if _, exists := payload[key]; !exists {
				payload[key] = value
			}
		}
	}
	report := evalReportSummary{
		ID:        runID + "/" + kind,
		RunID:     runID,
		Kind:      kind,
		Path:      filepath.ToSlash(path),
		Type:      stringFromAny(payload["type"]),
		Dataset:   stringFromAny(payload["dataset"]),
		Total:     intFromAny(payload["total"]),
		Evaluated: intFromAny(payload["evaluated"]),
		Hits:      intFromAny(payload["hits"]),
		PassRate:  numberFromAny(payload["pass_rate"]),
	}
	if report.Type == "" {
		report.Type = kind
	}
	if report.Hits == 0 {
		if correct := intFromAny(payload["correct"]); correct > 0 {
			report.Hits = correct
		}
	}
	if report.Evaluated == 0 {
		report.Evaluated = report.Total
	}
	if report.PassRate == 0 && report.Evaluated > 0 {
		report.PassRate = float64(report.Hits) / float64(report.Evaluated)
	}
	if generated := stringFromAny(payload["generated_at"]); generated != "" {
		if parsed, err := time.Parse(time.RFC3339, generated); err == nil {
			report.GeneratedAt = parsed
		}
	}
	if report.GeneratedAt.IsZero() {
		if info, err := os.Stat(path); err == nil {
			report.GeneratedAt = info.ModTime()
		}
	}
	return report, true
}

func readExtEvalFile(path string) (any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var payload any
	if err := json.NewDecoder(file).Decode(&payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func stringFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	default:
		return ""
	}
}

func numberFromAny(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		if parsed, err := v.Float64(); err == nil {
			return parsed
		}
	case string:
		if parsed, err := strconv.ParseFloat(v, 64); err == nil {
			return parsed
		}
	}
	return 0
}

func intFromAny(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		if parsed, err := v.Int64(); err == nil {
			return int(parsed)
		}
	case string:
		if parsed, err := strconv.Atoi(v); err == nil {
			return parsed
		}
	}
	return 0
}
