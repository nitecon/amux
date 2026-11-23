package storage

import "context"

type conversationFenceKey struct{}

type conversationFence struct {
	project      string
	conversation string
}

// WithConversationFence binds task mutations to the current owner of a conversation.
// The owner is rechecked in the task mutation transaction to prevent handoff races.
func WithConversationFence(ctx context.Context, project, conversation string) context.Context {
	return context.WithValue(ctx, conversationFenceKey{}, conversationFence{project: project, conversation: conversation})
}
