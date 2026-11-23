package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/nitecon/amux/storage"
)

func (a *api) registerPluginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/plugins", a.listPlugins)
	mux.HandleFunc("PUT /v1/plugins/{id}", a.setPluginEnabled)
}

func (a *api) listPlugins(w http.ResponseWriter, r *http.Request) {
	settings, err := a.store.PluginSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "plugin settings unavailable")
		return
	}
	writeJSON(w, settings)
}

func (a *api) setPluginEnabled(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	decoder := json.NewDecoder(r.Body)
	var body json.RawMessage
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "enabled must be a boolean")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request must contain one JSON object")
		return
	}
	pluginID := r.PathValue("id")
	known := false
	for _, id := range storage.SwitchablePluginIDs() {
		if id == pluginID {
			known = true
			break
		}
	}
	if !known {
		writeError(w, http.StatusNotFound, "unknown plugin")
		return
	}
	var request *struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(w, http.StatusBadRequest, "enabled must be a boolean")
		return
	}
	if request == nil || request.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled must be a boolean")
		return
	}
	if err := a.store.SetPluginEnabled(r.Context(), pluginID, *request.Enabled); err != nil {
		if errors.Is(err, storage.ErrUnknownPlugin) {
			writeError(w, http.StatusNotFound, "unknown plugin")
			return
		}
		writeError(w, http.StatusInternalServerError, "plugin setting update failed")
		return
	}
	writeJSON(w, storage.PluginSetting{ID: pluginID, Enabled: *request.Enabled})
}
