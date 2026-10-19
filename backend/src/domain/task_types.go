package domain

import "time"

// 任务规划流水线（复杂导购任务）的状态与数据结构。
// 主任务持有一份用户原始需求；子任务按依赖顺序执行并各自持久化状态，
// 用于支持断点恢复、链路追踪可视化和任务维度效果评估。

type AgentTaskStatus string

const (
	AgentTaskStatusInit      AgentTaskStatus = "INIT"
	AgentTaskStatusPlanning  AgentTaskStatus = "PLANNING"
	AgentTaskStatusRunning   AgentTaskStatus = "RUNNING"
	AgentTaskStatusPaused    AgentTaskStatus = "PAUSED"
	AgentTaskStatusCompleted AgentTaskStatus = "COMPLETED"
	AgentTaskStatusFailed    AgentTaskStatus = "FAILED"
)

type AgentSubTaskStatus string

const (
	AgentSubTaskStatusPending AgentSubTaskStatus = "PENDING"
	AgentSubTaskStatusRunning AgentSubTaskStatus = "RUNNING"
	AgentSubTaskStatusSuccess AgentSubTaskStatus = "SUCCESS"
	AgentSubTaskStatusFailed  AgentSubTaskStatus = "FAILED"
	AgentSubTaskStatusSkipped AgentSubTaskStatus = "SKIPPED"
)

// 子任务类型。规划器把复杂导购需求拆成有序步骤，执行器据此映射到具体工具，
// 结果校验器据此选择校验规则。
const (
	SubTaskTypeConditionExtract = "condition_extract"
	SubTaskTypeProductSearch    = "product_search"
	SubTaskTypeReviewQuery      = "review_query"
	SubTaskTypeConfirmInteract  = "confirm_interact"
	SubTaskTypeAddCart          = "add_cart"
	SubTaskTypeCheckout         = "checkout"
)

type AgentTask struct {
	TaskID         string          `json:"task_id"`
	UserID         string          `json:"user_id"`
	ConversationID string          `json:"conversation_id"`
	RunID          string          `json:"run_id,omitempty"`
	Query          string          `json:"query"`
	TaskStatus     AgentTaskStatus `json:"task_status"`
	PlanContent    string          `json:"plan_content,omitempty"`
	Resumed        bool            `json:"resumed,omitempty"`
	FailReason     string          `json:"fail_reason,omitempty"`
	CreateTime     time.Time       `json:"create_time"`
	FinishTime     *time.Time      `json:"finish_time,omitempty"`
}

type AgentSubTask struct {
	SubTaskID       string             `json:"sub_task_id"`
	TaskID          string             `json:"task_id"`
	SubTaskName     string             `json:"sub_task_name"`
	SubTaskType     string             `json:"sub_task_type"`
	Status          AgentSubTaskStatus `json:"status"`
	DependSubTaskID string             `json:"depend_sub_task_id,omitempty"`
	ToolName        string             `json:"tool_name,omitempty"`
	InputParams     string             `json:"input_params,omitempty"`
	OutputResult    string             `json:"output_result,omitempty"`
	VerifyResult    string             `json:"verify_result,omitempty"`
	RetryCount      int                `json:"retry_count"`
	CreatedAt       time.Time          `json:"created_at"`
	FinishedAt      *time.Time         `json:"finished_at,omitempty"`
}