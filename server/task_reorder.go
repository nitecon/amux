package server

import (
	"net/http"
	"strings"

	"github.com/nitecon/amux/storage"
)

func (a *api) reorderTasks(w http.ResponseWriter, r *http.Request) {
	origin, ok := requestActor(w, r)
	if !ok {
		return
	}
	project := r.PathValue("ident")
	p, err := a.store.Project(r.Context(), project)
	if err != nil {
		taskError(w, err)
		return
	}
	if p == nil {
		taskError(w, storage.ErrTaskNotFound)
		return
	}
	query := r.URL.Query()
	statusValues := query["status"]
	if len(statusValues) != 1 {
		writeError(w, http.StatusBadRequest, "exactly one status is required")
		return
	}
	if len(query["goal"]) > 1 {
		writeError(w, http.StatusBadRequest, "at most one goal is allowed")
		return
	}
	status := statusValues[0]
	if !storage.IsReorderableTaskStatus(status) {
		writeError(w, http.StatusBadRequest, "invalid status: must be todo|in_progress|stalled|user_review|done")
		return
	}
	var req *struct {
		Order []string `json:"order"`
	}
	if !readTaskRequest(w, r, &req) {
		return
	}
	if req == nil || req.Order == nil {
		writeError(w, http.StatusBadRequest, "order is required")
		return
	}
	selectedGoal := r.Header.Get("X-Task-Goal")
	responseGoal, filterGoal := "", origin != nil
	if values, exists := query["goal"]; exists {
		filterGoal = true
		if len(values) > 0 {
			responseGoal = values[0]
		}
	} else if selectedGoal != "" {
		responseGoal = selectedGoal
		filterGoal = true
	}
	tasks, err := a.store.ReorderTasks(r.Context(), project, status, req.Order, origin, storage.TaskTransitionPolicy{
		HumanRequest: origin == nil,
		CheckGoal:    origin != nil,
		SelectedGoal: selectedGoal,
	})
	if err != nil {
		taskError(w, err)
		return
	}
	if filterGoal {
		goal := strings.TrimSpace(responseGoal)
		filtered := make([]storage.TaskSummary, 0, len(tasks))
		for _, task := range tasks {
			value := ""
			if task.Goal != nil {
				value = *task.Goal
			}
			if value == goal {
				filtered = append(filtered, task)
			}
		}
		tasks = filtered
	}
	writeJSON(w, tasks)
}
