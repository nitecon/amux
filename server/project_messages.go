package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/nitecon/amux/storage"
)

type unreadProjectMessagesResponse struct {
	Messages []storage.ProjectMessage `json:"messages"`
	Status   string                   `json:"status"`
}

type confirmProjectMessageResponse struct {
	Confirmed bool `json:"confirmed"`
}

func (a *api) registerProjectMessageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/projects/{ident}/messages", a.listProjectMessageThreads)
	mux.HandleFunc("POST /v1/projects/{ident}/room", a.createProjectMessageRoom)
	mux.HandleFunc("POST /v1/projects/{ident}/messages", a.sendAgentProjectMessage)
	mux.HandleFunc("POST /v1/projects/{ident}/messages/{id}/reply", a.replyAgentProjectMessage)
	mux.HandleFunc("POST /v1/projects/{ident}/messages/{id}/action", a.actionAgentProjectMessage)
	mux.HandleFunc("GET /v1/projects/{ident}/messages/unread", a.getUnreadProjectMessages)
	mux.HandleFunc("POST /v1/projects/{ident}/messages/{id}/confirm", a.confirmProjectMessage)
	a.registerProjectMessageThreadRoutes(mux)
	a.registerProjectMessageAuthoringRoutes(mux)
	a.registerProjectMessageResolutionRoutes(mux)
}

func (a *api) getUnreadProjectMessages(w http.ResponseWriter, r *http.Request) {
	origin, ok := requestActor(w, r)
	if !ok {
		return
	}
	if origin == nil {
		writeError(w, http.StatusBadRequest, "unread messages require a complete native agent origin")
		return
	}
	project := r.PathValue("ident")
	if !projectExists(w, r, a.store, project) {
		return
	}
	messages, err := a.store.GetUnconfirmedProjectMessages(r.Context(), project, origin.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unread message lookup failed")
		return
	}
	status := "no messages"
	if len(messages) != 0 {
		status = strconv.Itoa(len(messages)) + " unread user message(s)"
	}
	writeJSON(w, unreadProjectMessagesResponse{Messages: messages, Status: status})
}

func (a *api) confirmProjectMessage(w http.ResponseWriter, r *http.Request) {
	origin, ok := requestActor(w, r)
	if !ok {
		return
	}
	if origin == nil {
		writeError(w, http.StatusBadRequest, "message confirmation requires a complete native agent origin")
		return
	}
	messageID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || messageID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid project message ID")
		return
	}
	project := r.PathValue("ident")
	if !projectExists(w, r, a.store, project) {
		return
	}
	confirmed, err := a.store.ConfirmProjectMessage(r.Context(), project, origin.SessionID, messageID)
	if errors.Is(err, storage.ErrProjectMessageNotFound) {
		writeError(w, http.StatusNotFound, "project message not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "message confirmation failed")
		return
	}
	writeJSON(w, confirmProjectMessageResponse{Confirmed: confirmed})
}

func projectExists(w http.ResponseWriter, r *http.Request, store *storage.Store, ident string) bool {
	project, err := store.Project(r.Context(), ident)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "project lookup failed")
		return false
	}
	if project == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return false
	}
	return true
}
