package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/nitecon/amux/auth"
	"github.com/nitecon/amux/storage"
)

func presenceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrPresenceInput):
		writeError(w, http.StatusBadRequest, "invalid presence report")
	case errors.Is(err, storage.ErrPresenceConflict):
		writeError(w, http.StatusConflict, "presence identity is held by another source")
	case errors.Is(err, storage.ErrTaskNotFound):
		writeError(w, http.StatusNotFound, "project not found")
	default:
		writeError(w, http.StatusInternalServerError, "presence operation failed")
	}
}

func (a *api) registerPresenceRoutes(mux *http.ServeMux) {
	a.registerTargetRoutes(mux)
	a.registerPresenceSummaryRoutes(mux)
	mux.HandleFunc("POST /v1/worker-presence", func(w http.ResponseWriter, r *http.Request) {
		var report *storage.PresenceReport
		if !readTaskRequest(w, r, &report) {
			return
		}
		if report == nil {
			writeError(w, http.StatusBadRequest, "presence report is required")
			return
		}
		principal := auth.PrincipalID(r.Context())
		if principal == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		accepted, err := a.store.ReportPresenceWithPrincipal(r.Context(), *report, time.Now().UTC(), principal)
		if err != nil {
			presenceError(w, err)
			return
		}
		writeJSON(w, struct {
			Accepted bool `json:"accepted"`
		}{accepted})
	})
	mux.HandleFunc("GET /v1/projects/{ident}/worker-presence", func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Query()) != 0 {
			writeError(w, http.StatusBadRequest, "presence query does not accept parameters")
			return
		}
		agents, err := a.store.Presence(r.Context(), r.PathValue("ident"))
		if err != nil {
			presenceError(w, err)
			return
		}
		writeJSON(w, agents)
	})
}

func (a *api) registerPresenceSummaryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/presence/summary", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		hostnames := query["hostname"]
		if len(query) != 0 && (len(query) != 1 || len(hostnames) != 1 || strings.TrimSpace(hostnames[0]) == "" || len(hostnames[0]) > 255) {
			writeError(w, http.StatusBadRequest, "invalid presence summary hostname")
			return
		}
		summary, err := a.store.PresenceSummaryForHost(r.Context(), time.Now().UTC(), query.Get("hostname"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "presence summary failed")
			return
		}
		writeJSON(w, summary)
	})
}
