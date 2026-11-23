package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/nitecon/amux/storage"
)

func (a *api) reassignTask(w http.ResponseWriter, r *http.Request) {
	actor, ok := requestActor(w, r)
	if !ok {
		return
	}
	if actor == nil {
		writeError(w, http.StatusBadRequest, "exact calling agent origin required")
		return
	}
	a.assignTaskHTTP(w, r, actor)
}

func (a *api) assignTask(w http.ResponseWriter, r *http.Request) {
	origin, err := agentOrigin(r.Header)
	if err != nil || origin != nil {
		writeError(w, http.StatusBadRequest, "human assignment must not send agent identity headers")
		return
	}
	if id := strings.TrimSpace(r.Header.Get("X-Agent-Id")); id != "" && id != "_default" {
		writeError(w, http.StatusBadRequest, "human assignment must not send agent identity headers")
		return
	}
	a.assignTaskHTTP(w, r, nil)
}

func (a *api) assignTaskHTTP(w http.ResponseWriter, r *http.Request, actor *storage.AgentOrigin) {
	var request *struct {
		Target *storage.RegisteredTargetIdentity `json:"target"`
	}
	if !readTaskRequest(w, r, &request) {
		return
	}
	if request == nil || request.Target == nil {
		writeError(w, http.StatusBadRequest, "target is required")
		return
	}
	var task *storage.Task
	var err error
	if actor != nil {
		task, err = a.store.ReassignTask(r.Context(), r.PathValue("ident"), r.PathValue("id"), *actor, *request.Target, time.Now().UTC())
	} else {
		task, err = a.store.AssignTaskFromUser(r.Context(), r.PathValue("ident"), r.PathValue("id"), *request.Target, time.Now().UTC())
	}
	if err != nil {
		taskError(w, err)
		return
	}
	if task == nil {
		taskError(w, storage.ErrTaskNotFound)
		return
	}
	writeJSON(w, task)
}
