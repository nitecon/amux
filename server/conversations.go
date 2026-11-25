package server

import (
	"context"
	"errors"
	"github.com/coder/websocket"
	"net/http"
	"strconv"
	"time"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/storage"
)

func (a *api) registerConversationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{ident}/tasks/{id}/conversations", a.createConversation)
	mux.HandleFunc("POST /v1/projects/{ident}/conversations", a.createConversation)
	mux.HandleFunc("GET /v1/projects/{ident}/tasks/{id}/conversations", a.listConversations)
	mux.HandleFunc("GET /v1/projects/{ident}/conversations", a.listConversations)
	mux.HandleFunc("GET /v1/projects/{ident}/conversations/{conversation}", a.getConversation)
	mux.HandleFunc("PUT /v1/projects/{ident}/conversations/{conversation}/session", a.bindConversation)
	mux.HandleFunc("POST /v1/projects/{ident}/conversations/{conversation}/resume", a.resumeConversation)
	mux.HandleFunc("POST /v1/projects/{ident}/conversations/{conversation}/events", a.appendConversationEvent)
	mux.HandleFunc("GET /v1/projects/{ident}/conversations/{conversation}/events", a.conversationEvents)
	mux.HandleFunc("GET /v1/projects/{ident}/conversations/{conversation}/stream", a.conversationStream)
}
func conversationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrConversationNotFound), errors.Is(err, storage.ErrTaskNotFound):
		writeError(w, 404, "conversation or task not found")
	case errors.Is(err, storage.ErrConversationInput):
		writeError(w, 400, err.Error())
	case errors.Is(err, storage.ErrConversationConflict), errors.Is(err, storage.ErrTaskConflict), errors.Is(err, storage.ErrTaskOwnership):
		writeError(w, 409, err.Error())
	default:
		writeError(w, 500, "conversation operation failed")
	}
}
func (a *api) conversationActor(w http.ResponseWriter, r *http.Request) (*storage.AgentOrigin, bool) {
	actor, ok := requestActor(w, r)
	if !ok {
		return nil, false
	}
	if actor == nil {
		writeError(w, 400, "conversation writes require complete agent origin headers")
		return nil, false
	}
	if id := r.PathValue("conversation"); id != "" {
		allowed, err := a.store.FleetConversationWriteAllowed(r.Context(), id, authenticatedPrincipal(r.Context()))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "check conversation owner")
			return nil, false
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "conversation belongs to a different enrolled client")
			return nil, false
		}
	}
	return actor, true
}
func (a *api) createConversation(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.conversationActor(w, r)
	if !ok {
		return
	}
	goal, err := selectedTaskGoal(r.Header)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid task goal")
		return
	}
	project, taskID := r.PathValue("ident"), r.PathValue("id")
	if taskID == "" && goal != nil && *goal != "" {
		writeError(w, http.StatusBadRequest, "a task goal requires a task")
		return
	}
	c, err := a.store.CreateConversationWithGoal(r.Context(), project, taskID, actor, goal)
	if err != nil {
		conversationError(w, err)
		return
	}
	writeJSON(w, c)
}
func (a *api) getConversation(w http.ResponseWriter, r *http.Request) {
	c, err := a.store.Conversation(r.Context(), r.PathValue("ident"), r.PathValue("conversation"))
	if err != nil {
		conversationError(w, err)
		return
	}
	writeJSON(w, c)
}

func (a *api) listConversations(w http.ResponseWriter, r *http.Request) {
	after := ""
	limit := 100
	for key, entries := range r.URL.Query() {
		if len(entries) != 1 {
			writeError(w, 400, "invalid conversation list query")
			return
		}
		switch key {
		case "after_id":
			after = entries[0]
		case "limit":
			n, err := strconv.Atoi(entries[0])
			if err != nil || n < 1 || n > 1000 {
				writeError(w, 400, "invalid conversation list limit")
				return
			}
			limit = n
		default:
			writeError(w, 400, "invalid conversation list query")
			return
		}
	}
	project, taskID := r.PathValue("ident"), r.PathValue("id")
	var items []storage.Conversation
	var err error
	if taskID == "" {
		items, err = a.store.ProjectConversations(r.Context(), project, after, limit)
	} else {
		items, err = a.store.TaskConversations(r.Context(), project, taskID, after, limit)
	}
	if err != nil {
		conversationError(w, err)
		return
	}
	writeJSON(w, items)
}
func (a *api) bindConversation(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.conversationActor(w, r)
	if !ok {
		return
	}
	var req *struct {
		NativeSessionID string `json:"native_session_id"`
	}
	if !readTaskRequest(w, r, &req) {
		return
	}
	if req == nil {
		writeError(w, 400, "session binding is required")
		return
	}
	c, err := a.store.BindConversationSession(r.Context(), r.PathValue("ident"), r.PathValue("conversation"), req.NativeSessionID, actor)
	if err != nil {
		conversationError(w, err)
		return
	}
	writeJSON(w, c)
}
func (a *api) resumeConversation(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.conversationActor(w, r)
	if !ok {
		return
	}
	var req *struct {
		PreviousInstanceID string `json:"previous_instance_id"`
	}
	if !readTaskRequest(w, r, &req) {
		return
	}
	if req == nil || req.PreviousInstanceID == "" {
		writeError(w, 400, "previous_instance_id is required")
		return
	}
	c, err := a.store.ResumeConversation(r.Context(), r.PathValue("ident"), r.PathValue("conversation"), req.PreviousInstanceID, actor)
	if err != nil {
		conversationError(w, err)
		return
	}
	writeJSON(w, c)
}
func (a *api) appendConversationEvent(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.conversationActor(w, r)
	if !ok {
		return
	}
	var req *struct {
		ID    string             `json:"id"`
		Event conversation.Event `json:"event"`
	}
	if !readJSONRequest(w, r, &req, 5<<20) {
		return
	}
	if req == nil {
		writeError(w, 400, "event is required")
		return
	}
	e, err := a.store.AppendConversationEvent(r.Context(), r.PathValue("ident"), r.PathValue("conversation"), req.ID, req.Event, actor)
	if err != nil {
		conversationError(w, err)
		return
	}
	writeJSON(w, e)
}
func replayRange(w http.ResponseWriter, r *http.Request) (int64, int, bool) {
	values := r.URL.Query()
	after := int64(0)
	limit := 100
	for key, entries := range values {
		if (key != "after" && key != "limit") || len(entries) != 1 {
			writeError(w, 400, "invalid replay query")
			return 0, 0, false
		}
		n, err := strconv.ParseInt(entries[0], 10, 64)
		if err != nil || n < 0 {
			writeError(w, 400, "invalid replay range")
			return 0, 0, false
		}
		if key == "after" {
			after = n
		} else {
			if n < 1 || n > 1000 {
				writeError(w, 400, "invalid replay limit")
				return 0, 0, false
			}
			limit = int(n)
		}
	}
	return after, limit, true
}
func (a *api) conversationEvents(w http.ResponseWriter, r *http.Request) {
	after, limit, ok := replayRange(w, r)
	if !ok {
		return
	}
	events, err := a.store.ConversationEvents(r.Context(), r.PathValue("ident"), r.PathValue("conversation"), after, limit)
	if err != nil {
		conversationError(w, err)
		return
	}
	writeJSON(w, events)
}

// conversationStream is read-only: writes and permission decisions use the
// authenticated HTTP API. Each event carries a durable per-conversation sequence
// which the client supplies as ?after=N on reconnect.
func (a *api) conversationStream(w http.ResponseWriter, r *http.Request) {
	after, afterCommands := int64(0), int64(0)
	for key, entries := range r.URL.Query() {
		if (key != "after" && key != "after_commands") || len(entries) != 1 {
			writeError(w, 400, "invalid conversation cursor")
			return
		}
		n, err := strconv.ParseInt(entries[0], 10, 64)
		if err != nil || n < 0 {
			writeError(w, 400, "invalid conversation cursor")
			return
		}
		if key == "after" {
			after = n
		} else {
			afterCommands = n
		}
	}
	project, id := r.PathValue("ident"), r.PathValue("conversation")
	bound, err := a.store.Conversation(r.Context(), project, id)
	if err != nil {
		conversationError(w, err)
		return
	}
	knownOwner := bound.Owner
	a.withStream(w, r, streamFrameBytes, func(parent context.Context, c *websocket.Conn) {
		ctx := c.CloseRead(parent)
		if err := sendFrame(ctx, c, map[string]any{"type": "ready", "version": 1, "conversation_id": id, "after": after, "after_commands": afterCommands, "owner": knownOwner}); err != nil {
			return
		}
		heartbeat := time.NewTicker(20 * time.Second)
		defer heartbeat.Stop()
		for {
			changed := a.store.ConversationChanges()
			current, err := a.store.Conversation(ctx, project, id)
			if err != nil {
				return
			}
			if current.Owner != knownOwner {
				if err := sendFrame(ctx, c, map[string]any{"type": "owner", "version": 1, "owner": current.Owner}); err != nil {
					return
				}
				knownOwner = current.Owner
			}
			events, err := a.store.ConversationEvents(ctx, project, id, after, 100)
			if err != nil {
				return
			}
			for _, event := range events {
				if err := sendFrame(ctx, c, map[string]any{"type": "event", "version": 1, "event": event}); err != nil {
					return
				}
				after = event.Sequence
			}
			commands, err := a.store.ConversationCommandChanges(ctx, project, id, afterCommands, 100)
			if err != nil {
				return
			}
			for _, command := range commands {
				if err := sendFrame(ctx, c, map[string]any{"type": "command", "version": 1, "command": command}); err != nil {
					return
				}
				afterCommands = command.Revision
			}
			if len(events) > 0 || len(commands) > 0 {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-changed:
				continue
			case <-heartbeat.C:
				pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				err := c.Ping(pingCtx)
				cancel()
				if err != nil {
					return
				}
			}
		}
	})
}
