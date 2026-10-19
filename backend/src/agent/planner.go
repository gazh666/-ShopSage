package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"shopsage/backend/src/configcenter"
	"shopsage/backend/src/domain"
)

// taskPlan 是 Plan-and-Solve 前置规划的结果。只有当复杂导购需求被判定需要多步时，
// Items 才非空；简单需求或规划失败时保持空，ReAct 循环照常工作，不影响既有链路。
type taskPlan struct {
	Enabled bool
	Items   []taskPlanItem
	Raw     string
}

type taskPlanItem struct {
	ID          string          `json:"id"`
	Description string          `json:"description"`
	Constraints taskConstraints `json:"constraints,omitempty"`
}

type taskConstraints struct {
	BudgetMax   string   `json:"budget_max,omitempty"`
	BudgetMin   string   `json:"budget_min,omitempty"`
	SkinType    string   `json:"skin_type,omitempty"`
	Categories  []string `json:"categories,omitempty"`
	Brands      []string `json:"brands,omitempty"`
	Negative    []string `json:"negative,omitempty"`
}

// planTasks 在 ReAct 循环之前调用，让模型先给出有序子任务。仅在 guide 路由下启用，
// 通过 agent.planning.enabled 配置开关。规划失败时静默降级为无规划，绝不影响回答产出。
func (r *Runtime) planTasks(ctx context.Context, run domain.AgentRun, plan runPlan, query string) taskPlan {
	result := taskPlan{}
	if plan.Route != "guide" || !r.boolConfig(ctx, "agent.planning.enabled", true) {
		return result
	}
	if !r.llm.Enabled() {
		return result
	}

	startedAt := time.Now()
	temperature := 0.1
	model := r.modelForRole(ctx, modelRoleTaskPlanning, r.llm.SmallModel())
	messages := []ChatMessage{
		{Role: "system", Content: r.stringConfig(ctx, "agent.prompt.task_planning", configcenter.DefaultTaskPlanningPrompt)},
		{Role: "user", Content: "用户问题：" + strings.TrimSpace(query) + "\n意图：" + plan.ReferenceIntent() + "\n只输出 JSON。"},
	}
	content, err := r.llm.Complete(ctx, model, messages, temperature)
	if err != nil {
		r.logger.Warn("task planning fallback", "run_id", run.RunID, "error", err)
		r.traceLLM(ctx, run, "planner.task_planning", model, startedAt, err, llmPromptMetadata(messages, temperature, nil))
		return result
	}
	r.traceLLM(ctx, run, "planner.task_planning", model, startedAt, nil, llmPromptMetadata(messages, temperature, map[string]any{"raw_length": len([]rune(content))}))

	var parsed struct {
		Tasks []taskPlanItem `json:"tasks"`
	}
	raw := extractJSONObject(content)
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil || len(parsed.Tasks) == 0 {
		var alt struct {
			Subtasks []taskPlanItem `json:"subtasks"`
		}
		if err2 := json.Unmarshal([]byte(raw), &alt); err2 == nil && len(alt.Subtasks) > 0 {
			parsed.Tasks = alt.Subtasks
		} else {
			r.logger.Warn("task planning json fallback", "run_id", run.RunID, "error", err, "content", content)
			return result
		}
	}
	items := parsed.Tasks[:0]
	for _, item := range parsed.Tasks {
		item.ID = strings.TrimSpace(item.ID)
		item.Description = strings.TrimSpace(item.Description)
		if item.ID != "" && item.Description != "" {
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return result
	}
	result.Enabled = true
	result.Items = items
	result.Raw = content
	return result
}

// promptForTaskPlan 把规划结果转成一段注入 ReAct 上下文的指令文本。
func promptForTaskPlan(plan taskPlan) string {
	if !plan.Enabled || len(plan.Items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("任务计划（按顺序执行，完成一步再进入下一步）：\n")
	for index, item := range plan.Items {
		b.WriteString(fmt.Sprintf("%d. %s", index+1, item.Description))
		b.WriteString(constraintsText(item.Constraints))
		b.WriteString("\n")
	}
	b.WriteString("执行要求：优先用 search_products 等工具逐步落实上述步骤；每步检索后核对返回商品是否满足对应约束（预算、品类、排除项、肤质），不满足就调整参数重新检索；信息足够且约束满足后再输出 final。")
	return b.String()
}

func constraintsText(c taskConstraints) string {
	parts := make([]string, 0, 6)
	if strings.TrimSpace(c.BudgetMax) != "" {
		parts = append(parts, "预算上限 "+c.BudgetMax)
	}
	if strings.TrimSpace(c.BudgetMin) != "" {
		parts = append(parts, "预算下限 "+c.BudgetMin)
	}
	if strings.TrimSpace(c.SkinType) != "" {
		parts = append(parts, "肤质 "+c.SkinType)
	}
	if len(c.Categories) > 0 {
		parts = append(parts, "品类 "+strings.Join(c.Categories, "/"))
	}
	if len(c.Brands) > 0 {
		parts = append(parts, "品牌 "+strings.Join(c.Brands, "/"))
	}
	if len(c.Negative) > 0 {
		parts = append(parts, "排除 "+strings.Join(c.Negative, "/"))
	}
	if len(parts) == 0 {
		return ""
	}
	return "（约束：" + strings.Join(parts, "；") + "）"
}

// validateTaskConstraints 校验一次商品检索结果是否满足规划中的硬约束。返回非空 note 表示未通过，
// 需要在观察回填里追加一条纠偏提示，让模型重新检索。
func validateTaskConstraints(plan taskPlan, observation toolObservation) string {
	if !plan.Enabled || len(plan.Items) == 0 || observation.Tool != toolSearchProducts {
		return ""
	}
	constraints := mergedPlanConstraints(plan)
	if constraints == nil {
		return ""
	}
	budgetMax := parsePrice(constraints.BudgetMax)
	items := observationItems(observation)
	if budgetMax > 0 && len(items) > 0 && constraints.hasBudgetMax() {
		for _, item := range items {
			if price := parsePrice(mapString(item, "price", "price_min", "price_max")); price > 0 && price <= budgetMax {
				return "" // 至少有一个候选在预算内
			}
		}
		return fmt.Sprintf("当前检索结果没有预算上限 %s 以内的商品", constraints.BudgetMax)
	}
	return ""
}

func mergedPlanConstraints(plan taskPlan) *taskConstraints {
	out := &taskConstraints{}
	hasAny := false
	for _, item := range plan.Items {
		c := item.Constraints
		if strings.TrimSpace(c.BudgetMax) != "" {
			out.BudgetMax = c.BudgetMax
			hasAny = true
		}
		if strings.TrimSpace(c.BudgetMin) != "" {
			out.BudgetMin = c.BudgetMin
			hasAny = true
		}
		if strings.TrimSpace(c.SkinType) != "" {
			out.SkinType = c.SkinType
			hasAny = true
		}
		if len(c.Categories) > 0 {
			out.Categories = appendUnique(out.Categories, c.Categories...)
			hasAny = true
		}
		if len(c.Brands) > 0 {
			out.Brands = appendUnique(out.Brands, c.Brands...)
			hasAny = true
		}
		if len(c.Negative) > 0 {
			out.Negative = appendUnique(out.Negative, c.Negative...)
			hasAny = true
		}
	}
	if !hasAny {
		return nil
	}
	return out
}

func (c taskConstraints) hasBudgetMax() bool {
	return strings.TrimSpace(c.BudgetMax) != ""
}

func observationItems(observation toolObservation) []map[string]any {
	if observation.Result == nil {
		return nil
	}
	switch rawItems := observation.Result["items"].(type) {
	case []map[string]any:
		return rawItems
	case []any:
		out := make([]map[string]any, 0, len(rawItems))
		for _, raw := range rawItems {
			if item, ok := raw.(map[string]any); ok {
				out = append(out, item)
			}
		}
		return out
	default:
		return nil
	}
}

// parsePrice 从类似 "2999.00"、"2999元"、"¥2999"、"2,999" 的字符串中提取数值，
// 用于预算约束校验。解析失败返回 0。
func parsePrice(text string) float64 {
	text = strings.TrimSpace(text)
	text = strings.ReplaceAll(text, ",", "")
	text = strings.ReplaceAll(text, "，", "")
	text = strings.TrimPrefix(text, "¥")
	text = strings.TrimPrefix(text, "￥")
	text = strings.TrimSuffix(text, "元")
	text = strings.TrimSuffix(text, "块")
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0
	}
	return value
}