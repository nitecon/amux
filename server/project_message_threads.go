package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/nitecon/amux/storage"
)

// registerProjectMessageThreadRoutes adds the project-scoped thread lookup.
// The caller owns registration alongside the existing project message routes.
func (a *api) registerProjectMessageThreadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/projects/{ident}/messages/{id}/thread", a.getProjectMessageThread)
}

func (a *api) getProjectMessageThread(w http.ResponseWriter, r *http.Request) {
	messageID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || messageID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid project message ID")
		return
	}
	thread, err := a.store.GetProjectMessageThread(r.Context(), r.PathValue("ident"), messageID)
	if errors.Is(err, storage.ErrProjectMessageNotFound) {
		writeError(w, http.StatusNotFound, "project message not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "project message thread lookup failed")
		return
	}
	writeJSON(w, thread)
}
