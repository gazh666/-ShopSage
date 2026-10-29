package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"shopsage/backend/src/domain"
)

// extTelemetrySchema 是扩展埋点表结构。
//
// 该表只由扩展包在启用时惰性创建，不进入 001_mysql_schema.sql，
// 从而保证关闭扩展开关时项目行为与原始版本完全一致。
const extTelemetrySchema = `
CREATE TABLE IF NOT EXISTS ext_telemetry_events (
  event_id VARCHAR(64) NOT NULL PRIMARY KEY,
  event_type VARCHAR(64) NOT NULL DEFAULT '',
  path VARCHAR(255) NOT NULL DEFAULT '',
  method VARCHAR(16) NOT NULL DEFAULT '',
  status INT NOT NULL DEFAULT 0,
  duration_ms BIGINT NOT NULL DEFAULT 0,
  account_id VARCHAR(64) NOT NULL DEFAULT '',
  role VARCHAR(32) NOT NULL DEFAULT '',
  source VARCHAR(32) NOT NULL DEFAULT 'server',
  metadata_json TEXT,
  created_at DATETIME NOT NULL,
  KEY idx_ext_telemetry_created (created_at),
  KEY idx_ext_telemetry_path (path)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
`

var (
	extTelemetryReadyMu sync.Mutex
	extTelemetryReady   bool
)

// EnsureExtTelemetryTable 惰性创建扩展埋点表。
// 创建失败时返回错误，由扩展包降级为只记录日志、不写库。
func (s *MySQLStore) EnsureExtTelemetryTable(ctx context.Context) error {
	extTelemetryReadyMu.Lock()
	defer extTelemetryReadyMu.Unlock()
	if extTelemetryReady {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, extTelemetrySchema); err != nil {
		return fmt.Errorf("ensure ext telemetry table: %w", err)
	}
	extTelemetryReady = true
	return nil
}

// InsertExtTelemetryEvents 批量写入扩展埋点事件。
func (s *MySQLStore) InsertExtTelemetryEvents(ctx context.Context, events []domain.ExtTelemetryEvent) error {
	if len(events) == 0 {
		return nil
	}
	if err := s.EnsureExtTelemetryTable(ctx); err != nil {
		return err
	}
	placeholders := make([]string, 0, len(events))
	args := make([]any, 0, len(events)*11)
	for _, event := range events {
		placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		createdAt := event.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now()
		}
		args = append(args,
			event.EventID,
			event.EventType,
			event.Path,
			event.Method,
			event.Status,
			event.DurationMS,
			event.AccountID,
			event.Role,
			event.Source,
			event.MetadataJSON,
			createdAt,
		)
	}
	query := `INSERT INTO ext_telemetry_events
		(event_id, event_type, path, method, status, duration_ms, account_id, role, source, metadata_json, created_at)
		VALUES ` + strings.Join(placeholders, ",")
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("insert ext telemetry events: %w", err)
	}
	return nil
}

// ListExtTelemetryEvents 按时间窗口倒序读取扩展埋点事件。
func (s *MySQLStore) ListExtTelemetryEvents(ctx context.Context, since time.Time, limit int) ([]domain.ExtTelemetryEvent, error) {
	if limit <= 0 || limit > 20000 {
		limit = 20000
	}
	if err := s.EnsureExtTelemetryTable(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id, event_type, path, method, status, duration_ms, account_id, role, source, metadata_json, created_at
		FROM ext_telemetry_events
		WHERE created_at >= ?
		ORDER BY created_at DESC
		LIMIT ?
	`, since, limit)
	if err != nil {
		return nil, fmt.Errorf("list ext telemetry events: %w", err)
	}
	defer rows.Close()

	items := make([]domain.ExtTelemetryEvent, 0)
	for rows.Next() {
		var event domain.ExtTelemetryEvent
		var metadata string
		if err := rows.Scan(
			&event.EventID,
			&event.EventType,
			&event.Path,
			&event.Method,
			&event.Status,
			&event.DurationMS,
			&event.AccountID,
			&event.Role,
			&event.Source,
			&metadata,
			&event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan ext telemetry event: %w", err)
		}
		event.MetadataJSON = metadata
		items = append(items, event)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return nil, fmt.Errorf("iterate ext telemetry events: %w", err)
	}
	return items, nil
}

// DeleteExtTelemetryBefore 清理过期埋点，供运维按需调用（不会被自动触发）。
func (s *MySQLStore) DeleteExtTelemetryBefore(ctx context.Context, before time.Time) (int64, error) {
	if err := s.EnsureExtTelemetryTable(ctx); err != nil {
		return 0, err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM ext_telemetry_events WHERE created_at < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("delete ext telemetry events: %w", err)
	}
	return result.RowsAffected()
}
