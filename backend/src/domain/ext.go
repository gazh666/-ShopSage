package domain

import "time"

// ExtTelemetryEvent 是扩展埋点事件。
//
// 该结构只服务于 extensions 扩展包与 ext_telemetry_events 表，
// 不参与原有 Agent 推理、工具调用与电商业务流程。
type ExtTelemetryEvent struct {
	EventID      string    `json:"event_id"`
	EventType    string    `json:"event_type"`
	Path         string    `json:"path"`
	Method       string    `json:"method"`
	Status       int       `json:"status"`
	DurationMS   int64     `json:"duration_ms"`
	AccountID    string    `json:"account_id,omitempty"`
	Role         string    `json:"role,omitempty"`
	Source       string    `json:"source"`
	MetadataJSON string    `json:"metadata_json,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}
