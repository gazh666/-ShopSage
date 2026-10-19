package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"shopsage/backend/src/domain"
)

// shouldUseTaskPipeline 决定当前 turn 是否进入任务规划流水线。仅当：导购路由 + 配置开启 + 判定为复杂多步骤需求。
func (r *Runtime) shouldUseTaskPipeline(ctx context.Context, run domain.AgentRun, plan runPlan, query string) bool {
	if plan.Route != "guide" {
		return false
	}
	if !r.boolConfig(ctx, "agent.task_pipeline.enabled", true) {
		return false
	}
	return isComplexGuideQuery(query)
}

// isComplexGuideQuery 用轻量启发式判断“复杂多步骤导购请求”：
// 既是检索意向（找/搜/推荐/品类词），又带后续动作（对比/评价/加购/下单）。
func isComplexGuideQuery(query string) bool {
	query = strings.TrimSpace(query)
	if len([]rune(query)) < 8 {
		return false
	}
	searchSignals := []string{"找", "搜", "推荐", "买", "选", "面霜", "手机", "耳机", "电脑", "鞋", "衣服", "洗面奶", "精华", "口红", "护肤", "水乳"}
	actionSignals := []string{"对比", "比较", "评价", "口碑", "加入购物车", "加购", "下单", "结算", "哪个好", "哪款", "二选一", "最合适"}
	return containsAny(query, searchSignals) && containsAny(query, actionSignals)
}

// runTaskPipeline 是复杂导购任务流水线的编排入口：任务初始化/断点恢复 → Plan 生成子任务 →
// Verify & Execute 串行调度 → 结束标记与指标写入。返回 handled=true 表示本轮由流水线完成、无需再走单轮 Agent。
func (r *Runtime) runTaskPipeline(ctx context.Context, run domain.AgentRun, plan runPlan, query string, emit func(domain.SSEEvent) error) (bool, error) {
	startedAt := time.Now()

	// 1. 任务初始化 / 断点恢复：同一会话内存在未完成任务则续跑，否则新建。
	task, resumed := r.initOrResumeTask(ctx, run, query)
	if task.TaskID == "" {
		// 主任务创建失败，回退传统单轮 Agent，绝不影响既有链路。
		return false, nil
	}

	// 2. Plan 阶段：恢复时复用已持久化的子任务，否则调用 LLM 生成结构化子任务。
	var planned []plannedSubTask
	var subs []domain.AgentSubTask
	if resumed {
		subs = r.loadSubTasks(ctx, task.TaskID)
		subs = r.resetInterruptedSubTasks(ctx, task.TaskID, subs)
		planned = make([]plannedSubTask, len(subs))
	} else {
		prefs := r.buildUserPreferences(ctx, run, query)
		var err error
		planned, _, err = r.planSubTaskList(ctx, run, query, prefs)
		if err != nil {
			// 规划失败或输出格式错误：降级为传统单轮 Agent。
			r.setTaskStatus(ctx, task.TaskID, domain.AgentTaskStatusFailed, "规划失败："+err.Error())
			r.traceTaskEvent(ctx, run, task, "plan_failed", "failed", map[string]any{"error": err.Error()})
			r.logger.Warn("task pipeline plan degraded", "run_id", run.RunID, "error", err)
			return false, nil
		}
		subs = r.buildAgentSubTasks(ctx, task.TaskID, planned)
		if err := r.persistSubTasks(ctx, subs); err != nil {
			r.setTaskStatus(ctx, task.TaskID, domain.AgentTaskStatusFailed, "子任务持久化失败")
			r.logger.Error("persist sub tasks failed", "run_id", run.RunID, "error", err)
			return false, nil
		}
		if raw, err := json.Marshal(planned); err == nil {
			r.traceTaskPlan(ctx, run, task, string(raw), len(subs))
		}
	}

	r.setTaskStatus(ctx, task.TaskID, domain.AgentTaskStatusRunning, "")

	// 3. Verify & Execute：串行调度子任务，复用既有工具，结果双重校验 + 重试 + 降级。
	outcome := r.executeAgentSubTasks(ctx, run, task, planned, subs, emit)

	// 4. 结束标记与任务指标写入。
	if outcome.Success {
		r.setTaskStatus(ctx, task.TaskID, domain.AgentTaskStatusCompleted, "")
		r.traceTaskEvent(ctx, run, task, "completed", "ok", map[string]any{
			"retry_total":         outcome.RetryTotal,
			"subtask_total":       len(subs),
			"subtask_success_rate": taskSuccessRate(planned, subs, &outcome),
			"duration_ms":         time.Since(startedAt).Milliseconds(),
		})
		if err := r.emitTaskFinalAnswer(ctx, run, task, outcome.Workspace, emit); err != nil {
			return true, err
		}
	} else {
		r.setTaskStatus(ctx, task.TaskID, domain.AgentTaskStatusFailed, outcome.FailReason)
		r.traceTaskEvent(ctx, run, task, "degraded", "failed", map[string]any{
			"fail_reason":         outcome.FailReason,
			"retry_total":         outcome.RetryTotal,
			"subtask_total":       len(subs),
			"duration_ms":         time.Since(startedAt).Milliseconds(),
		})
		if err := r.emitTaskFailureAnswer(run, task, outcome, emit); err != nil {
			return true, err
		}
	}
	return true, nil
}

// initOrResumeTask 查询当前会话未完成任务；有则标记恢复，无则创建新任务。
func (r *Runtime) initOrResumeTask(ctx context.Context, run domain.AgentRun, query string) (domain.AgentTask, bool) {
	if existing, ok := r.findIncompleteTask(ctx, run); ok {
		r.resumeTask(ctx, existing.TaskID)
		existing.Resumed = true
		r.traceTaskEvent(ctx, run, existing, "resumed", "ok", nil)
		return existing, true
	}
	task, err := r.createAgentTask(ctx, run, query)
	if err != nil {
		r.logger.Error("create agent task failed", "run_id", run.RunID, "error", err)
		return domain.AgentTask{}, false
	}
	r.traceTaskEvent(ctx, run, task, "created", "ok", nil)
	return task, false
}

// resetInterruptedSubTasks 把断点处残留的 RUNNING 子任务复位为 PENDING，保证续跑时重新执行。
func (r *Runtime) resetInterruptedSubTasks(ctx context.Context, taskID string, subs []domain.AgentSubTask) []domain.AgentSubTask {
	for i := range subs {
		if subs[i].Status == domain.AgentSubTaskStatusRunning {
			subs[i].Status = domain.AgentSubTaskStatusPending
			_ = r.store.UpdateAgentSubTask(ctx, subs[i])
		}
	}
	return subs
}

// emitTaskFinalAnswer 依据执行结果输出最终回答，并附带商品/购物车结构化块。
func (r *Runtime) emitTaskFinalAnswer(ctx context.Context, run domain.AgentRun, task domain.AgentTask, ws *execWorkspace, emit func(domain.SSEEvent) error) error {
	if len(ws.Products) > 0 {
		ids := make([]string, 0, len(ws.Products))
		for _, p := range ws.Products {
			if strings.TrimSpace(p.ProductID) != "" {
				ids = append(ids, p.ProductID)
			}
		}
		if len(ids) > 0 {
			block := domain.AgentBlock{Type: "product_refs", ProductIDs: ids}
			if err := emit(domain.SSEEvent{Type: "block_delta", RunID: run.RunID, Block: &block}); err != nil {
				return err
			}
		}
	}
	if ws.AddedCart && ws.LastCart != nil {
		block := domain.AgentBlock{Type: "cart_state", Cart: ws.LastCart}
		if err := emit(domain.SSEEvent{Type: "block_delta", RunID: run.RunID, Block: &block}); err != nil {
			return err
		}
	}

	var b strings.Builder
	if len(ws.Products) > 0 {
		fmt.Fprintf(&b, "已按你的需求筛选出 %d 款候选商品", len(ws.Products))
		if ws.BestProduct != nil {
			fmt.Fprintf(&b, "，优先推荐 %s", ws.BestProduct.Name)
		}
		b.WriteString("。")
	}
	if ws.AddedCart {
		if ws.BestProduct != nil {
			fmt.Fprintf(&b, "已把最合适的商品（%s）加入购物车。", ws.BestProduct.Name)
		} else {
			b.WriteString("已把最合适的商品加入购物车。")
		}
	}
	if len(ws.Reviews) > 0 {
		b.WriteString("已结合当前可见评价完成对比。")
	}
	if b.Len() == 0 {
		b.WriteString("已完成本次导购任务，如有更具体的偏好可以继续告诉我。")
	}
	final := b.String()
	final += "\n\n> 本回答由 AI 生成，仅供参考，具体商品信息以商城页面为准。"
	return r.emitText(run, final, emit)
}

// emitTaskFailureAnswer 输出降级提示，引导用户放宽筛选条件。
func (r *Runtime) emitTaskFailureAnswer(run domain.AgentRun, task domain.AgentTask, outcome taskExecOutcome, emit func(domain.SSEEvent) error) error {
	text := fmt.Sprintf("很抱歉，本次导购任务未完成：%s。你可以尝试放宽筛选条件（例如提高预算、放宽肤质要求），我会重新规划并继续。\n\n> 本回答由 AI 生成，仅供参考。", outcome.FailReason)
	return r.emitText(run, text, emit)
}

func taskSuccessRate(planned []plannedSubTask, subs []domain.AgentSubTask, outcome *taskExecOutcome) float64 {
	if len(subs) == 0 {
		return 0
	}
	success := 0
	for i := range subs {
		if subs[i].Status == domain.AgentSubTaskStatusSuccess {
			success++
		}
	}
	return float64(success) / float64(len(subs))
}