package server

import (
	"net/http"
)

func (a *api) registerExecutionHistoryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/execution/runs", a.executionRuns)
}

func (a *api) executionRuns(w http.ResponseWriter, r *http.Request) {
	query, err := parseEventQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid event query")
		return
	}
	if !requireUserAdmin(w, r) {
		return
	}
	runs, err := a.store.ListExecutionRuns(r.Context(), query.Project, query.TaskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "execution history unavailable")
		return
	}
	writeJSONStatus(w, http.StatusOK, runs)
}
