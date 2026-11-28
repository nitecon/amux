package server

import (
	"encoding/json"
	"net/http"

	"github.com/nitecon/amux/auth"
	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
)

func (a *api) publishGlobalNotification(w http.ResponseWriter, r *http.Request) {
	if !requireUserAdmin(w, r) {
		return
	}
	if a.remote == nil || a.remote.ctx.Err() != nil {
		writeError(w, http.StatusServiceUnavailable, "event router is not running")
		return
	}
	var payload json.RawMessage
	if !readJSONRequest(w, r, &payload, 32<<10) {
		return
	}
	if err := controllers.ValidateGlobalNotification(payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := a.store.PublishBroadcast(r.Context(), a.remote.registry, fism.EmitRequest{
		ID: controllers.GlobalNotifyEventID, Version: 1, Source: "server:" + auth.PrincipalID(r.Context()), Payload: payload,
	})
	if err != nil || !result.Stored {
		writeError(w, http.StatusInternalServerError, "cannot save notification")
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]string{"uid": result.Event.UID})
}
