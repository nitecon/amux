package localconversation

import (
	"context"
	"fmt"
	"sync"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/storage"
)

type taskApproval struct {
	Approval
	resolve func(context.Context, string) error
}

// TaskApprovals pairs currently pending task permissions with the running
// task's command transport.
type TaskApprovals struct {
	mu      sync.Mutex
	pending map[string]taskApproval
	changed chan struct{}
}

func NewTaskApprovals() *TaskApprovals {
	return &TaskApprovals{pending: make(map[string]taskApproval), changed: make(chan struct{})}
}

func (p *TaskApprovals) Observe(info storage.Conversation, id string, event conversation.Event, resolve func(context.Context, string) error) {
	switch event.Kind {
	case "permission", "permission_resolved", "turn_completed", "closed":
	default:
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	updated := false
	for key, previous := range p.pending {
		if previous.Conversation.ID == info.ID && (event.Kind == "turn_completed" || event.Kind == "closed" || previous.Event.Event.RequestID == event.RequestID) {
			delete(p.pending, key)
			updated = true
		}
	}
	if event.Kind == "permission" && event.RequestID != "" && id != "" && resolve != nil {
		p.pending[id] = taskApproval{Approval{Conversation: info, Event: storage.ConversationEvent{ID: id, Event: event}, Task: true}, resolve}
		updated = true
	}
	if updated {
		close(p.changed)
		p.changed = make(chan struct{})
	}
}

func (p *TaskApprovals) snapshot() ([]Approval, <-chan struct{}) {
	if p == nil {
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]Approval, 0, len(p.pending))
	for _, approval := range p.pending {
		result = append(result, approval.Approval)
	}
	return result, p.changed
}

func (p *TaskApprovals) resolve(ctx context.Context, id, option string) error {
	p.mu.Lock()
	approval, exists := p.pending[id]
	p.mu.Unlock()
	if !exists {
		return fmt.Errorf("permission request is no longer active")
	}
	return approval.resolve(ctx, option)
}
