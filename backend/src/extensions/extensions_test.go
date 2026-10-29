package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shopsage/backend/src/domain"
)

type stubBase struct {
	calls int
	path  string
}

func (s *stubBase) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.calls++
	s.path = r.URL.Path
	w.Header().Set("X-Base", "1")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "base")
}

type fakeAccounts struct {
	accounts map[string]domain.Account
	calls    int
	mu       sync.Mutex
}

func (f *fakeAccounts) GetAccountByToken(_ context.Context, token string) (domain.Account, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	account, ok := f.accounts[token]
	return account, ok
}

type fakeTelemetry struct {
	mu       sync.Mutex
	events   []domain.ExtTelemetryEvent
	listErr  error
	inserted chan []domain.ExtTelemetryEvent
}

func (f *fakeTelemetry) InsertExtTelemetryEvents(_ context.Context, events []domain.ExtTelemetryEvent) error {
	f.mu.Lock()
	f.events = append(f.events, events...)
	f.mu.Unlock()
	if f.inserted != nil {
		f.inserted <- events
	}
	return nil
}

func (f *fakeTelemetry) ListExtTelemetryEvents(_ context.Context, _ time.Time, _ int) ([]domain.ExtTelemetryEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]domain.ExtTelemetryEvent(nil), f.events...), nil
}

type fakeConfigs struct {
	values map[string]string
}

func (f fakeConfigs) GetMap(context.Context) map[string]string {
	return f.values
}

func testDeps(t *testing.T) (Deps, *fakeAccounts, *fakeTelemetry) {
	t.Helper()
	accounts := &fakeAccounts{accounts: map[string]domain.Account{
		"user-token":  {AccountID: "u1", Role: domain.AccountRoleUser},
		"admin-token": {AccountID: "a1", Role: domain.AccountRoleAdmin},
	}}
	telemetry := &fakeTelemetry{inserted: make(chan []domain.ExtTelemetryEvent, 8)}
	return Deps{
		Accounts:  accounts,
		Telemetry: telemetry,
		Configs:   fakeConfigs{values: map[string]string{"http.cors.allowed_origins": "*"}},
	}, accounts, telemetry
}

func testConfig(t *testing.T, fitlabURL string) {
	t.Helper()
	t.Setenv("EXT_ENABLED", "true")
	t.Setenv("EXT_FITLAB_ENABLED", "true")
	t.Setenv("FITLAB_BASE_URL", fitlabURL)
	t.Setenv("FITLAB_TIMEOUT_MS", "2000")
	t.Setenv("EXT_TELEMETRY_ENABLED", "true")
}

func newPNGServer(t *testing.T) *httptest.Server {
	t.Helper()
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "engine": "geometry-v1"})
		case "/v1/tryon":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func multipartRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	_ = writer.WriteField("category", "upper")
	for _, field := range []string{"person_image", "garment_image"} {
		part, err := writer.CreateFormFile(field, field+".png")
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write([]byte("fake-image-bytes")); err != nil {
			t.Fatalf("write form file: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, target, &buffer)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func TestWrapDisabledReturnsOriginalHandler(t *testing.T) {
	t.Setenv("EXT_ENABLED", "false")
	base := &stubBase{}
	deps, accounts, _ := testDeps(t)
	if got := Wrap(base, deps); got != base {
		t.Fatalf("disabled extensions should return the original handler")
	}
	if accounts.calls != 0 {
		t.Fatalf("disabled extensions must not touch auth store, got %d calls", accounts.calls)
	}
}

func TestBaseRoutesPassThroughWhenEnabled(t *testing.T) {
	testConfig(t, "http://127.0.0.1:65535")
	base := &stubBase{}
	deps, _, telemetry := testDeps(t)
	handler := Wrap(base, deps)
	wrapper := handler.(*Wrapper)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("X-Base") != "1" {
		t.Fatalf("base route should be untouched, status=%d", recorder.Code)
	}

	// 关闭扩展时刷新埋点协程，确认 base 路由的请求也被记录且不影响响应。
	wrapper.Close()
	waitForInsert(t, telemetry)
}

func waitForInsert(t *testing.T, telemetry *fakeTelemetry) {
	t.Helper()
	select {
	case <-telemetry.inserted:
	case <-time.After(3 * time.Second):
		t.Fatalf("telemetry event was not flushed")
	}
}

func TestExtensionAuthAndCORS(t *testing.T) {
	server := newPNGServer(t)
	testConfig(t, server.URL)
	deps, _, _ := testDeps(t)
	wrapper := Wrap(&stubBase{}, deps).(*Wrapper)

	health := httptest.NewRecorder()
	wrapper.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/api/v1/fitlab/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health should be public, got %d", health.Code)
	}

	anonymous := multipartRequest(t, "/api/v1/fitlab/tryon")
	anonymousRecorder := httptest.NewRecorder()
	wrapper.ServeHTTP(anonymousRecorder, anonymous)
	if anonymousRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("tryon should require auth, got %d", anonymousRecorder.Code)
	}

	user := multipartRequest(t, "/api/v1/fitlab/tryon")
	user.Header.Set("Authorization", "Bearer user-token")
	user.Header.Set("Origin", "https://shop.example")
	userRecorder := httptest.NewRecorder()
	wrapper.ServeHTTP(userRecorder, user)
	if userRecorder.Code != http.StatusOK {
		t.Fatalf("user tryon should proxy fitlab, got %d: %s", userRecorder.Code, userRecorder.Body.String())
	}
	if userRecorder.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("unexpected content type %q", userRecorder.Header().Get("Content-Type"))
	}
	if userRecorder.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("missing CORS header for extension route")
	}

	adminDenied := httptest.NewRequest(http.MethodGet, "/api/v1/admin/ext/overview", nil)
	adminDenied.Header.Set("Authorization", "Bearer user-token")
	deniedRecorder := httptest.NewRecorder()
	wrapper.ServeHTTP(deniedRecorder, adminDenied)
	if deniedRecorder.Code != http.StatusForbidden {
		t.Fatalf("user should not access admin extension, got %d", deniedRecorder.Code)
	}

	options := httptest.NewRequest(http.MethodOptions, "/api/v1/fitlab/tryon", nil)
	optionsRecorder := httptest.NewRecorder()
	wrapper.ServeHTTP(optionsRecorder, options)
	if optionsRecorder.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS should return 204, got %d", optionsRecorder.Code)
	}
}

func TestFitlabDegradesWhenUnavailable(t *testing.T) {
	testConfig(t, "http://127.0.0.1:65535")
	deps, _, _ := testDeps(t)
	wrapper := Wrap(&stubBase{}, deps).(*Wrapper)
	defer wrapper.Close()

	request := multipartRequest(t, "/api/v1/fitlab/tryon")
	request.Header.Set("Authorization", "Bearer user-token")
	recorder := httptest.NewRecorder()
	wrapper.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable fitlab should degrade to 503, got %d", recorder.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode degrade payload: %v", err)
	}
	if payload["code"] != "fitlab_unavailable" || payload["degraded"] != true {
		t.Fatalf("unexpected degrade payload: %v", payload)
	}
}

func TestFitlabDisabledBySwitch(t *testing.T) {
	testConfig(t, "http://127.0.0.1:65535")
	t.Setenv("EXT_FITLAB_ENABLED", "false")
	deps, _, _ := testDeps(t)
	wrapper := Wrap(&stubBase{}, deps).(*Wrapper)
	defer wrapper.Close()

	request := multipartRequest(t, "/api/v1/fitlab/tryon")
	request.Header.Set("Authorization", "Bearer user-token")
	recorder := httptest.NewRecorder()
	wrapper.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled fitlab should return 503, got %d", recorder.Code)
	}
	var payload map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &payload)
	if payload["code"] != "fitlab_disabled" {
		t.Fatalf("unexpected code: %v", payload["code"])
	}
}

func TestAdminOverviewAndEvalReports(t *testing.T) {
	server := newPNGServer(t)
	testConfig(t, server.URL)
	dir := t.TempDir()
	t.Setenv("EXT_EVAL_DIR", dir)
	runDir := filepath.Join(dir, "run-1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	report := map[string]any{
		"type":         "agent_e2e",
		"dataset":      "e2e",
		"generated_at": "2026-10-29T10:00:00Z",
		"total":        20,
		"evaluated":    20,
		"hits":         17,
		"pass_rate":    0.85,
	}
	raw, _ := json.Marshal(report)
	if err := os.WriteFile(filepath.Join(runDir, "agent_e2e.json"), raw, 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}

	deps, _, telemetry := testDeps(t)
	telemetry.events = []domain.ExtTelemetryEvent{
		{EventID: "1", EventType: "http_request", Path: "/api/v1/products", Status: 200, DurationMS: 12, AccountID: "u1", Source: "server"},
		{EventID: "2", EventType: "http_request", Path: "/api/v1/products", Status: 500, DurationMS: 30, Source: "server"},
		{EventID: "3", EventType: "client_exposure", Path: "/product/1", Status: 0, Source: "client"},
	}
	wrapper := Wrap(&stubBase{}, deps).(*Wrapper)
	defer wrapper.Close()

	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/ext/overview", nil)
	request.Header.Set("Authorization", "Bearer admin-token")
	recorder := httptest.NewRecorder()
	wrapper.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("admin overview status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Enabled   bool             `json:"enabled"`
		Fitlab    fitlabHealth     `json:"fitlab"`
		Telemetry telemetrySummary `json:"telemetry"`
		Eval      evalSummaryView  `json:"eval"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if !payload.Enabled || !payload.Fitlab.Available {
		t.Fatalf("overview should report enabled fitlab, got %+v", payload.Fitlab)
	}
	if payload.Telemetry.Events != 3 || payload.Telemetry.Errors != 1 || payload.Telemetry.ClientEvents != 1 {
		t.Fatalf("unexpected telemetry summary: %+v", payload.Telemetry)
	}
	if payload.Eval.Reports != 1 || len(payload.Eval.Latest) != 1 || payload.Eval.Latest[0].Hits != 17 {
		t.Fatalf("unexpected eval summary: %+v", payload.Eval)
	}

	list := httptest.NewRequest(http.MethodGet, "/api/v1/admin/ext/eval/reports", nil)
	list.Header.Set("Authorization", "Bearer admin-token")
	listRecorder := httptest.NewRecorder()
	wrapper.ServeHTTP(listRecorder, list)
	if listRecorder.Code != http.StatusOK || !strings.Contains(listRecorder.Body.String(), "run-1/agent_e2e") {
		t.Fatalf("eval report list failed: %d %s", listRecorder.Code, listRecorder.Body.String())
	}

	detail := httptest.NewRequest(http.MethodGet, "/api/v1/admin/ext/eval/reports/detail?id=run-1/agent_e2e", nil)
	detail.Header.Set("Authorization", "Bearer admin-token")
	detailRecorder := httptest.NewRecorder()
	wrapper.ServeHTTP(detailRecorder, detail)
	if detailRecorder.Code != http.StatusOK {
		t.Fatalf("eval report detail failed: %d %s", detailRecorder.Code, detailRecorder.Body.String())
	}
}

func TestTelemetryIngestAndRecorderFlush(t *testing.T) {
	server := newPNGServer(t)
	testConfig(t, server.URL)
	deps, _, telemetry := testDeps(t)
	wrapper := Wrap(&stubBase{}, deps).(*Wrapper)

	body := `{"events":[{"event_type":"client_exposure","path":"/products","status":200,"duration_ms":8,"metadata":{"product_id":"p1"}}]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/ext/telemetry/events", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	wrapper.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("ingest status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	wrapper.Close()
	waitForInsert(t, telemetry)
	telemetry.mu.Lock()
	events := append([]domain.ExtTelemetryEvent(nil), telemetry.events...)
	telemetry.mu.Unlock()
	found := false
	for _, event := range events {
		if event.Source == "client" && event.Path == "/products" && event.EventType == "client_exposure" {
			found = true
		}
	}
	if len(events) == 0 || !found {
		t.Fatalf("unexpected ingested events: %+v", events)
	}
}

func TestSummarizeTelemetryPercentile(t *testing.T) {
	summary := summarizeTelemetry(24, []domain.ExtTelemetryEvent{
		{Path: "/a", Status: 200, DurationMS: 10},
		{Path: "/a", Status: 200, DurationMS: 20},
		{Path: "/a", Status: 200, DurationMS: 30},
		{Path: "/a", Status: 200, DurationMS: 40},
		{Path: "/a", Status: 500, DurationMS: 50},
	})
	if summary.Events != 5 || summary.Errors != 1 || summary.P95MS != 40 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if len(summary.ByPath) != 1 || summary.ByPath[0].Count != 5 || summary.ByPath[0].Errors != 1 {
		t.Fatalf("unexpected path aggregation: %+v", summary.ByPath)
	}
}
