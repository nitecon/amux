package server

import (
	"encoding/json"
	"net/http"

	"github.com/nitecon/amux/storage"
)

func (a *api) registerTaskCancellationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/projects/{ident}/tasks/{id}/cancellation", a.getTaskCancellation)
	mux.HandleFunc("POST /v1/projects/{ident}/tasks/{id}/cancellation", a.cancelTask)
}

func (a *api) getTaskCancellation(w http.ResponseWriter, r *http.Request) {
	report, err := a.store.TaskCancellation(r.Context(), r.PathValue("ident"), r.PathValue("id"))
	if err != nil {
		taskError(w, err)
		return
	}
	if report == nil {
		taskError(w, storage.ErrTaskNotFound)
		return
	}
	writeJSON(w, report)
}

func (a *api) cancelTask(w http.ResponseWriter, r *http.Request) {
	origin, ok := requestActor(w, r)
	if !ok {
		return
	}
	if origin == nil {
		writeError(w, http.StatusBadRequest, "exact agent origin required")
		return
	}
	var input *struct {
		Reason    *string         `json:"reason"`
		Changeset json.RawMessage `json:"changeset"`
	}
	if !readTaskRequest(w, r, &input) {
		return
	}
	if input == nil || input.Reason == nil {
		writeError(w, http.StatusBadRequest, "reason must be 1..=16384 bytes")
		return
	}
	reason, err := storage.ValidateTaskCancellationReason(*input.Reason)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.Changeset != nil {
		if _, err := storage.ValidateChangeset(input.Changeset); err != nil {
			writeError(w, http.StatusBadRequest, "changeset must be a valid file array or null")
			return
		}
	}
	result, err := a.store.CancelTask(r.Context(), r.PathValue("ident"), r.PathValue("id"), reason, input.Changeset, origin)
	if err != nil {
		taskError(w, err)
		return
	}
	if result == nil {
		taskError(w, storage.ErrTaskNotFound)
		return
	}
	writeJSON(w, result)
}
