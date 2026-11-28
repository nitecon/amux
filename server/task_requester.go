package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

type taskRequesterRequest struct {
	Requester json.RawMessage `json:"requester"`
}

func parseOptionalRequester(raw json.RawMessage) (*storage.RegisteredTargetIdentity, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var requester *storage.RegisteredTargetIdentity
	if err := strictFrame(raw, &requester); err != nil || requester == nil {
		return nil, fmt.Errorf("invalid requester")
	}
	return requester, nil
}

func currentRequester(ctx context.Context, store *storage.Store, project string, origin *agent.Origin) (*storage.RegisteredTargetIdentity, error) {
	if origin == nil {
		return nil, fmt.Errorf("requester subscription requires a native execution")
	}
	directory, err := store.TargetDirectory(ctx, project, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	var found *storage.RegisteredTargetIdentity
	for _, target := range directory.Targets {
		if target.Target.Origin != *origin {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("requesting execution has multiple active registrations")
		}
		identity := target.Target
		found = &identity
	}
	if found == nil {
		return nil, fmt.Errorf("requesting execution is not currently registered in this project")
	}
	return found, nil
}

func (a *api) setTaskRequester(w http.ResponseWriter, r *http.Request) {
	origin, ok := requestActor(w, r)
	if !ok {
		return
	}
	var request *taskRequesterRequest
	if !readTaskRequest(w, r, &request) {
		return
	}
	if request == nil || len(request.Requester) == 0 {
		writeError(w, http.StatusBadRequest, "requester is required; use null to clear it")
		return
	}
	requester, err := parseOptionalRequester(request.Requester)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid requester subscription")
		return
	}
	if requester != nil && origin != nil && requester.Origin != *origin {
		writeError(w, http.StatusBadRequest, "native callers may subscribe only their current execution")
		return
	}
	task, err := a.store.SetTaskRequester(r.Context(), r.PathValue("ident"), r.PathValue("id"), requester, origin)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, task)
}
