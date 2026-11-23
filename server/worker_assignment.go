package server

import (
	"net/http"
)

func (a *api) registerWorkerAssignmentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{ident}/worker-assignment", func(w http.ResponseWriter, r *http.Request) {
		actor, ok := conversationActor(w, r)
		if !ok {
			return
		}
		// Adoption has no client-selected task or prompt. Both come from the
		// exact committed assignment, and retries return the same binding.
		if r.Body != nil {
			var body *struct{}
			if !readJSONRequest(w, r, &body, 1024) {
				return
			}
			if body == nil {
				writeError(w, http.StatusBadRequest, "assignment request must be an object")
				return
			}
		}
		goal, err := selectedTaskGoal(r.Header)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		assignment, err := a.store.AdoptWorkerAssignmentWithGoal(r.Context(), r.PathValue("ident"), actor, goal)
		if err != nil {
			conversationError(w, err)
			return
		}
		writeJSON(w, assignment)
	})
}
