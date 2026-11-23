package server

import (
	"net/http"

	"github.com/nitecon/amux/conversation"
)

func (a *api) registerConversationCommandRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{ident}/conversations/{conversation}/commands", a.enqueueConversationCommand)
	mux.HandleFunc("GET /v1/projects/{ident}/conversations/{conversation}/commands", a.conversationCommands)
	mux.HandleFunc("GET /v1/projects/{ident}/conversations/{conversation}/commands/changes", a.conversationCommandChanges)
	mux.HandleFunc("POST /v1/projects/{ident}/conversations/{conversation}/commands/{command}/claim", a.claimConversationCommand)
	mux.HandleFunc("POST /v1/projects/{ident}/conversations/{conversation}/commands/{command}/result", a.finishConversationCommand)
}
func (a *api) enqueueConversationCommand(w http.ResponseWriter, r *http.Request) {
	var req *struct {
		ID      string               `json:"id"`
		Command conversation.Command `json:"command"`
	}
	if !readJSONRequest(w, r, &req, 2<<20) {
		return
	}
	if req == nil {
		writeError(w, 400, "command is required")
		return
	}
	c, err := a.store.EnqueueConversationCommand(r.Context(), r.PathValue("ident"), r.PathValue("conversation"), req.ID, req.Command)
	if err != nil {
		conversationError(w, err)
		return
	}
	writeJSON(w, c)
}
func (a *api) conversationCommands(w http.ResponseWriter, r *http.Request) {
	after, limit, ok := replayRange(w, r)
	if !ok {
		return
	}
	commands, err := a.store.ConversationCommands(r.Context(), r.PathValue("ident"), r.PathValue("conversation"), after, limit)
	if err != nil {
		conversationError(w, err)
		return
	}
	writeJSON(w, commands)
}
func (a *api) conversationCommandChanges(w http.ResponseWriter, r *http.Request) {
	after, limit, ok := replayRange(w, r)
	if !ok {
		return
	}
	changes, err := a.store.ConversationCommandChanges(r.Context(), r.PathValue("ident"), r.PathValue("conversation"), after, limit)
	if err != nil {
		conversationError(w, err)
		return
	}
	writeJSON(w, changes)
}
func (a *api) claimConversationCommand(w http.ResponseWriter, r *http.Request) {
	actor, ok := conversationActor(w, r)
	if !ok {
		return
	}
	c, err := a.store.ClaimConversationCommand(r.Context(), r.PathValue("ident"), r.PathValue("conversation"), r.PathValue("command"), actor)
	if err != nil {
		conversationError(w, err)
		return
	}
	writeJSON(w, c)
}
func (a *api) finishConversationCommand(w http.ResponseWriter, r *http.Request) {
	actor, ok := conversationActor(w, r)
	if !ok {
		return
	}
	var req *struct {
		State string `json:"state"`
	}
	if !readTaskRequest(w, r, &req) {
		return
	}
	if req == nil {
		writeError(w, 400, "command state is required")
		return
	}
	c, err := a.store.FinishConversationCommand(r.Context(), r.PathValue("ident"), r.PathValue("conversation"), r.PathValue("command"), req.State, actor)
	if err != nil {
		conversationError(w, err)
		return
	}
	writeJSON(w, c)
}
