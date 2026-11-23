package gatewayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/storage"
)

type ProjectMessageInput struct {
	Body     *string `json:"body,omitempty"`
	Content  *string `json:"content,omitempty"`
	Message  *string `json:"message,omitempty"`
	Subject  *string `json:"subject,omitempty"`
	Hostname *string `json:"hostname,omitempty"`
	EventAt  *int64  `json:"event_at,omitempty"`
}

type ProjectMessageReceipt struct {
	MessageID         int64  `json:"message_id"`
	ExternalMessageID string `json:"external_message_id"`
	ParentMessageID   *int64 `json:"parent_message_id,omitempty"`
}

// CreateProjectMessageRoom returns the registered project and provisioned room.
// It performs one POST and never retries an uncertain response.
func (c *Client) CreateProjectMessageRoom(ctx context.Context, project string) (*storage.Project, error) {
	project = projects.NormalizeIdent(project)
	if project == "" {
		return nil, fmt.Errorf("project is required")
	}
	var raw json.RawMessage
	path := "/v1/projects/" + url.PathEscape(project) + "/room"
	if err := c.do(ctx, http.MethodPost, path, nil, nil, &raw); err != nil {
		return nil, fmt.Errorf("project room outcome is not confirmed; inspect the project before retrying: %w", err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, fmt.Errorf("invalid project room acknowledgement")
	}
	for _, name := range []string{"ident", "channel_name", "room_id", "kind"} {
		value, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("project room acknowledgement omitted %s", name)
		}
	}
	var result storage.Project
	if json.Unmarshal(raw, &result) != nil || result.Ident != project || strings.TrimSpace(result.ChannelName) == "" || strings.TrimSpace(result.RoomID) == "" || strings.TrimSpace(result.Kind) == "" {
		return nil, fmt.Errorf("project room acknowledgement does not match the requested project")
	}
	return &result, nil
}

func (c *Client) SendProjectMessage(ctx context.Context, project string, input ProjectMessageInput, origin storage.AgentOrigin) (*ProjectMessageReceipt, error) {
	return c.writeProjectMessage(ctx, project, 0, "messages", input, origin)
}

func (c *Client) ReplyToProjectMessage(ctx context.Context, project string, parentID int64, input ProjectMessageInput, origin storage.AgentOrigin) (*ProjectMessageReceipt, error) {
	return c.writeProjectMessage(ctx, project, parentID, "reply", input, origin)
}

func (c *Client) TakeProjectMessageAction(ctx context.Context, project string, parentID int64, input ProjectMessageInput, origin storage.AgentOrigin) (*ProjectMessageReceipt, error) {
	return c.writeProjectMessage(ctx, project, parentID, "action", input, origin)
}

func (c *Client) writeProjectMessage(ctx context.Context, project string, parentID int64, operation string, input ProjectMessageInput, origin storage.AgentOrigin) (*ProjectMessageReceipt, error) {
	project = projects.NormalizeIdent(project)
	if project == "" {
		return nil, fmt.Errorf("project is required")
	}
	if operation != "messages" && parentID <= 0 {
		return nil, fmt.Errorf("parent message ID must be positive")
	}
	actor, err := storage.NormalizeOrigin(&origin)
	if err != nil {
		return nil, err
	}
	body := input.Body
	if body == nil {
		body = input.Content
		if operation == "action" {
			body = input.Message
		}
	}
	if body == nil || strings.TrimSpace(*body) == "" {
		return nil, fmt.Errorf("message body must not be empty")
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode project message: %w", err)
	}
	if len(encoded) > 1<<20 {
		return nil, fmt.Errorf("encoded project message exceeds 1 MiB")
	}
	path := "/v1/projects/" + url.PathEscape(project) + "/messages"
	if operation == "reply" || operation == "action" {
		path += "/" + strconv.FormatInt(parentID, 10) + "/" + operation
	}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodPost, path, input, actor, &raw); err != nil {
		return nil, fmt.Errorf("project message outcome is not confirmed; inspect the project thread before retrying: %w", err)
	}
	receipt, err := parseProjectMessageReceipt(raw, parentID, operation != "messages")
	if err != nil {
		return nil, fmt.Errorf("project message outcome is not confirmed; inspect the project thread before retrying: %w", err)
	}
	return receipt, nil
}

func parseProjectMessageReceipt(raw json.RawMessage, expectedParent int64, isReply bool) (*ProjectMessageReceipt, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, fmt.Errorf("invalid project message acknowledgement")
	}
	if _, ok := fields["message_id"]; !ok {
		return nil, fmt.Errorf("project message acknowledgement omitted message_id")
	}
	if _, ok := fields["external_message_id"]; !ok {
		return nil, fmt.Errorf("project message acknowledgement omitted external_message_id")
	}
	if isReply {
		if _, ok := fields["parent_message_id"]; !ok {
			return nil, fmt.Errorf("project message acknowledgement omitted parent_message_id")
		}
	} else if _, ok := fields["parent_message_id"]; ok {
		return nil, fmt.Errorf("root project message acknowledgement unexpectedly has a parent")
	}
	var receipt ProjectMessageReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil || receipt.MessageID <= 0 || strings.TrimSpace(receipt.ExternalMessageID) == "" {
		return nil, fmt.Errorf("invalid project message acknowledgement")
	}
	if strings.HasPrefix(receipt.ExternalMessageID, "internal:") && receipt.ExternalMessageID != "internal:"+strconv.FormatInt(receipt.MessageID, 10) {
		return nil, fmt.Errorf("project message internal receipt does not match message_id")
	}
	if isReply {
		parentRaw := bytes.TrimSpace(fields["parent_message_id"])
		if len(parentRaw) == 0 || bytes.Equal(parentRaw, []byte("null")) {
			return nil, fmt.Errorf("invalid acknowledged parent message ID")
		}
		var parent int64
		if err := json.Unmarshal(parentRaw, &parent); err != nil || parent != expectedParent || receipt.MessageID <= parent {
			return nil, fmt.Errorf("project message acknowledgement has a different parent")
		}
		receipt.ParentMessageID = &parent
	}
	return &receipt, nil
}
