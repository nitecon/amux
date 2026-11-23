package server

import (
	"errors"
	"net/http"

	"github.com/nitecon/amux/storage"
)

type createTaskSubtaskRequest struct {
	Title           string   `json:"title"`
	Description     *string  `json:"description"`
	Specification   *string  `json:"specification"`
	TargetProjectID *string  `json:"target_project_ident"`
	Labels          []string `json:"labels"`
}

func (a *api) getTaskSubtasks(w http.ResponseWriter, r *http.Request) {
	children, err := a.store.TaskSubtasks(r.Context(), r.PathValue("ident"), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, storage.ErrSubtaskParentNotFound) {
			writeError(w, http.StatusNotFound, "parent task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "cannot read task subtasks")
		return
	}
	writeJSON(w, children)
}

func (a *api) createTaskSubtask(w http.ResponseWriter, r *http.Request) {
	origin, ok := requestActor(w, r)
	if !ok {
		return
	}
	var request *createTaskSubtaskRequest
	if !readTaskRequest(w, r, &request) {
		return
	}
	if request == nil {
		writeError(w, http.StatusBadRequest, "subtask must be an object")
		return
	}
	child, err := a.store.CreateTaskSubtask(r.Context(), storage.TaskSubtaskInsert{
		ParentProject: r.PathValue("ident"), ParentID: r.PathValue("id"),
		TargetProject: request.TargetProjectID, Title: request.Title,
		Description: request.Description, Specification: request.Specification,
		Labels: request.Labels, Reporter: resolveTaskIdentity(nil, r),
	}, origin)
	if err != nil {
		writeTaskSubtaskError(w, err)
		return
	}
	writeJSON(w, child)
}

func writeTaskSubtaskError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrSubtaskParentNotFound):
		writeError(w, http.StatusNotFound, "parent task not found")
	case errors.Is(err, storage.ErrSubtaskTargetNotFound):
		writeError(w, http.StatusNotFound, "target project not found")
	case errors.Is(err, storage.ErrSubtaskTargetArchived):
		writeError(w, http.StatusConflict, "target project is archived")
	default:
		taskError(w, err)
	}
}
