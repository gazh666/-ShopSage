package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"
)

const (
	fitlabMaxFileBytes  = 6 << 20
	fitlabMaxResultSize = 12 << 20
	fitlabProbeTimeout  = 2 * time.Second
)

type fitlabHealth struct {
	Enabled   bool   `json:"enabled"`
	Available bool   `json:"available"`
	BaseURL   string `json:"base_url,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	Engine    string `json:"engine,omitempty"`
	Error     string `json:"error,omitempty"`
}

// probeFitlab 只做短超时健康探测，任何失败都返回 available=false 而不是报错退出。
func (e *Wrapper) probeFitlab(ctx context.Context) fitlabHealth {
	health := fitlabHealth{Enabled: e.cfg.FitlabEnabled}
	if !e.cfg.FitlabEnabled {
		health.Error = "fitlab_disabled"
		return health
	}
	timeout := fitlabProbeTimeout
	if e.cfg.FitlabTimeout < timeout {
		timeout = e.cfg.FitlabTimeout
	}
	for _, baseURL := range e.fitlabCandidates() {
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			cancel()
			continue
		}
		started := time.Now()
		response, err := e.client.Do(request)
		if err != nil {
			cancel()
			e.logger.Debug("fitlab health probe failed", "base_url", baseURL, "error", err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 256*1024))
		response.Body.Close()
		latency := time.Since(started).Milliseconds()
		cancel()
		if readErr != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
			health.Error = "fitlab_unavailable"
			continue
		}
		var payload struct {
			Status string `json:"status"`
			Engine string `json:"engine"`
		}
		_ = json.Unmarshal(body, &payload)
		if payload.Status != "" && payload.Status != "ok" {
			health.Error = "fitlab_unavailable"
			continue
		}
		health.Available = true
		health.BaseURL = baseURL
		health.LatencyMS = latency
		health.Engine = payload.Engine
		e.rememberFitlabURL(baseURL)
		return health
	}
	if health.Error == "" {
		health.Error = "fitlab_unavailable"
	}
	return health
}

func (e *Wrapper) handleFitlabHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeExtError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET")
		return
	}
	writeExtJSON(w, http.StatusOK, e.probeFitlab(r.Context()))
}

// handleFitlabTryon 是试衣能力的独立 HTTP 接入点。
// 任何试衣服务异常（未配置、超时、连接失败、返回非图片）都会降级为 503 JSON，
// 由前端继续回退到原有文字导购流程，不影响 Agent 主链路。
func (e *Wrapper) handleFitlabTryon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeExtError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST")
		return
	}
	if !e.cfg.FitlabEnabled {
		writeFitlabDegraded(w, r, "fitlab_disabled", "试衣服务未开启，已降级为文字导购")
		return
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeExtError(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "试衣图片过大")
			return
		}
		writeExtError(w, r, http.StatusBadRequest, "bad_request", "试衣请求需使用 multipart/form-data，并上传 person_image 与 garment_image")
		return
	}
	person, personHeader, err := readFitlabUpload(r, "person_image")
	if err != nil {
		writeExtError(w, r, http.StatusBadRequest, "bad_person_image", err.Error())
		return
	}
	garment, garmentHeader, err := readFitlabUpload(r, "garment_image")
	if err != nil {
		writeExtError(w, r, http.StatusBadRequest, "bad_garment_image", err.Error())
		return
	}
	category := trimLimit(r.FormValue("category"), 32)

	ctx, cancel := context.WithTimeout(r.Context(), e.cfg.FitlabTimeout)
	defer cancel()
	result, contentType, baseURL, err := e.forwardFitlabTryon(ctx, person, personHeader, garment, garmentHeader, category)
	if err != nil {
		e.logger.Warn("fitlab tryon degraded", "request_id", requestIDFromExtContext(r.Context()), "error", err)
		writeFitlabDegraded(w, r, "fitlab_unavailable", "试衣服务暂不可用，已降级为文字导购")
		return
	}
	e.rememberFitlabURL(baseURL)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Fitlab-Base", baseURL)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result)
}

func writeFitlabDegraded(w http.ResponseWriter, r *http.Request, code string, message string) {
	writeExtJSON(w, http.StatusServiceUnavailable, map[string]any{
		"code":       code,
		"message":    message,
		"degraded":   true,
		"request_id": requestIDFromExtContext(r.Context()),
	})
}

func readFitlabUpload(r *http.Request, field string) ([]byte, *multipart.FileHeader, error) {
	file, header, err := r.FormFile(field)
	if err != nil {
		return nil, nil, fmt.Errorf("缺少 %s 图片", field)
	}
	defer file.Close()
	if header.Size <= 0 {
		return nil, nil, fmt.Errorf("%s 图片为空", field)
	}
	if header.Size > fitlabMaxFileBytes {
		return nil, nil, fmt.Errorf("%s 图片超过 6MB 限制", field)
	}
	data, err := io.ReadAll(io.LimitReader(file, fitlabMaxFileBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("读取 %s 图片失败", field)
	}
	if len(data) > fitlabMaxFileBytes {
		return nil, nil, fmt.Errorf("%s 图片超过 6MB 限制", field)
	}
	if len(data) == 0 {
		return nil, nil, fmt.Errorf("%s 图片为空", field)
	}
	return data, header, nil
}

func (e *Wrapper) forwardFitlabTryon(ctx context.Context, person []byte, personHeader *multipart.FileHeader, garment []byte, garmentHeader *multipart.FileHeader, category string) ([]byte, string, string, error) {
	var lastErr error
	for _, baseURL := range e.fitlabCandidates() {
		request, err := buildFitlabRequest(ctx, baseURL+"/v1/tryon", person, personHeader, garment, garmentHeader, category)
		if err != nil {
			lastErr = err
			continue
		}
		response, err := e.client.Do(request)
		if err != nil {
			lastErr = err
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
			response.Body.Close()
			lastErr = fmt.Errorf("fitlab status %d: %s", response.StatusCode, sanitizeFitlabError(body))
			continue
		}
		contentType := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Type")))
		if !strings.HasPrefix(contentType, "image/") {
			response.Body.Close()
			lastErr = fmt.Errorf("fitlab returned non-image content type %q", contentType)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, fitlabMaxResultSize+1))
		response.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if len(body) > fitlabMaxResultSize {
			lastErr = fmt.Errorf("fitlab result exceeds limit")
			continue
		}
		if len(body) == 0 {
			lastErr = fmt.Errorf("fitlab result is empty")
			continue
		}
		return body, contentType, baseURL, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("fitlab is not configured")
	}
	return nil, "", "", lastErr
}

func buildFitlabRequest(ctx context.Context, endpoint string, person []byte, personHeader *multipart.FileHeader, garment []byte, garmentHeader *multipart.FileHeader, category string) (*http.Request, error) {
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	if category != "" {
		_ = writer.WriteField("category", category)
	}
	if err := writeFitlabPart(writer, "person_image", personHeader.Filename, personHeader.Header.Get("Content-Type"), person); err != nil {
		return nil, err
	}
	if err := writeFitlabPart(writer, "garment_image", garmentHeader.Filename, garmentHeader.Header.Get("Content-Type"), garment); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buffer)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Accept", "image/png, image/jpeg")
	return request, nil
}

func writeFitlabPart(writer *multipart.Writer, field string, filename string, contentType string, payload []byte) error {
	filename = filepath.Base(strings.TrimSpace(filename))
	if filename == "" || filename == "." {
		filename = field + ".png"
	}
	if contentType == "" {
		contentType = http.DetectContentType(payload)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, field, escapeFitlabFilename(filename)))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = part.Write(payload)
	return err
}

func escapeFitlabFilename(value string) string {
	return strings.NewReplacer("\\", "_", `"`, "_").Replace(value)
}

func sanitizeFitlabError(body []byte) string {
	value := strings.Join(strings.Fields(string(body)), " ")
	return trimLimit(value, 200)
}

func (e *Wrapper) rememberFitlabURL(baseURL string) {
	e.fitlabMu.Lock()
	e.favoredURL = baseURL
	e.fitlabMu.Unlock()
}

func (e *Wrapper) fitlabCandidates() []string {
	e.fitlabMu.RLock()
	favored := e.favoredURL
	e.fitlabMu.RUnlock()
	items := make([]string, 0, len(e.cfg.FitlabBaseURLs)+1)
	if favored != "" {
		items = append(items, favored)
	}
	for _, baseURL := range e.cfg.FitlabBaseURLs {
		if baseURL == favored {
			continue
		}
		items = append(items, baseURL)
	}
	return items
}
