package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/nitecon/amux/storage"
)

type humanProjectMessageRequest struct {
	Content         string          `json:"content"`
	ParentMessageID json.RawMessage `json:"parent_message_id"`
	// Author is a display label for external channel sends and is never stored
	// as the message's agent identity.
	Author  string          `json:"author"`
	Resolve json.RawMessage `json:"resolve"`
}

type humanProjectMessageResponse struct {
	MessageID       int64   `json:"message_id"`
	ParentMessageID *int64  `json:"parent_message_id"`
	Delivered       bool    `json:"delivered"`
	DeliveryError   *string `json:"delivery_error"`
}

func (a *api) registerProjectMessageAuthoringRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{ident}/messages/human", a.postHumanProjectMessage)
}

func (a *api) postHumanProjectMessage(w http.ResponseWriter, r *http.Request) {
	if hasAgentIdentityHeaders(r) {
		writeError(w, http.StatusBadRequest, "human project messages cannot include native agent identity headers")
		return
	}
	var request humanProjectMessageRequest
	if !readJSONRequest(w, r, &request, 1<<20) {
		return
	}
	content := strings.TrimSpace(request.Content)
	if content == "" {
		writeError(w, http.StatusBadRequest, "content must not be empty")
		return
	}
	project, err := a.store.Project(r.Context(), r.PathValue("ident"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "project lookup failed")
		return
	}
	if project == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	resolve := humanResolveValue(request.Resolve)
	resolvedBy := ""
	if resolve {
		resolvedBy = projectMessageResolverIdentity(r)
	}
	message, err := a.store.CreateHumanProjectMessage(r.Context(), project.Ident, storage.HumanProjectMessageInput{
		Content: content, ParentMessageID: humanParentMessageID(request.ParentMessageID), Resolve: resolve, ResolvedBy: resolvedBy,
	})
	if err != nil {
		// The Rust route maps project/parent insert failures to NOT_FOUND. The
		// project existence check above keeps missing-project responses explicit.
		writeError(w, http.StatusNotFound, "project message or parent not found")
		return
	}
	response := humanProjectMessageResponse{MessageID: message.ID, ParentMessageID: message.ParentMessageID}
	if project.ChannelName == "internal" {
		response.Delivered = true
		writeJSON(w, response)
		return
	}
	provider, ok := a.channels[project.ChannelName]
	if !ok {
		deliveryError := fmt.Sprintf("channel plugin '%s' is not configured", project.ChannelName)
		response.DeliveryError = &deliveryError
		writeJSON(w, response)
		return
	}
	project, err = a.ensureProjectRoom(r.Context(), project, provider)
	if err != nil {
		deliveryError := err.Error()
		response.DeliveryError = &deliveryError
		writeJSON(w, response)
		return
	}
	var parentExternalID *string
	if message.ParentMessageID != nil {
		parent, parentErr := a.store.ProjectMessage(r.Context(), project.Ident, *message.ParentMessageID)
		if parentErr != nil {
			deliveryError := parentErr.Error()
			response.DeliveryError = &deliveryError
			writeJSON(w, response)
			return
		}
		parentExternalID = parent.ExternalMessageID
	}
	author := strings.TrimSpace(request.Author)
	if author == "" {
		author = "user"
		if user := authenticatedBrowserUser(r.Context()); user != nil {
			author = user.DisplayName
		}
	}
	text := fmt.Sprintf("**%s** (via gateway): %s", author, content)
	externalID, err := provider.SendText(r.Context(), project.RoomID, parentExternalID, text)
	if err != nil {
		deliveryError := err.Error()
		response.DeliveryError = &deliveryError
		writeJSON(w, response)
		return
	}
	if err := a.store.SetProjectMessageExternalID(r.Context(), project.Ident, message.ID, externalID); err != nil {
		writeError(w, http.StatusInternalServerError, "external message receipt could not be saved")
		return
	}
	response.Delivered = true
	writeJSON(w, response)
}

func hasAgentIdentityHeaders(r *http.Request) bool {
	for _, name := range []string{"X-Agent-Session-Id", "X-Agent-Instance-Id", "X-Agent-Provider", "X-Agent-Os", "X-Agent-Id"} {
		if len(r.Header.Values(name)) != 0 {
			return true
		}
	}
	return false
}

func humanParentMessageID(raw json.RawMessage) *int64 {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	var value string
	if len(trimmed) > 0 && trimmed[0] == '"' {
		if json.Unmarshal(trimmed, &value) != nil {
			return nil
		}
	} else {
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.UseNumber()
		var decoded any
		if decoder.Decode(&decoded) != nil {
			return nil
		}
		number, ok := decoded.(json.Number)
		if !ok {
			return nil
		}
		value = number.String()
	}
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id <= 0 {
		return nil
	}
	return &id
}

func humanResolveValue(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var value string
		if json.Unmarshal(trimmed, &value) != nil {
			return false
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true", "on", "1", "yes":
			return true
		default:
			return false
		}
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case json.Number:
		number, err := typed.Int64()
		return err == nil && number != 0
	default:
		return false
	}
}
