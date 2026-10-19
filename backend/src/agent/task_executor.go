package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"shopsage/backend/src/domain"
)

// taskProductRef 是执行期从工具返回里解析出的候选商品快照，供后续 review_query / add_cart 动态引用。
type taskProductRef struct {
	ProductID string `json:"product_id"`
	SkuID     string `json:"sku_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Price     string `json:"price,omitempty"`
}

// execWorkspace 是 TaskExecutor 在串行调度期间累积的上下文：最近一次检索候选、
// 可加购白名单、是否已加购等。它让“先检索→再评比较→最后加购”这类依赖关系无需再次调用模型。
type execWorkspace struct {
	Products     []taskProductRef    `json:"products,omitempty"`
	AllowedIDs   map[string]bool     `json:"-"`
	BestProduct  *taskProductRef     `json:"best_product,omitempty"`
	AddedCart    bool                `json:"added_cart"`
	Reviews      []map[string]any    `json:"reviews,omitempty"`
	LastCart     *domain.Cart        `json:"-"`
	SubTaskCount int                 `json:"subtask_count"`
}

func newExecWorkspace() *execWorkspace {
	return &execWorkspace{AllowedIDs: map[string]bool{}}
}

// taskExecOutcome 是整条子任务调度流水线的执行结果，用于判断主任务进入 COMPLETED 还是 FAILED（降级）。
type taskExecOutcome struct {
	Success    bool
	FailReason string
	RetryTotal int
	DurationMS int64
	Workspace  *execWorkspace
}

// executeAgentSubTasks 按依赖顺序串行执行子任务：无工具步骤直接成功；有工具步骤执行后做规则/模型双重校验，
// 失败按配置重试，仍失败则标记 FAILED 并触发上层降级。
func (r *Runtime) executeAgentSubTasks(ctx context.Context, run domain.AgentRun, task domain.AgentTask, planned []plannedSubTask, subs []domain.AgentSubTask, emit func(domain.SSEEvent) error) taskExecOutcome {
	startedAt := time.Now()
	outcome := taskExecOutcome{Success: true, Workspace: rebuildWorkspaceFromSubs(subs)}
	maxRetry := r.intConfig(ctx, "agent.task_pipeline.subtask_retry", 2)
	if maxRetry < 0 {
		maxRetry = 0
	}

	for i := range subs {
		sub := &subs[i]
		var constraints plannedSubTask
		if i < len(planned) {
			constraints = planned[i]
		}

		// 断点恢复：已成功的子任务跳过，从第一个未完成处继续。
		if sub.Status == domain.AgentSubTaskStatusSuccess {
			continue
		}

		if r.store.IsRunCanceled(ctx, run.RunID) {
			outcome.Success = false
			outcome.FailReason = "运行已取消"
			return outcome
		}
		if err := r.emitStatus(ctx, run.RunID, "tool", "正在执行："+sub.SubTaskName, emit); err != nil {
			outcome.Success = false
			outcome.FailReason = "流式输出中断"
			return outcome
		}

		// 无工具步骤（condition_extract / confirm_interact）为记录/确认环节，直接成功。
		if strings.TrimSpace(sub.ToolName) == "" {
			r.markSubTaskRunning(ctx, sub)
			r.completeNoToolSubTask(ctx, run, task, sub, constraints)
			continue
		}

		r.markSubTaskRunning(ctx, sub)
		if ok, reason := r.executeSubTaskWithRetry(ctx, run, task, sub, constraints, outcome.Workspace, maxRetry); !ok {
			r.markSubTaskFailed(ctx, sub, reason)
			outcome.Success = false
			outcome.FailReason = fmt.Sprintf("子任务「%s」执行失败：%s", sub.SubTaskName, reason)
			outcome.RetryTotal = sub.RetryCount
			outcome.DurationMS = time.Since(startedAt).Milliseconds()
			return outcome
		}
	}

	outcome.RetryTotal = sumSubTaskRetries(subs)
	outcome.Workspace.SubTaskCount = len(subs)
	outcome.DurationMS = time.Since(startedAt).Milliseconds()
	return outcome
}

// executeSubTaskWithRetry 执行单个有工具的子任务，带重试。成功更新workspace并持久化 SUCCESS。
func (r *Runtime) executeSubTaskWithRetry(ctx context.Context, run domain.AgentRun, task domain.AgentTask, sub *domain.AgentSubTask, constraints plannedSubTask, ws *execWorkspace, maxRetry int) (bool, string) {
	for attempt := 0; attempt <= maxRetry; attempt++ {
		sub.RetryCount = attempt
		rawArgs, resolveOK := r.resolveSubTaskArguments(sub, ws)
		if !resolveOK {
			// 无法解析前置依赖（如 review/add_cart 前没有候选商品），不迭带重试，直接失败。
			r.traceSubTask(ctx, run, task, sub, "resolve", "failed", 0, "缺少前置商品候选")
			return false, firstNonEmpty(sub.VerifyResult, "缺少前置商品候选，无法生成工具入参")
		}

		startedAt := time.Now()
		obs := r.executeTool(ctx, run, reactAction{Tool: sub.ToolName, Arguments: json.RawMessage([]byte(rawArgs))}, toolExecutionContext{
			AllowedAddProductIDs: ws.AllowedIDs,
		})
		duration := obs.DurationMS
		if duration <= 0 {
			duration = time.Since(startedAt).Milliseconds()
		}

		verdict := r.verifySubTaskResult(ctx, run, constraints, obs)
		r.traceSubTask(ctx, run, task, sub, obs.Tool, traceStatus(verdict.Passed), duration, verdict.summary())

		if verdict.Passed {
			r.applySubTaskResult(ctx, sub, obs, ws)
			return true, ""
		}

		sub.VerifyResult = verdict.summary()
		if attempt < maxRetry {
			r.traceSubTask(ctx, run, task, sub, "retry", "pending", duration, fmt.Sprintf("第 %d 次重试：%s", attempt+1, verdict.summary()))
		}
	}
	return false, firstNonEmpty(sub.VerifyResult, "重试后仍未通过校验")
}

// resolveSubTaskArguments 依据 workspace 动态补齐工具入参（product_id / sku_id），其余参数沿用规划期入参。
func (r *Runtime) resolveSubTaskArguments(sub *domain.AgentSubTask, ws *execWorkspace) (string, bool) {
	args := map[string]any{}
	if err := json.Unmarshal([]byte(sub.InputParams), &args); err != nil {
		args = map[string]any{}
	}
	switch sub.ToolName {
	case toolListReviews, toolAddCartItem:
		if ws.BestProduct == nil || strings.TrimSpace(ws.BestProduct.ProductID) == "" {
			return "", false
		}
		args["product_id"] = ws.BestProduct.ProductID
		if sub.ToolName == toolAddCartItem && strings.TrimSpace(ws.BestProduct.SkuID) != "" {
			args["sku_id"] = ws.BestProduct.SkuID
		}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return sub.InputParams, true
	}
	return string(raw), true
}

// applySubTaskResult 把成功结果写入 workspace 并持久化子任务 SUCCESS 输出。
func (r *Runtime) applySubTaskResult(ctx context.Context, sub *domain.AgentSubTask, obs toolObservation, ws *execWorkspace) {
	now := time.Now()
	switch sub.ToolName {
	case toolSearchProducts:
		refs := searchProductsFromObservation(obs)
		ws.Products = refs
		if len(refs) > 0 {
			best := refs[0]
			ws.BestProduct = &best
		}
		for _, id := range obs.ProductIDs {
			if strings.TrimSpace(id) != "" {
				ws.AllowedIDs[id] = true
			}
		}
		sub.OutputResult = stringifySearchObservation(obs)
	case toolListReviews:
		ws.Reviews = observationItems(obs)
		sub.OutputResult = jsonCompact(obs.Result)
	case toolAddCartItem:
		ws.AddedCart = true
		ws.LastCart = obs.Cart
		sub.OutputResult = jsonCompact(obs.Result)
	case toolCheckout:
		sub.OutputResult = jsonCompact(obs.Result)
	default:
		sub.OutputResult = jsonCompact(obs.Result)
	}
	sub.Status = domain.AgentSubTaskStatusSuccess
	sub.VerifyResult = "ok"
	sub.FinishedAt = &now
	_ = r.store.UpdateAgentSubTask(ctx, *sub)
}

// completeNoToolSubTask 处理无工具的 condition_extract / confirm_interact 子任务，直接置成功并写入输出。
func (r *Runtime) completeNoToolSubTask(ctx context.Context, run domain.AgentRun, task domain.AgentTask, sub *domain.AgentSubTask, constraints plannedSubTask) {
	now := time.Now()
	switch normalizeSubTaskType(sub.SubTaskType) {
	case domain.SubTaskTypeConditionExtract:
		sub.OutputResult = jsonCompact(taskConstraints{
			BudgetMax:  constraints.BudgetMax,
			BudgetMin:  constraints.BudgetMin,
			SkinType:   constraints.SkinType,
			Categories: constraints.Categories,
			Brands:      constraints.Brands,
			Negative:    constraints.Negative,
		})
	case domain.SubTaskTypeConfirmInteract:
		sub.OutputResult = `{"confirmed": true, "note": "自动确认后继续执行"}`
	}
	sub.Status = domain.AgentSubTaskStatusSuccess
	sub.VerifyResult = "ok"
	sub.FinishedAt = &now
	_ = r.store.UpdateAgentSubTask(ctx, *sub)
	r.traceSubTask(ctx, run, task, sub, sub.ToolName, "ok", 0, sub.SubTaskName)
}

func (r *Runtime) markSubTaskRunning(ctx context.Context, sub *domain.AgentSubTask) {
	sub.Status = domain.AgentSubTaskStatusRunning
	_ = r.store.UpdateAgentSubTask(ctx, *sub)
}

func (r *Runtime) markSubTaskFailed(ctx context.Context, sub *domain.AgentSubTask, reason string) {
	now := time.Now()
	sub.Status = domain.AgentSubTaskStatusFailed
	sub.VerifyResult = reason
	sub.FinishedAt = &now
	_ = r.store.UpdateAgentSubTask(ctx, *sub)
}

func searchProductsFromObservation(obs toolObservation) []taskProductRef {
	items := observationItems(obs)
	refs := make([]taskProductRef, 0, len(items))
	for _, item := range items {
		refs = append(refs, taskProductRef{
			ProductID: mapString(item, "product_id"),
			SkuID:     mapString(item, "sku_id"),
			Name:      mapString(item, "name"),
			Price:     mapString(item, "price"),
		})
	}
	return refs
}

func stringifySearchObservation(obs toolObservation) string {
	items := observationItems(obs)
	out := make([]map[string]string, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]string{
			"product_id": mapString(item, "product_id"),
			"sku_id":     mapString(item, "sku_id"),
			"name":       mapString(item, "name"),
			"brand":      mapString(item, "brand"),
			"price":      mapString(item, "price"),
		})
	}
	return jsonCompact(out)
}

func jsonCompact(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func sumSubTaskRetries(subs []domain.AgentSubTask) int {
	total := 0
	for i := range subs {
		total += subs[i].RetryCount
	}
	return total
}

// rebuildWorkspaceFromSubs 从已 SUCCESS 的串行子任务输出中重建执行上下文，支撑断点续跑时
// 「先检索→再评比较→最后加购」的依赖恢复，避免重复检索或丢失可加购白名单。
func rebuildWorkspaceFromSubs(subs []domain.AgentSubTask) *execWorkspace {
	ws := newExecWorkspace()
	for i := range subs {
		sub := subs[i]
		if sub.Status != domain.AgentSubTaskStatusSuccess {
			break
		}
		switch sub.ToolName {
		case toolSearchProducts:
			var refs []taskProductRef
			if err := json.Unmarshal([]byte(sub.OutputResult), &refs); err == nil && len(refs) > 0 {
				ws.Products = refs
				best := refs[0]
				ws.BestProduct = &best
				for _, ref := range refs {
					if strings.TrimSpace(ref.ProductID) != "" {
						ws.AllowedIDs[ref.ProductID] = true
					}
				}
			}
		case toolAddCartItem:
			ws.AddedCart = true
		}
	}
	return ws
}