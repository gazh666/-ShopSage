package agent

import (
	"context"

	"shopsage/backend/src/domain"
)

// 本文件是追踪适配层（TraceAdapter）：把任务计划、子任务执行日志接入原有 agent_trace_events 链路，
// 供管理员后台链路追踪页面新增的“任务计划可视化区块”读取。所有事件都写入 task_id，后续日志可关联到本次任务。

// traceTaskEvent 记录一次主任务级事件（创建/规划/恢复/完成/降级）。
func (r *Runtime) traceTaskEvent(ctx context.Context, run domain.AgentRun, task domain.AgentTask, eventType string, status string, metadata map[string]any) {
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["task_id"] = task.TaskID
	metadata["conversation_id"] = task.ConversationID
	r.trace(ctx, run, "task", eventType, "", status, 0, "", metadata)
}

// traceTaskPlan 记录任务计划全量内容，供后台可视化展示“本次导购任务的完整计划”。
func (r *Runtime) traceTaskPlan(ctx context.Context, run domain.AgentRun, task domain.AgentTask, planJSON string, subtaskCount int) {
	r.traceTaskEvent(ctx, run, task, "plan", "ok", map[string]any{
		"plan":          planJSON,
		"subtask_count": subtaskCount,
	})
}

// traceSubTask 记录单个子任务的执行/校验/重试结果，作为链路追踪页面里每一步的状态、耗时、校验结果与工具摘要。
func (r *Runtime) traceSubTask(ctx context.Context, run domain.AgentRun, task domain.AgentTask, sub *domain.AgentSubTask, tool string, status string, durationMS int64, detail string) {
	metadata := map[string]any{
		"task_id":       task.TaskID,
		"sub_task_id":   sub.SubTaskID,
		"sub_task_name": sub.SubTaskName,
		"sub_task_type": sub.SubTaskType,
		"tool":          tool,
		"retry_count":   sub.RetryCount,
		"verify_result": sub.VerifyResult,
	}
	if detail != "" {
		metadata["detail"] = detail
	}
	r.trace(ctx, run, "task.subtask", "execute", "", status, durationMS, "", metadata)
}