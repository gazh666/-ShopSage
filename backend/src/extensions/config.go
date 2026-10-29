package extensions

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是扩展能力的全部开关与参数。
//
// 所有扩展能力默认关闭（EXT_ENABLED=false），此时 extensions.Wrap 会原样返回
// 原始 http.Handler，项目行为与未接入扩展的版本完全一致。
type Config struct {
	Enabled          bool
	FitlabEnabled    bool
	FitlabBaseURLs   []string
	FitlabTimeout    time.Duration
	TelemetryEnabled bool
	TelemetryWindow  time.Duration
	TelemetryMaxBody int64
	EvalDir          string
}

func LoadConfig() Config {
	cfg := Config{
		Enabled:          envBool("EXT_ENABLED", false),
		FitlabEnabled:    envBool("EXT_FITLAB_ENABLED", true),
		FitlabBaseURLs:   splitList(env("FITLAB_BASE_URL", "http://fitlab:8000,http://127.0.0.1:8000")),
		FitlabTimeout:    time.Duration(envInt("FITLAB_TIMEOUT_MS", 8000)) * time.Millisecond,
		TelemetryEnabled: envBool("EXT_TELEMETRY_ENABLED", true),
		TelemetryWindow:  time.Duration(envInt("EXT_TELEMETRY_WINDOW_HOURS", 24)) * time.Hour,
		TelemetryMaxBody: int64(envInt("EXT_TELEMETRY_MAX_BODY_KB", 64)) * 1024,
		EvalDir:          strings.TrimSpace(os.Getenv("EXT_EVAL_DIR")),
	}
	if cfg.FitlabTimeout <= 0 {
		cfg.FitlabTimeout = 8 * time.Second
	}
	if cfg.TelemetryWindow <= 0 {
		cfg.TelemetryWindow = 24 * time.Hour
	}
	if cfg.TelemetryMaxBody <= 0 {
		cfg.TelemetryMaxBody = 64 * 1024
	}
	return cfg
}

func env(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envBool(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on", "enabled":
		return true
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return fallback
	}
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		items = append(items, strings.TrimRight(part, "/"))
	}
	return items
}
