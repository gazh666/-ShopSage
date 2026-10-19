package agent

import (
	"context"
	"strings"

	"shopsage/backend/src/domain"
)

// 本文件封装任务状态管理（TaskStateManager）：面对 agent_task / agent_sub_task 两张表的
// 创建、状态流转、断点恢复查询。它是对 store 接口的薄封装，让任务流水线无需直接接触 SQL 细节。

// createAgentTask 创建一条主任务记录（初始状态 INIT），并绑定用户、会话与原始 query。
func (r *Runtime) createAgentTask(ctx context.Context, run domain.AgentRun, query string) (domain.AgentTask, error) {
	task := domain.AgentTask{
		UserID:         run.AccountID,
		ConversationID: run.SessionID,
		RunID:          run.RunID,
		Query:          query,
		TaskStatus:     domain.AgentTaskStatusInit,
	}
	return r.store.CreateAgentTask(ctx, task)
}

// findIncompleteTask 通过会话 ID 查询未完成的主任务，用于“中断恢复”。
func (r *Runtime) findIncompleteTask(ctx context.Context, run domain.AgentRun) (domain.AgentTask, bool) {
	if strings.TrimSpace(run.AccountID) == "" || strings.TrimSpace(run.SessionID) == "" {
		return domain.AgentTask{}, false
	}
	return r.store.GetIncompleteTaskByConversation(ctx, run.AccountID, run.SessionID)
}

// setTaskStatus 更新主任务状态；进入终态（COMPLETED/FAILED）时自动写入完成时间。
func (r *Runtime) setTaskStatus(ctx context.Context, taskID string, status domain.AgentTaskStatus, failReason string) bool {
	return r.store.UpdateAgentTaskStatus(ctx, taskID, status, failReason)
}

// resumeTask 把已中断的任务置为 RUNNING 并标记为恢复执行，供断点续跑。
func (r *Runtime) resumeTask(ctx context.Context, taskID string) bool {
	return r.store.ResumeAgentTask(ctx, taskID)
}

// persistSubTasks 持久化规划生成的子任务（初始状态 PENDING）。
func (r *Runtime) persistSubTasks(ctx context.Context, subs []domain.AgentSubTask) error {
	return r.store.CreateAgentSubTasks(ctx, subs)
}

// loadSubTasks 按创建顺序读取子任务，作为断点续跑与可视化展示的数据源。
func (r *Runtime) loadSubTasks(ctx context.Context, taskID string) []domain.AgentSubTask {
	return r.store.ListAgentSubTasks(ctx, taskID)
}