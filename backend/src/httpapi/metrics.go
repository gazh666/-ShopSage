package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"shopsage/backend/src/domain"
)

// defaultCNYPer1KTokens 是推理成本估算用的单价（人民币 / 千 token）。
// 仅用于后台展示的量级估算，实际计费以模型提供商账单为准。
const defaultCNYPer1KTokens = 0.004

type agentBusinessMetrics struct {
	WindowHours    int                  `json:"window_hours"`
	Runs           int                  `json:"runs"`
	TaskCompletion taskCompletionMetric `json:"task_completion"`
	Task           taskMetric           `json:"task"`
	Tool           toolMetric           `json:"tool"`
	Latency        latencyMetric        `json:"latency"`
	TokenCost      tokenCostMetric      `json:"token_cost"`
	Conversion     conversionMetric     `json:"conversion"`
}

type taskCompletionMetric struct {
	CompletedRuns   int     `json:"completed_runs"`
	FoundProductRuns int    `json:"found_product_runs"`
	CheckoutRuns    int     `json:"checkout_runs"`
	GoalReachedRuns int     `json:"goal_reached_runs"`
	CompletionRate  float64 `json:"completion_rate"`
}

type toolMetric struct {
	Calls              int            `json:"calls"`
	Success            int            `json:"success"`
	SuccessRate        float64        `json:"success_rate"`
	RetriesTotal       int64          `json:"retries_total"`
	RetryDistribution  map[string]int `json:"retry_distribution"`
	SearchProductCalls int            `json:"search_product_calls"`
	SearchProductEmpty int            `json:"search_product_empty"`
}

type latencyMetric struct {
	ToolP95MS int64 `json:"tool_p95_ms"`
	ToolAvgMS int64 `json:"tool_avg_ms"`
	LLMP95MS  int64 `json:"llm_p95_ms"`
	LLMAvgMS  int64 `json:"llm_avg_ms"`
}

type tokenCostMetric struct {
	PromptCharsTotal   int64   `json:"prompt_chars_total"`
	OutputCharsTotal   int64   `json:"output_chars_total"`
	EstimatedTokens    int64   `json:"estimated_tokens"`
	EstimatedCostCNY   float64 `json:"estimated_cost_cny"`
}

type conversionMetric struct {
	RecommendedProducts int     `json:"recommended_products"`
	AddCartCalls        int     `json:"add_cart_calls"`
	AddCartSuccess      int     `json:"add_cart_success"`
	AddCartRate         float64 `json:"add_cart_rate"`
}

// taskMetric 是任务规划流水线维度的效果评估指标：
// 导购任务完成率、子任务执行成功率、工具平均重试次数、任务中断恢复成功率、复杂请求平均耗时。
type taskMetric struct {
	Tasks              int     `json:"tasks"`
	Completed          int     `json:"completed"`
	Failed             int     `json:"failed"`
	CompletionRate     float64 `json:"completion_rate"`
	SubTaskTotal       int     `json:"subtask_total"`
	SubTaskSuccess     int     `json:"subtask_success"`
	SubTaskSuccessRate float64 `json:"subtask_success_rate"`
	AvgRetries         float64 `json:"avg_retries"`
	ResumedTasks       int     `json:"resumed_tasks"`
	ResumeSuccessRate  float64 `json:"resume_success_rate"`
	AvgDurationMS      int64   `json:"avg_duration_ms"`
}

func (s *Server) handleAdminAgentMetrics(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	hours := 24
	if raw := r.URL.Query().Get("hours"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 24*30 {
			hours = parsed
		}
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	events := s.store.ListAgentTraceEvents(r.Context(), since, 20000)
	runs := s.store.ListRecentAgentRuns(r.Context(), 100)
	tasks := s.store.ListAgentTasksSince(r.Context(), since, 2000)
	subs := s.store.ListAgentSubTasksSince(r.Context(), since, 20000)
	writeJSON(w, http.StatusOK, computeAgentBusinessMetrics(hours, runs, events, tasks, subs))
}

func computeAgentBusinessMetrics(windowHours int, runs []domain.AgentRun, events []domain.AgentTraceEvent, tasks []domain.AgentTask, subs []domain.AgentSubTask) agentBusinessMetrics {
	m := agentBusinessMetrics{WindowHours: windowHours, Runs: len(runs)}
	m.Tool.RetryDistribution = map[string]int{}
	m.Task = computeTaskMetric(tasks, subs)

	foundSet := map[string]bool{}
	checkoutSet := map[string]bool{}
	for _, run := range runs {
		if run.Status != domain.RunStatusCompleted {
			continue
		}
		m.TaskCompletion.CompletedRuns++
		for _, block := range run.Blocks {
			switch block.Type {
			case "product_refs":
				if len(block.ProductIDs) > 0 {
					foundSet[run.RunID] = true
				}
			case "order_summary":
				if len(block.Orders) > 0 {
					checkoutSet[run.RunID] = true
				}
			}
		}
	}
	m.TaskCompletion.FoundProductRuns = len(foundSet)
	m.TaskCompletion.CheckoutRuns = len(checkoutSet)
	m.TaskCompletion.GoalReachedRuns = unionCount(foundSet, checkoutSet)
	if m.TaskCompletion.CompletedRuns > 0 {
		m.TaskCompletion.CompletionRate = ratio(m.TaskCompletion.GoalReachedRuns, m.TaskCompletion.CompletedRuns)
	}

	toolLatency := []int64{}
	llmLatency := []int64{}
	var retriesTotal int64
	for _, event := range events {
		meta := parseTraceMetadata(event.MetadataJSON)
		if event.Stage == "tools" {
			m.Tool.Calls++
			if event.Status == "ok" {
				m.Tool.Success++
			}
			retries := intValue(metaNested(meta, "observation", "retries"), 0)
			retriesTotal += int64(retries)
			bucket := strconv.Itoa(retries)
			m.Tool.RetryDistribution[bucket]++
			if event.DurationMS > 0 {
				toolLatency = append(toolLatency, event.DurationMS)
			}
			if event.EventType == "search_products" {
				m.Tool.SearchProductCalls++
				ids := stringSlice(meta["product_ids"])
				if len(ids) == 0 {
					m.Tool.SearchProductEmpty++
				} else {
					m.Conversion.RecommendedProducts += len(ids)
				}
			}
			if event.EventType == "add_cart_item" {
				m.Conversion.AddCartCalls++
				if event.Status == "ok" {
					m.Conversion.AddCartSuccess++
				}
			}
			continue
		}
		if event.EventType == "llm_call" {
			m.TokenCost.PromptCharsTotal += int64(intValue(meta["prompt_chars"], 0))
			m.TokenCost.OutputCharsTotal += int64(intValue(meta["raw_length"], 0))
			if event.DurationMS > 0 {
				llmLatency = append(llmLatency, event.DurationMS)
			}
		}
	}
	m.Tool.RetriesTotal = retriesTotal
	if m.Tool.Calls > 0 {
		m.Tool.SuccessRate = ratio(m.Tool.Success, m.Tool.Calls)
	}
	if m.Conversion.AddCartCalls > 0 {
		m.Conversion.AddCartRate = ratio(m.Conversion.AddCartSuccess, m.Conversion.AddCartCalls)
	}
	m.Latency.ToolP95MS = percentile(toolLatency, 0.95)
	m.Latency.ToolAvgMS = average(toolLatency)
	m.Latency.LLMP95MS = percentile(llmLatency, 0.95)
	m.Latency.LLMAvgMS = average(llmLatency)

	// token 估算：中文为主，粗略按 1 字符 ≈ 1 token；成本按固定单价估算。
	m.TokenCost.EstimatedTokens = m.TokenCost.PromptCharsTotal + m.TokenCost.OutputCharsTotal
	m.TokenCost.EstimatedCostCNY = float64(m.TokenCost.EstimatedTokens) / 1000 * defaultCNYPer1KTokens
	return m
}

func computeTaskMetric(tasks []domain.AgentTask, subs []domain.AgentSubTask) taskMetric {
	m := taskMetric{Tasks: len(tasks)}
	var durations []int64
	for _, task := range tasks {
		if task.Resumed {
			m.ResumedTasks++
		}
		switch task.TaskStatus {
		case domain.AgentTaskStatusCompleted:
			m.Completed++
		case domain.AgentTaskStatusFailed:
			m.Failed++
		}
		if !task.CreateTime.IsZero() && task.FinishTime != nil {
			if d := task.FinishTime.Sub(task.CreateTime).Milliseconds(); d > 0 {
				durations = append(durations, d)
			}
		}
	}
	if m.Tasks > 0 {
		m.CompletionRate = ratio(m.Completed, m.Tasks)
	}
	m.SubTaskTotal = len(subs)
	retries := 0
	for _, sub := range subs {
		retries += sub.RetryCount
		if sub.Status == domain.AgentSubTaskStatusSuccess {
			m.SubTaskSuccess++
		}
	}
	if m.SubTaskTotal > 0 {
		m.SubTaskSuccessRate = ratio(m.SubTaskSuccess, m.SubTaskTotal)
		m.AvgRetries = float64(retries) / float64(m.SubTaskTotal)
	}
	// 中断恢复成功率：已恢复任务里最终完成的比例（无恢复任务时为 0）。
	if m.ResumedTasks > 0 {
		resumedCompleted := 0
		for _, task := range tasks {
			if task.Resumed && task.TaskStatus == domain.AgentTaskStatusCompleted {
				resumedCompleted++
			}
		}
		m.ResumeSuccessRate = ratio(resumedCompleted, m.ResumedTasks)
	}
	m.AvgDurationMS = average(durations)
	return m
}

func parseTraceMetadata(raw string) map[string]any {
	out := map[string]any{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func metaNested(meta map[string]any, keys ...string) any {
	var current any = meta
	for _, key := range keys {
		obj, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = obj[key]
	}
	return current
}

func intValue(value any, fallback int) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		if parsed, err := v.Int64(); err == nil {
			return int(parsed)
		}
	case string:
		if parsed, err := strconv.Atoi(v); err == nil {
			return parsed
		}
	}
	return fallback
}

func stringSlice(value any) []string {
	switch v := value.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	default:
		return nil
	}
}

func unionCount(a map[string]bool, b map[string]bool) int {
	merged := map[string]bool{}
	for key := range a {
		merged[key] = true
	}
	for key := range b {
		merged[key] = true
	}
	return len(merged)
}

func ratio(part int, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total)
}

func percentile(values []int64, quantile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(float64(len(sorted)-1) * quantile)
	return sorted[index]
}

func average(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	var sum int64
	for _, value := range values {
		sum += value
	}
	return sum / int64(len(values))
}