// Package channels contains outbound project-channel adapters.
package channels

import "context"

// OutboundMessage is the structured agent message sent to a provider.
type OutboundMessage struct {
	AgentID  string
	Hostname string
	Subject  string
	Body     string
	EventAt  int64
}

// Outbound provisions project rooms and sends text or structured messages.
type Outbound interface {
	EnsureRoom(context.Context, string) (string, error)
	Send(context.Context, string, *string, OutboundMessage) (string, error)
	SendText(context.Context, string, *string, string) (string, error)
}
