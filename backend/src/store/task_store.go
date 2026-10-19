package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"shopsage/backend/src/domain"
)

// 下面是任务规划流水线的持久化实现。主任务 agent_task 和子任务 agent_sub_task
// 都按行存储，状态字段用字符串枚举，文本字段用 TEXT 承载 JSON，方便管理员后台展示。

func (s *MySQLStore) CreateAgentTask(ctx context.Context, task domain.AgentTask) (domain.AgentTask, error) {
	if strings.TrimSpace(task.TaskID) == "" {
		task.TaskID = nextID("task")
	}
	task.CreateTime = time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_task (
			task_id, user_id, conversation_id, run_id, query, task_status,
			plan_content, resumed, fail_reason, create_time, finish_time
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, task.TaskID, task.UserID, task.ConversationID, task.RunID, task.Query,
		string(task.TaskStatus), emptyFallback(task.PlanContent, ""), task.Resumed, task.FailReason,
		task.CreateTime, nullableTime(task.FinishTime))
	if err != nil {
		return domain.AgentTask{}, err
	}
	return task, nil
}

func (s *MySQLStore) GetAgentTask(ctx context.Context, taskID string) (domain.AgentTask, bool) {
	rows, err := s.db.QueryContext(ctx, taskSelectSQL+` WHERE task_id = ?`, taskID)
	if err != nil {
		return domain.AgentTask{}, false
	}
	defer rows.Close()
	items := scanAgentTasks(rows)
	if len(items) == 0 {
		return domain.AgentTask{}, false
	}
	return items[0], true
}

func (s *MySQLStore) GetIncompleteTaskByConversation(ctx context.Context, accountID string, conversationID string) (domain.AgentTask, bool) {
	rows, err := s.db.QueryContext(ctx, taskSelectSQL+`
		WHERE conversation_id = ? AND user_id = ?
		  AND task_status IN ('INIT','PLANNING','RUNNING','PAUSED')
		ORDER BY create_time DESC, task_id DESC
		LIMIT 1
	`, conversationID, accountID)
	if err != nil {
		return domain.AgentTask{}, false
	}
	defer rows.Close()
	items := scanAgentTasks(rows)
	if len(items) == 0 {
		return domain.AgentTask{}, false
	}
	return items[0], true
}

func (s *MySQLStore) UpdateAgentTaskStatus(ctx context.Context, taskID string, status domain.AgentTaskStatus, failReason string) bool {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_task
		SET task_status = ?, fail_reason = ?, finish_time = ?
		WHERE task_id = ?
	`, string(status), failReason, terminalTaskTime(status), taskID)
	return err == nil
}

func (s *MySQLStore) ResumeAgentTask(ctx context.Context, taskID string) bool {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_task
		SET task_status = 'RUNNING', resumed = TRUE, fail_reason = '', finish_time = NULL
		WHERE task_id = ?
	`, taskID)
	return err == nil
}

func (s *MySQLStore) ListAgentTasksPage(ctx context.Context, page int, pageSize int) ([]domain.AgentTask, int) {
	page, pageSize = normalizePage(page, pageSize)
	total := s.countRows(ctx, "agent_task")
	rows, err := s.db.QueryContext(ctx, taskSelectSQL+`
		ORDER BY create_time DESC, task_id DESC
		LIMIT ? OFFSET ?
	`, pageSize, pageOffset(page, pageSize))
	if err != nil {
		return nil, 0
	}
	defer rows.Close()
	return scanAgentTasks(rows), total
}

func (s *MySQLStore) ListAgentTasksSince(ctx context.Context, since time.Time, limit int) []domain.AgentTask {
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	rows, err := s.db.QueryContext(ctx, taskSelectSQL+`
		WHERE create_time >= ?
		ORDER BY create_time DESC, task_id DESC
		LIMIT ?
	`, since, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanAgentTasks(rows)
}

func (s *MySQLStore) CreateAgentSubTasks(ctx context.Context, subs []domain.AgentSubTask) error {
	if len(subs) == 0 {
		return nil
	}
	for _, sub := range subs {
		if strings.TrimSpace(sub.SubTaskID) == "" {
			sub.SubTaskID = nextID("subtask")
		}
		if sub.Status == "" {
			sub.Status = domain.AgentSubTaskStatusPending
		}
		if sub.CreatedAt.IsZero() {
			sub.CreatedAt = time.Now()
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO agent_sub_task (
				sub_task_id, task_id, sub_task_name, sub_task_type, status,
				depend_sub_task_id, tool_name, input_params, output_result,
				verify_result, retry_count, created_at, finished_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, sub.SubTaskID, sub.TaskID, sub.SubTaskName, sub.SubTaskType, string(sub.Status),
			sub.DependSubTaskID, sub.ToolName, emptyFallback(sub.InputParams, ""), emptyFallback(sub.OutputResult, ""),
			emptyFallback(sub.VerifyResult, ""), sub.RetryCount, sub.CreatedAt, nullableTime(sub.FinishedAt)); err != nil {
			return err
		}
	}
	return nil
}

func (s *MySQLStore) ListAgentSubTasks(ctx context.Context, taskID string) []domain.AgentSubTask {
	rows, err := s.db.QueryContext(ctx, subTaskSelectSQL+`
		WHERE task_id = ?
		ORDER BY created_at, sub_task_id
	`, taskID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanAgentSubTasks(rows)
}

func (s *MySQLStore) ListAgentSubTasksSince(ctx context.Context, since time.Time, limit int) []domain.AgentSubTask {
	if limit <= 0 || limit > 20000 {
		limit = 20000
	}
	rows, err := s.db.QueryContext(ctx, subTaskSelectSQL+`
		WHERE created_at >= ?
		ORDER BY created_at DESC, sub_task_id DESC
		LIMIT ?
	`, since, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanAgentSubTasks(rows)
}

func (s *MySQLStore) UpdateAgentSubTask(ctx context.Context, sub domain.AgentSubTask) bool {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_sub_task
		SET status = ?, tool_name = ?, input_params = ?, output_result = ?,
			verify_result = ?, retry_count = ?, finished_at = ?
		WHERE sub_task_id = ? AND task_id = ?
	`, string(sub.Status), sub.ToolName, emptyFallback(sub.InputParams, ""), emptyFallback(sub.OutputResult, ""),
		emptyFallback(sub.VerifyResult, ""), sub.RetryCount, nullableTime(sub.FinishedAt), sub.SubTaskID, sub.TaskID)
	return err == nil
}

const taskSelectSQL = `
	SELECT task_id, user_id, conversation_id, run_id, query, task_status,
		COALESCE(plan_content, ''), resumed, COALESCE(fail_reason, ''),
		create_time, finish_time
	FROM agent_task`

const subTaskSelectSQL = `
	SELECT sub_task_id, task_id, sub_task_name, sub_task_type, status,
		depend_sub_task_id, tool_name, COALESCE(input_params, ''),
		COALESCE(output_result, ''), COALESCE(verify_result, ''),
		retry_count, created_at, finished_at
	FROM agent_sub_task`

func scanAgentTasks(rows *sql.Rows) []domain.AgentTask {
	items := make([]domain.AgentTask, 0)
	for rows.Next() {
		var item domain.AgentTask
		var status string
		var finish sql.NullTime
		if err := rows.Scan(
			&item.TaskID, &item.UserID, &item.ConversationID, &item.RunID, &item.Query, &status,
			&item.PlanContent, &item.Resumed, &item.FailReason, &item.CreateTime, &finish,
		); err != nil {
			return nil
		}
		item.TaskStatus = domain.AgentTaskStatus(status)
		if finish.Valid {
			item.FinishTime = &finish.Time
		}
		items = append(items, item)
	}
	return items
}

func scanAgentSubTasks(rows *sql.Rows) []domain.AgentSubTask {
	items := make([]domain.AgentSubTask, 0)
	for rows.Next() {
		var item domain.AgentSubTask
		var status string
		var finish sql.NullTime
		if err := rows.Scan(
			&item.SubTaskID, &item.TaskID, &item.SubTaskName, &item.SubTaskType, &status,
			&item.DependSubTaskID, &item.ToolName, &item.InputParams, &item.OutputResult,
			&item.VerifyResult, &item.RetryCount, &item.CreatedAt, &finish,
		); err != nil {
			return nil
		}
		item.Status = domain.AgentSubTaskStatus(status)
		if finish.Valid {
			item.FinishedAt = &finish.Time
		}
		items = append(items, item)
	}
	return items
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}

// terminalTaskTime 在任务进入终态时写入完成时间，其余状态保持空。
func terminalTaskTime(status domain.AgentTaskStatus) any {
	switch status {
	case domain.AgentTaskStatusCompleted, domain.AgentTaskStatusFailed:
		return time.Now()
	default:
		return nil
	}
}