package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/nitecon/amux/storage"
)

type targetRegistrationRequest struct {
	Generation      string  `json:"generation"`
	CanonicalRemote *string `json:"canonical_remote"`
	DisplayName     *string `json:"display_name"`
	Hostname        *string `json:"hostname"`
}

type targetListEntry struct {
	Target      storage.RegisteredTargetIdentity `json:"target"`
	SeenAt      int64                            `json:"seen_at"`
	ExpiresAt   int64                            `json:"expires_at"`
	DisplayName *string                          `json:"display_name"`
	Hostname    *string                          `json:"hostname"`
}

func targetRegistrationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrTargetInput):
		writeError(w, http.StatusBadRequest, "invalid target registration")
	case errors.Is(err, storage.ErrTargetBinding):
		writeError(w, http.StatusConflict, "target binding does not match project")
	case errors.Is(err, storage.ErrTaskNotFound):
		writeError(w, http.StatusNotFound, "project not found")
	default:
		writeError(w, http.StatusInternalServerError, "target registration operation failed")
	}
}

func (a *api) registerTargetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{ident}/task-targets", func(w http.ResponseWriter, r *http.Request) {
		origin, err := agentOrigin(r.Header)
		if err != nil || origin == nil {
			writeError(w, http.StatusBadRequest, "native agent origin required")
			return
		}
		var request *targetRegistrationRequest
		if !readTaskRequest(w, r, &request) {
			return
		}
		if request == nil {
			writeError(w, http.StatusBadRequest, "target registration is required")
			return
		}
		registered, err := a.store.RegisterTarget(r.Context(), r.PathValue("ident"), storage.TargetRegistration{
			Origin:          *origin,
			Generation:      request.Generation,
			CanonicalRemote: request.CanonicalRemote,
			DisplayName:     request.DisplayName,
			Hostname:        request.Hostname,
		}, time.Now().UTC())
		if err != nil {
			targetRegistrationError(w, err)
			return
		}
		writeJSON(w, registered)
	})
	mux.HandleFunc("GET /v1/projects/{ident}/task-targets", func(w http.ResponseWriter, r *http.Request) {
		directory, err := a.store.TargetDirectory(r.Context(), r.PathValue("ident"), time.Now().UTC())
		if err != nil {
			targetRegistrationError(w, err)
			return
		}
		entries := make([]targetListEntry, len(directory.Targets))
		for i, target := range directory.Targets {
			entries[i] = targetListEntry{
				Target: target.Target, SeenAt: target.SeenAt, ExpiresAt: target.ExpiresAt,
				DisplayName: target.DisplayName, Hostname: target.Hostname,
			}
		}
		writeJSON(w, struct {
			Targets         []targetListEntry `json:"targets"`
			CanonicalRemote *string           `json:"canonical_remote"`
		}{Targets: entries, CanonicalRemote: directory.CanonicalRemote})
	})
}
