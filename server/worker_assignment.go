package server

import (
	"errors"
	"net/http"

	"github.com/nitecon/amux/auth"
	"github.com/nitecon/amux/storage"
)

func (a *api) registerWorkerAssignmentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{ident}/worker-assignment", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := a.conversationActor(w, r)
		if !ok {
			return
		}
		// A streamed client can narrow adoption to a task. The journal
		// still decides ownership, the prompt and the idempotent binding.
		var taskID string
		if r.Body != nil {
			var body *struct {
				TaskID string `json:"task_id,omitempty"`
			}
			if !readJSONRequest(w, r, &body, 1024) {
				return
			}
			if body == nil {
				writeError(w, http.StatusBadRequest, "assignment request must be an object")
				return
			}
			taskID = body.TaskID
		}
		goal, err := selectedTaskGoal(r.Header)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		assignment, err := a.store.AdoptWorkerTask(r.Context(), r.PathValue("ident"), taskID, actor, goal, auth.PrincipalID(r.Context()))
		if errors.Is(err, storage.ErrTaskOwnership) {
			writeError(w, http.StatusForbidden, "assignment belongs to a different enrolled client")
			return
		}
		if err != nil {
			conversationError(w, err)
			return
		}
		writeJSON(w, assignment)
	})
}
