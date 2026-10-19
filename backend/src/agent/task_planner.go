package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"shopsage/backend/src/configcenter"
	"shopsage/backend/src/domain"
)

// plannedSubTask 是 TaskPlanner（LLM）输出的结构化子任务，对应 subtask_planning prompt 的 JSON schema。
// 它只负责表达“计划”，与 domain.AgentSubTask（数据库持久化对象）分离：后者多了 tool_name、input_params、依赖等可执行信息。
type plannedSubTask struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Query       string   `json:"query,omitempty"`
	BudgetMax   string   `json:"budget_max,omitempty"`
	BudgetMin   string   `json:"budget_min,omitempty"`
	SkinType    string   `json:"skin_type,omitempty"`
	Categories  []string `json:"categories,omitempty"`
	Brands      []string `json:"brands,omitempty"`
	Negative    []string `json:"negative,omitempty"`
}

// planSubTaskList 调用 LLM 生成结构化子任务流水线，并做类型白名单归一化与去空。
// 返回的列表是有序的（前一步结果是后一步输入）；规划失败时返回错误，由上层降级为传统单轮 Agent。
func (r *Runtime) planSubTaskList(ctx context.Context, run domain.AgentRun, query string, prefs domain.UserPreferences) ([]plannedSubTask, string, error) {
	model := r.modelForRole(ctx, modelRoleTaskPlanning, r.llm.SmallModel())
	temperature := 0.1
	messages := []ChatMessage{
		{Role: "system", Content: r.stringConfig(ctx, "agent.prompt.subtask_planning", configcenter.DefaultSubTaskPlanningPrompt)},
		{Role: "user", Content: buildSubTaskPlanningUserPrompt(query, prefs)},
	}
	startedAt := time.Now()
	content, err := r.llm.Complete(ctx, model, messages, temperature)
	if err != nil {
		r.traceLLM(ctx, run, "task.planner.subtask_planning", model, startedAt, err, llmPromptMetadata(messages, temperature, nil))
		return nil, "", err
	}
	r.traceLLM(ctx, run, "task.planner.subtask_planning", model, startedAt, nil, llmPromptMetadata(messages, temperature, map[string]any{"raw_length": len([]rune(content))}))

	var parsed struct {
		Tasks []plannedSubTask `json:"tasks"`
	}
	raw := extractJSONObject(content)
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil || len(parsed.Tasks) == 0 {
		var alt struct {
			Subtasks []plannedSubTask `json:"subtasks"`
		}
		if err2 := json.Unmarshal([]byte(raw), &alt); err2 == nil && len(alt.Subtasks) > 0 {
			parsed.Tasks = alt.Subtasks
		} else {
			if err == nil {
				err = fmt.Errorf("empty subtask plan")
			}
			return nil, content, err
		}
	}

	tasks := make([]plannedSubTask, 0, len(parsed.Tasks))
	for _, task := range parsed.Tasks {
		task.Type = normalizeSubTaskType(task.Type)
		task.Description = strings.TrimSpace(task.Description)
		task.Query = strings.TrimSpace(task.Query)
		if task.Type == "" || task.Description == "" {
			continue
		}
		tasks = append(tasks, task)
	}
	if len(tasks) == 0 {
		return nil, content, fmt.Errorf("empty subtask plan after normalization")
	}
	return tasks, content, nil
}

// buildAgentSubTasks 把规划结果转为可持久化的子任务对象，补齐工具名、入参和串行依赖关系。
// 子任务 ID 在此预生成，便于写入 depend_sub_task_id（串行依赖：第 N 步依赖第 N-1 步）。
func (r *Runtime) buildAgentSubTasks(ctx context.Context, taskID string, tasks []plannedSubTask) []domain.AgentSubTask {
	maxSubTasks := r.intConfig(ctx, "agent.task_pipeline.max_subtasks", 6)
	if maxSubTasks <= 0 {
		maxSubTasks = 6
	}
	subs := make([]domain.AgentSubTask, 0, len(tasks))
	now := time.Now()
	var previousID string
	for i, task := range tasks {
		if len(subs) >= maxSubTasks {
			break
		}
		toolName, inputParams := subtaskToolSpec(task)
		sub := domain.AgentSubTask{
			SubTaskID:       fmt.Sprintf("subtask_%d_%d", now.UnixNano(), i+1),
			TaskID:          taskID,
			SubTaskName:     task.Description,
			SubTaskType:     task.Type,
			Status:          domain.AgentSubTaskStatusPending,
			DependSubTaskID: previousID,
			ToolName:        toolName,
			InputParams:     inputParams,
		}
		subs = append(subs, sub)
		previousID = sub.SubTaskID
	}
	return subs
}

// buildSubTaskPlanningUserPrompt 组装规划阶段的 user prompt，附带用户长期偏好辅助约束判断。
func buildSubTaskPlanningUserPrompt(query string, prefs domain.UserPreferences) string {
	var b strings.Builder
	b.WriteString("用户需求：")
	b.WriteString(strings.TrimSpace(query))
	if text := preferencesContextText(prefs); text != "" {
		b.WriteString("\n用户长期偏好：")
		b.WriteString(text)
	}
	b.WriteString("\n只输出 JSON。")
	return b.String()
}

// subtaskToolSpec 把规划子任务映射到既有工具，并生成工具入参。
// product_search 复用文本商品检索；review_query 复用评价查询；add_cart 复用购物车加购；checkout 复用结算。
// condition_extract / confirm_interact 为纯交互/记录步骤，不绑定工具（执行器会直接置成功并做追踪）。
func subtaskToolSpec(task plannedSubTask) (string, string) {
	switch task.Type {
	case domain.SubTaskTypeProductSearch:
		args := map[string]any{"query": task.Query, "limit": 5}
		constraints := map[string]any{}
		if len(task.Categories) > 0 {
			constraints["categories"] = task.Categories
		}
		if len(task.Brands) > 0 {
			constraints["brands"] = task.Brands
		}
		if strings.TrimSpace(task.SkinType) != "" {
			constraints["terms"] = []string{task.SkinType}
		}
		if len(constraints) > 0 {
			args["constraints"] = constraints
		}
		if len(task.Negative) > 0 {
			args["negative"] = map[string]any{"terms": task.Negative}
		}
		raw, _ := json.Marshal(args)
		return toolSearchProducts, string(raw)
	case domain.SubTaskTypeReviewQuery:
		// product_id 在执行期由 TaskExecutor 依据前序检索结果动态填充。
		return toolListReviews, `{"limit": 10}`
	case domain.SubTaskTypeAddCart:
		// product_id / sku_id 在执行期动态填充。
		return toolAddCartItem, `{"quantity": 1}`
	case domain.SubTaskTypeCheckout:
		return toolCheckout, `{}`
	default:
		return "", "{}"
	}
}

// normalizeSubTaskType 对模型输出做白名单收敛，防止 prompt 漂移产生未知类型。
func normalizeSubTaskType(rawType string) string {
	switch strings.TrimSpace(rawType) {
	case domain.SubTaskTypeConditionExtract, domain.SubTaskTypeProductSearch, domain.SubTaskTypeReviewQuery,
		domain.SubTaskTypeConfirmInteract, domain.SubTaskTypeAddCart, domain.SubTaskTypeCheckout:
		return strings.TrimSpace(rawType)
	default:
		return ""
	}
}