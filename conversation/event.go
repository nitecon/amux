// Package conversation defines provider-independent events consumed by both
// desktop and headless clients. Task/conversation binding belongs to the host.
package conversation

import (
	"context"
	"encoding/json"
)

type Event struct {
	Kind       string             `json:"kind"`
	SessionID  string             `json:"session_id"`
	MessageID  string             `json:"message_id,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
	RequestID  string             `json:"request_id,omitempty"`
	Text       string             `json:"text,omitempty"`
	Title      string             `json:"title,omitempty"`
	Status     string             `json:"status,omitempty"`
	Data       json.RawMessage    `json:"data,omitempty"`
	Options    []PermissionOption `json:"options,omitempty"`
}
type PermissionOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Command is client intent. A permission references the persisted event UUID,
// never an unscoped provider request ID which may be reused in another turn.
type Command struct {
	Kind              string `json:"kind"`
	Text              string `json:"text,omitempty"`
	PermissionEventID string `json:"permission_event_id,omitempty"`
	OptionID          string `json:"option_id,omitempty"`
	PromptCommandID   string `json:"prompt_command_id,omitempty"`
}
type Sink func(context.Context, Event) error
