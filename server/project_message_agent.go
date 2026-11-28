package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nitecon/amux/channels"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func (a *api) createProjectMessageRoom(w http.ResponseWriter, r *http.Request) {
	project, err := a.store.Project(r.Context(), r.PathValue("ident"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "project lookup failed")
		return
	}
	if project == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	if project.ChannelName != "internal" {
		provider, ok := a.channels[project.ChannelName]
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "external channel integration is not configured")
			return
		}
		project, err = a.ensureProjectRoom(r.Context(), project, provider)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "channel room creation failed")
			return
		}
	}
	writeJSON(w, *project)
}

type agentProjectMessageRequest struct {
	Body     *string         `json:"body"`
	Content  json.RawMessage `json:"content"`
	Message  json.RawMessage `json:"message"`
	Subject  *string         `json:"subject"`
	Hostname *string         `json:"hostname"`
	EventAt  *int64          `json:"event_at"`
}

func (request *agentProjectMessageRequest) UnmarshalJSON(data []byte) error {
	type wire agentProjectMessageRequest
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*request = agentProjectMessageRequest(decoded)
	return nil
}

type agentProjectMessageReceipt struct {
	MessageID         int64  `json:"message_id"`
	ExternalMessageID string `json:"external_message_id"`
	ParentMessageID   *int64 `json:"parent_message_id,omitempty"`
}

func (a *api) sendAgentProjectMessage(w http.ResponseWriter, r *http.Request) {
	a.writeAgentProjectMessage(w, r, "message")
}

func (a *api) replyAgentProjectMessage(w http.ResponseWriter, r *http.Request) {
	a.writeAgentProjectMessage(w, r, "reply")
}

func (a *api) actionAgentProjectMessage(w http.ResponseWriter, r *http.Request) {
	a.writeAgentProjectMessage(w, r, "action")
}

func (a *api) writeAgentProjectMessage(w http.ResponseWriter, r *http.Request, messageType string) {
	origin, ok := requestActor(w, r)
	if !ok {
		return
	}
	if origin == nil {
		writeError(w, http.StatusBadRequest, "project messages require a complete native agent origin")
		return
	}
	var parentID *int64
	if messageType != "message" {
		parsed, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid parent message ID")
			return
		}
		parentID = &parsed
	}
	var req *agentProjectMessageRequest
	if !readTaskRequest(w, r, &req) {
		return
	}
	if req == nil {
		writeError(w, http.StatusBadRequest, "project message must be an object")
		return
	}
	alias := req.Content
	if messageType == "action" {
		alias = req.Message
	}
	aliasText, err := decodeProjectMessageAlias(alias)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project message body alias")
		return
	}
	body := preferredMessageBody(req.Body, aliasText)
	bodyText := ""
	if body != nil {
		bodyText = *body
	}
	subject, hostname, eventAt := projectMessageDefaults(bodyText, req.Subject, messageType == "action", req.Hostname, req.EventAt, *origin)
	input := storage.AgentProjectMessageInput{
		Content: bodyText, ParentMessageID: parentID, MessageType: messageType, Subject: subject, Hostname: hostname, EventAt: eventAt,
	}
	message, err := a.store.CreateAgentProjectMessage(r.Context(), r.PathValue("ident"), *origin, input)
	if !errors.Is(err, storage.ErrProjectMessageUnsupportedChannel) {
		respondAgentProjectMessage(w, message, err)
		return
	}
	project, projectErr := a.store.Project(r.Context(), r.PathValue("ident"))
	if projectErr != nil {
		respondAgentProjectMessage(w, nil, projectErr)
		return
	}
	if project == nil {
		respondAgentProjectMessage(w, nil, storage.ErrTaskNotFound)
		return
	}
	provider, ok := a.channels[project.ChannelName]
	if !ok {
		respondAgentProjectMessage(w, nil, storage.ErrProjectMessageUnsupportedChannel)
		return
	}
	var parentExternalID *string
	if parentID != nil {
		parent, parentErr := a.store.ProjectMessage(r.Context(), project.Ident, *parentID)
		if parentErr != nil {
			respondAgentProjectMessage(w, nil, parentErr)
			return
		}
		parentExternalID = parent.ExternalMessageID
	}
	project, err = a.ensureProjectRoom(r.Context(), project, provider)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "channel room creation failed")
		return
	}
	externalID, err := provider.Send(r.Context(), project.RoomID, parentExternalID, channels.OutboundMessage{
		AgentID: origin.SessionID, Hostname: *hostname, Subject: *subject, Body: bodyText, EventAt: *eventAt,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "channel message send failed")
		return
	}
	if strings.TrimSpace(externalID) == "" {
		writeError(w, http.StatusInternalServerError, "channel returned an empty message receipt")
		return
	}
	input.ExternalMessageID = &externalID
	message, err = a.store.CreateAgentProjectMessage(r.Context(), project.Ident, *origin, input)
	respondAgentProjectMessage(w, message, err)
}

func (a *api) ensureProjectRoom(ctx context.Context, project *storage.Project, provider channels.Outbound) (*storage.Project, error) {
	if project.RoomID != "" {
		return project, nil
	}
	room, err := provider.EnsureRoom(ctx, project.Ident)
	if err != nil {
		return nil, err
	}
	return a.store.SetProjectRoom(ctx, project.Ident, room)
}

func preferredMessageBody(body, alias *string) *string {
	if body != nil {
		return body
	}
	return alias
}

func decodeProjectMessageAlias(raw json.RawMessage) (*string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func projectMessageDefaults(body string, suppliedSubject *string, action bool, suppliedHostname *string, suppliedEventAt *int64, origin agent.Origin) (*string, *string, *int64) {
	derived := deriveProjectMessageSubject(body)
	var subject string
	if action && suppliedSubject == nil {
		subject = "[ACTION] " + derived
	} else if suppliedSubject == nil || strings.TrimSpace(*suppliedSubject) == "" {
		subject = derived
	} else {
		subject = strings.TrimSpace(*suppliedSubject)
	}
	// Native messages use the registered session ID when hostname is omitted.
	hostname := origin.SessionID
	if suppliedHostname != nil && strings.TrimSpace(*suppliedHostname) != "" {
		hostname = strings.TrimSpace(*suppliedHostname)
	}
	eventAt := time.Now().UnixMilli()
	if suppliedEventAt != nil {
		eventAt = *suppliedEventAt
	}
	return &subject, &hostname, &eventAt
}

func deriveProjectMessageSubject(body string) string {
	first := ""
	for _, line := range strings.Split(body, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			first = trimmed
			break
		}
	}
	if first == "" {
		return "(no content)"
	}
	runes := []rune(first)
	if len(runes) <= 80 {
		return first
	}
	return string(runes[:79]) + "…"
}

func respondAgentProjectMessage(w http.ResponseWriter, message *storage.ProjectMessage, err error) {
	if errors.Is(err, storage.ErrProjectMessageInput) {
		writeError(w, http.StatusBadRequest, "invalid project message")
		return
	}
	if errors.Is(err, storage.ErrProjectMessageNotFound) || errors.Is(err, storage.ErrTaskNotFound) {
		writeError(w, http.StatusNotFound, "project message or project not found")
		return
	}
	if errors.Is(err, storage.ErrProjectMessageUnsupportedChannel) {
		writeError(w, http.StatusServiceUnavailable, "external channel integration is not configured")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "project message could not be stored")
		return
	}
	if message == nil || message.ID <= 0 || message.ExternalMessageID == nil || strings.TrimSpace(*message.ExternalMessageID) == "" {
		writeError(w, http.StatusInternalServerError, "project message receipt unavailable")
		return
	}
	writeJSON(w, agentProjectMessageReceipt{MessageID: message.ID, ExternalMessageID: *message.ExternalMessageID, ParentMessageID: message.ParentMessageID})
}
