package httpapi

import (
	"net/http"
	"strings"
)

// 任务规划流水线的管理员查询接口：供后台“链路追踪 / 任务计划可视化”使用。
// 前端通过列表页拿到任务，再按 task_id 拉取子任务明细（状态、耗时、重试、校验结果、工具摘要）。

func (s *Server) handleListAdminAgentTasks(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	page, pageSize := readPagination(r)
	items, total := s.store.ListAgentTasksPage(r.Context(), page, pageSize)
	writeJSON(w, http.StatusOK, pagedPayload(items, page, pageSize, total))
}

func (s *Server) handleAdminAgentTaskDetail(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	taskID, ok := splitAdminTaskPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "接口不存在")
		return
	}
	task, found := s.store.GetAgentTask(r.Context(), taskID)
	if !found {
		writeError(w, http.StatusNotFound, "task_not_found", "任务不存在")
		return
	}
	subs := s.store.ListAgentSubTasks(r.Context(), taskID)
	writeJSON(w, http.StatusOK, map[string]any{"task": task, "subtasks": subs})
}

func splitAdminTaskPath(path string) (string, bool) {
	rest := strings.TrimPrefix(path, "/api/v1/admin/agent/tasks/")
	rest = strings.TrimSpace(rest)
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}