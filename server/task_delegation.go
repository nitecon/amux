package server

import (
	"net/http"
	"strings"

	"github.com/nitecon/amux/storage"
)

type delegateTaskRequest struct {
	TargetProject string                            `json:"target_project_ident"`
	Title         string                            `json:"title"`
	ParentID      *string                           `json:"parent_task_id"`
	Specification *string                           `json:"specification"`
	Details       *string                           `json:"details"`
	Description   *string                           `json:"description"`
	Labels        []string                          `json:"labels"`
	Hostname      *string                           `json:"hostname"`
	Reporter      *string                           `json:"reporter"`
	Requester     *storage.RegisteredTargetIdentity `json:"requester"`
}

type updateTaskDelegationRequest struct {
	Title         *string  `json:"title"`
	Description   *string  `json:"description"`
	Details       *string  `json:"details"`
	Specification *string  `json:"specification"`
	Labels        []string `json:"labels"`
}

func (a *api) updateTaskDelegation(w http.ResponseWriter, r *http.Request) {
	origin, ok := requestActor(w, r)
	if !ok {
		return
	}
	var req *updateTaskDelegationRequest
	if !readTaskRequest(w, r, &req) {
		return
	}
	if req == nil {
		writeError(w, http.StatusBadRequest, "delegation update must be an object")
		return
	}
	result, err := a.store.UpdateTaskDelegationContract(r.Context(), r.PathValue("ident"), r.PathValue("id"), storage.TaskDelegationUpdate{
		Title: req.Title, Description: req.Description, Details: req.Details, Specification: req.Specification, Labels: req.Labels,
	}, origin)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, result)
}

func (a *api) delegateTask(w http.ResponseWriter, r *http.Request) {
	origin, ok := requestActor(w, r)
	if !ok {
		return
	}
	var req *delegateTaskRequest
	if !readTaskRequest(w, r, &req) {
		return
	}
	if req == nil {
		writeError(w, http.StatusBadRequest, "delegation must be an object")
		return
	}
	if strings.TrimSpace(req.TargetProject) == "" || strings.TrimSpace(req.Title) == "" {
		writeError(w, http.StatusBadRequest, "target_project_ident and title are required")
		return
	}
	details := req.Specification
	if details == nil {
		details = req.Details
	}
	result, err := a.store.DelegateTask(r.Context(), storage.TaskDelegationInput{
		SourceProject: r.PathValue("ident"), TargetProject: req.TargetProject, Title: req.Title,
		Reporter: resolveTaskIdentity(req.Reporter, r), ParentID: req.ParentID,
		Description: req.Description, Details: details, Hostname: req.Hostname,
		Labels: req.Labels, Requester: req.Requester,
	}, origin)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, result)
}
