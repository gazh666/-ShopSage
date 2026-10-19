package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"shopsage/backend/src/domain"
)

// taskVerifyOutcome 是 ResultVerifier 的一次校验结论。Passed 为 true 表示工具结果满足用户约束，
// 可以进入下一个子任务；否则 Reasons 里给出失败原因，供重试与降级提示使用。
type taskVerifyOutcome struct {
	Passed  bool
	Reasons []string
}

func (o taskVerifyOutcome) summary() string {
	if o.Passed {
		return "ok"
	}
	return strings.Join(o.Reasons, "；")
}

// verifySubTaskResult 对单个子任务的工具返回做“规则校验 + 轻量模型校验”双重校验。
// 规则校验覆盖：工具是否成功、检索是否有候选、预算范围是否满足。
// 轻量模型校验（可选，agent.task_pipeline.llm_verify）判断“敏感肌”等主观约束是否满足。
func (r *Runtime) verifySubTaskResult(ctx context.Context, run domain.AgentRun, task plannedSubTask, obs toolObservation) taskVerifyOutcome {
	outcome := taskVerifyOutcome{Passed: true}
	if !obs.OK {
		outcome.Passed = false
		outcome.Reasons = append(outcome.Reasons, firstNonEmpty(obs.Message, "工具执行失败"))
		return outcome
	}
	switch normalizeSubTaskType(task.Type) {
	case domain.SubTaskTypeProductSearch:
		items := observationItems(obs)
		if len(items) == 0 {
			outcome.Passed = false
			outcome.Reasons = append(outcome.Reasons, "检索结果为空，没有可挂出的候选商品")
			return outcome
		}
		if reason := verifyBudgetConstraint(task, items); reason != "" {
			outcome.Passed = false
			outcome.Reasons = append(outcome.Reasons, reason)
			return outcome
		}
		if r.boolConfig(ctx, "agent.task_pipeline.llm_verify", false) {
			if outcome := r.verifySubjectiveWithLLM(ctx, run, task, items); !outcome.Passed {
				return outcome
			}
		}
		return outcome
	default:
		return outcome
	}
}

// verifyBudgetConstraint 校验检索结果中是否至少存在一个预算上限内的候选商品。
func verifyBudgetConstraint(task plannedSubTask, items []map[string]any) string {
	budgetMax := parsePrice(task.BudgetMax)
	budgetMin := parsePrice(task.BudgetMin)
	if budgetMax <= 0 && budgetMin <= 0 {
		return ""
	}
	for _, item := range items {
		price := parsePrice(mapString(item, "price"))
		if price <= 0 {
			continue
		}
		if budgetMax > 0 && price > budgetMax {
			continue
		}
		if budgetMin > 0 && price < budgetMin {
			continue
		}
		return ""
	}
	if budgetMin > 0 {
		return fmt.Sprintf("检索结果均不在预算下限 %s 元以上", task.BudgetMin)
	}
	return fmt.Sprintf("检索结果均不在预算上限 %s 元以内", task.BudgetMax)
}

// verifySubjectiveWithLLM 用轻量模型二次校验主观约束（如敏感肌匹配），避免纯规则漏判。
func (r *Runtime) verifySubjectiveWithLLM(ctx context.Context, run domain.AgentRun, task plannedSubTask, items []map[string]any) taskVerifyOutcome {
	if strings.TrimSpace(task.SkinType) == "" {
		return taskVerifyOutcome{Passed: true}
	}
	model := r.modelForRole(ctx, modelRoleProductFilter, r.llm.SmallModel())
	temperature := 0.0
	messages := []ChatMessage{
		{Role: "system", Content: `你是电商导购商品的约束校验器。判断候选商品是否满足给定主观约束（如肤质）。
只输出 JSON：{"pass": true/false, "reason": "<一句话中文原因>"}。不确定时 pass=false 并说明原因。`},
		{Role: "user", Content: buildSubjectiveVerifyPrompt(task, items)},
	}
	startedAt := time.Now()
	content, err := r.llm.Complete(ctx, model, messages, temperature)
	if err != nil {
		r.traceLLM(ctx, run, "task.verify.llm", model, startedAt, err, llmPromptMetadata(messages, temperature, nil))
		// 轻量模型不可用时降级为规则通过，避免阻塞整条流水线。
		return taskVerifyOutcome{Passed: true}
	}
	r.traceLLM(ctx, run, "task.verify.llm", model, startedAt, nil, llmPromptMetadata(messages, temperature, map[string]any{"raw_length": len([]rune(content))}))

	var parsed struct {
		Pass   bool   `json:"pass"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(content)), &parsed); err != nil {
		return taskVerifyOutcome{Passed: true}
	}
	if parsed.Pass {
		return taskVerifyOutcome{Passed: true}
	}
	return taskVerifyOutcome{Passed: false, Reasons: []string{firstNonEmpty(parsed.Reason, "候选商品不满足肤质约束")}}
}

func buildSubjectiveVerifyPrompt(task plannedSubTask, items []map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "用户约束：肤质 %s\n候选商品：\n", task.SkinType)
	for i, item := range items {
		if i >= 6 {
			break
		}
		fmt.Fprintf(&b, "- 商品ID=%s 名称=%s 品牌=%s 标签=%s 卖点=%s\n",
			mapString(item, "product_id"), mapString(item, "name"), mapString(item, "brand"),
			mapString(item, "tags", "selling_points"), mapString(item, "selling_points"))
	}
	return b.String()
}