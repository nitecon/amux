package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/nitecon/amux/storage"
)

type providerSettingReceipt struct {
	storage.ProviderSetting
	ReleasedTasks int `json:"released_tasks"`
}

func (a *api) registerProviderRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/providers", a.listProviders)
	mux.HandleFunc("PUT /v1/providers/{id}", a.setProviderEnabled)
}

func (a *api) listProviders(w http.ResponseWriter, r *http.Request) {
	settings, err := a.store.ProviderSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "provider settings unavailable")
		return
	}
	writeJSON(w, settings)
}

func (a *api) setProviderEnabled(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	decoder := json.NewDecoder(r.Body)
	var request struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decoder.Decode(&request); err != nil || request.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled must be a boolean")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request must contain one JSON object")
		return
	}
	providerID := r.PathValue("id")
	released, err := a.store.SetProviderEnabled(r.Context(), providerID, *request.Enabled)
	if err != nil {
		if errors.Is(err, storage.ErrUnknownProvider) {
			writeError(w, http.StatusNotFound, "unknown provider")
			return
		}
		writeError(w, http.StatusInternalServerError, "provider setting update failed")
		return
	}
	writeJSON(w, providerSettingReceipt{
		ProviderSetting: storage.ProviderSetting{ID: providerID, Enabled: *request.Enabled},
		ReleasedTasks:   released,
	})
}
