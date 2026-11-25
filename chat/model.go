// Package chat projects persisted provider events into a UI-ready transcript.
package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/storage"
)

var (
	ErrSequenceGap = errors.New("conversation event sequence gap")
	ErrConflict    = errors.New("conflicting conversation event replay")
)

type Snapshot struct {
	Sequence    int64
	Items       []Item
	Permissions []Permission
	Draft       string
}

type Item struct {
	ID         string
	Kind       string
	Role       string
	Text       string
	Title      string
	Command    string
	Status     string
	ToolCallID string
	Expanded   bool
	Details    json.RawMessage
}

type Permission struct {
	EventID    string
	RequestID  string
	ToolCallID string
	Title      string
	Options    []conversation.PermissionOption
	Details    json.RawMessage
}

type Model struct {
	mu             sync.Mutex
	sequence       int64
	conversationID string
	sessionID      string
	items          []Item
	permissions    []Permission
	draft          string
	bySequence     map[int64]storage.ConversationEvent
	byID           map[string]int64
	itemByID       map[string]int
	expanded       map[string]bool
	pendingUser    map[string][]int
	hasAnonymous   bool
	anonymousKey   string
	anonymousIdx   int
}

func New() *Model {
	return &Model{bySequence: map[int64]storage.ConversationEvent{}, byID: map[string]int64{}, itemByID: map[string]int{}, expanded: map[string]bool{}, pendingUser: map[string][]int{}}
}

func (m *Model) Apply(e storage.ConversationEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.Sequence < 1 || e.ID == "" {
		return fmt.Errorf("invalid conversation event identity")
	}
	if e.ConversationID == "" || e.Event.SessionID == "" {
		return fmt.Errorf("conversation event has no conversation or native session identity")
	}
	if (m.conversationID != "" && e.ConversationID != m.conversationID) || (m.sessionID != "" && e.Event.SessionID != m.sessionID) {
		return ErrConflict
	}
	if e.Sequence <= m.sequence {
		prior, ok := m.bySequence[e.Sequence]
		if !ok || prior.ID != e.ID || prior.ConversationID != e.ConversationID || !sameEvent(prior.Event, e.Event) {
			return ErrConflict
		}
		return nil
	}
	if e.Sequence != m.sequence+1 {
		return fmt.Errorf("%w: have %d, received %d", ErrSequenceGap, m.sequence, e.Sequence)
	}
	if _, exists := m.byID[e.ID]; exists {
		return ErrConflict
	}
	// Validate and reduce before committing the replay cursor so a malformed
	// event can be retried after its source is corrected.
	if err := m.reduce(e); err != nil {
		return err
	}
	if m.conversationID == "" {
		m.conversationID = e.ConversationID
		m.sessionID = e.Event.SessionID
	}
	e.Event = cloneEvent(e.Event)
	m.bySequence[e.Sequence] = e
	m.byID[e.ID] = e.Sequence
	m.sequence = e.Sequence
	return nil
}

func (m *Model) Snapshot() Snapshot {
	snapshot, _, _ := m.TranscriptWindow(-1, -1)
	return snapshot
}

// TranscriptWindow returns at most limit items before end, plus their starting
// index and the total item count. A negative bound selects the full history.
func (m *Model) TranscriptWindow(end, limit int) (Snapshot, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := len(m.items)
	if end < 0 || end > count {
		end = count
	}
	start := 0
	if limit >= 0 {
		start = max(0, end-limit)
	}
	s := Snapshot{Sequence: m.sequence, Draft: m.draft, Items: make([]Item, end-start), Permissions: make([]Permission, len(m.permissions))}
	for i, item := range m.items[start:end] {
		s.Items[i] = item
		s.Items[i].Details = append(json.RawMessage(nil), item.Details...)
		s.Items[i].Expanded = item.Kind == "tool" && m.expanded[item.ToolCallID]
	}
	for i, permission := range m.permissions {
		s.Permissions[i] = permission
		s.Permissions[i].Options = append([]conversation.PermissionOption(nil), permission.Options...)
		s.Permissions[i].Details = cloneRaw(permission.Details)
	}
	return s, start, count
}

func (m *Model) Draft() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.draft
}

func (m *Model) Sequence() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sequence
}

func (m *Model) SetExpanded(toolID string, expanded bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	index, ok := m.itemByID["tool:"+toolID]
	if toolID == "" || !ok || m.items[index].Kind != "tool" {
		return fmt.Errorf("unknown tool call %q", toolID)
	}
	m.expanded[toolID] = expanded
	return nil
}

func (m *Model) SetDraft(text string) {
	m.mu.Lock()
	m.draft = text
	m.mu.Unlock()
}

// ClearPermissions drops pending approvals for a live owner-frame transition.
// It does not advance the durable event sequence.
func (m *Model) ClearPermissions() {
	m.mu.Lock()
	m.permissions = nil
	m.mu.Unlock()
}

func (m *Model) reduce(e storage.ConversationEvent) error {
	ev := e.Event
	if ev.Kind == "permission" {
		if ev.RequestID == "" {
			return fmt.Errorf("permission event has no request ID")
		}
		for _, p := range m.permissions {
			if p.EventID == e.ID || p.RequestID == ev.RequestID {
				return fmt.Errorf("%w: duplicate pending permission", ErrConflict)
			}
		}
	}
	switch ev.Kind {
	case "tool_call", "tool_update", "tool_output_delta":
		if ev.ToolCallID == "" {
			return fmt.Errorf("tool event has no call ID")
		}
	}
	anonymousKey := anonymousDeltaKey(ev)
	if anonymousKey == "" || !m.hasAnonymous || m.anonymousKey != anonymousKey {
		m.hasAnonymous = false
	}
	switch ev.Kind {
	case "user_message", "user_delta", "user_snapshot", "text_delta", "message_snapshot", "reasoning_delta", "reasoning_snapshot", "content":
		return m.reduceMessage(e)
	case "tool_call", "tool_update", "tool_output_delta":
		return m.reduceTool(e)
	case "permission":
		m.permissions = append(m.permissions, Permission{EventID: e.ID, RequestID: ev.RequestID, ToolCallID: ev.ToolCallID, Title: ev.Title, Options: append([]conversation.PermissionOption(nil), ev.Options...), Details: cloneRaw(ev.Data)})
	case "permission_resolved":
		m.retirePermission(ev.RequestID)
	case "turn_completed", "owner_handoff":
		m.permissions = nil
	case "state", "session_ready", "turn_started", "unsupported_request":
		m.appendStandalone(e, "state", "")
	default:
		// Unknown provider-neutral events remain inspectable without blocking replay.
		m.appendStandalone(e, "state", "")
	}
	return nil
}

func (m *Model) reduceMessage(e storage.ConversationEvent) error {
	ev := e.Event
	role, kind := "assistant", "message"
	if strings.HasPrefix(ev.Kind, "user_") {
		role = "user"
	}
	if strings.HasPrefix(ev.Kind, "reasoning_") {
		kind = "reasoning"
	}
	if ev.Kind == "content" {
		kind = "content"
	}
	key := ev.MessageID
	if key != "" {
		key = role + ":" + key
	}
	index, found := -1, false
	if key != "" {
		index, found = m.itemByID[key]
	} else if anonymousDeltaKey(ev) != "" && m.hasAnonymous {
		index, found = m.anonymousIdx, true
	}
	// Worker echoes use a generated ID while providers may later snapshot the
	// same user prompt under a native ID. Consume only an unmatched worker echo,
	// so identical prompts in later turns remain distinct transcript items.
	if !found && ev.Kind == "user_snapshot" {
		pending := m.pendingUser[ev.Text]
		if len(pending) > 0 {
			index, found = pending[0], true
			if len(pending) == 1 {
				delete(m.pendingUser, ev.Text)
			} else {
				m.pendingUser[ev.Text] = pending[1:]
			}
		}
	}
	if !found {
		id := key
		if id == "" {
			id = fmt.Sprintf("%s:%d", role, e.Sequence)
		}
		index = len(m.items)
		m.items = append(m.items, Item{ID: id, Kind: kind, Role: role})
		if key != "" {
			m.itemByID[key] = index
		}
		if ev.Kind == "user_message" {
			m.pendingUser[ev.Text] = append(m.pendingUser[ev.Text], index)
		}
	}
	if anonymousKey := anonymousDeltaKey(ev); anonymousKey != "" {
		m.hasAnonymous = true
		m.anonymousKey = anonymousKey
		m.anonymousIdx = index
	}
	item := &m.items[index]
	if item.Kind != "tool" {
		item.Kind = kind
	}
	if item.Role == "" {
		item.Role = role
	}
	if item.Title == "" {
		item.Title = ev.Title
	}
	if ev.Status != "" {
		item.Status = ev.Status
	}
	switch ev.Kind {
	case "message_snapshot", "user_snapshot", "reasoning_snapshot":
		if !strings.HasPrefix(ev.Kind, "user_") || item.Text == "" {
			item.Text = reconcileSnapshot(item.Text, ev.Text)
		}
	case "content":
		item.Details = cloneRaw(ev.Data)
		item.Text = appendSnapshot(item.Text, ev.Text)
	default:
		item.Text += ev.Text
	}
	if len(ev.Data) > 0 && ev.Kind != "content" {
		item.Details = cloneRaw(ev.Data)
	}
	return nil
}

func (m *Model) reduceTool(e storage.ConversationEvent) error {
	ev := e.Event
	key := "tool:" + ev.ToolCallID
	index, ok := m.itemByID[key]
	if !ok {
		index = len(m.items)
		m.itemByID[key] = index
		m.items = append(m.items, Item{ID: ev.ToolCallID, Kind: "tool", ToolCallID: ev.ToolCallID})
	}
	item := &m.items[index]
	if ev.Title != "" {
		item.Title = ev.Title
	}
	if ev.Kind == "tool_call" {
		if command := toolCommand(ev.Data, ev.ToolCallID); command != "" {
			item.Command = command
		}
	}
	if ev.Status != "" {
		item.Status = ev.Status
	}
	if ev.Kind == "tool_output_delta" {
		item.Text += ev.Text
	} else if ev.Kind == "tool_update" && item.Text == "" {
		// Claude's completed tool result is often a JSON string rather than an
		// output delta. Show its decoded text while retaining the original event.
		var output string
		if json.Unmarshal(ev.Data, &output) == nil {
			item.Text = output
		}
	}
	item.Details = appendToolDetail(item.Details, e)
	return nil
}

func toolCommand(data json.RawMessage, toolID string) string {
	var payload struct {
		Command  string `json:"command"`
		RawInput struct {
			Command string `json:"command"`
		} `json:"rawInput"`
		Item struct {
			Command string `json:"command"`
		} `json:"item"`
		Content []struct {
			Type  string `json:"type"`
			ID    string `json:"id"`
			Input struct {
				Command string `json:"command"`
			} `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return ""
	}
	if payload.Command != "" {
		return payload.Command
	}
	if payload.RawInput.Command != "" {
		return payload.RawInput.Command
	}
	if payload.Item.Command != "" {
		return payload.Item.Command
	}
	for _, block := range payload.Content {
		if block.Type == "tool_use" && block.ID == toolID {
			return block.Input.Command
		}
	}
	return ""
}

func anonymousDeltaKey(event conversation.Event) string {
	if event.MessageID != "" {
		return ""
	}
	switch event.Kind {
	case "text_delta":
		return "assistant:message"
	case "user_delta":
		return "user:message"
	case "reasoning_delta":
		return "assistant:reasoning"
	default:
		return ""
	}
}

func (m *Model) appendStandalone(e storage.ConversationEvent, kind, role string) {
	id := e.Event.MessageID
	if id == "" {
		id = e.ID
	}
	item := Item{ID: id, Kind: kind, Role: role, Text: e.Event.Text, Title: e.Event.Title, Status: e.Event.Status, ToolCallID: e.Event.ToolCallID, Details: cloneRaw(e.Event.Data)}
	m.itemByID[kind+":"+id] = len(m.items)
	m.items = append(m.items, item)
}

func (m *Model) retirePermission(requestID string) {
	if requestID == "" {
		m.permissions = nil
		return
	}
	for i := range m.permissions {
		if m.permissions[i].RequestID == requestID {
			m.permissions = append(m.permissions[:i], m.permissions[i+1:]...)
			return
		}
	}
}

func reconcileSnapshot(current, snapshot string) string {
	if current == "" || current == snapshot || strings.HasPrefix(snapshot, current) {
		return snapshot
	}
	if strings.HasPrefix(current, snapshot) {
		return current
	}
	return snapshot
}

func appendSnapshot(current, snapshot string) string {
	if strings.HasSuffix(current, snapshot) || strings.HasPrefix(snapshot, current) {
		return reconcileSnapshot(current, snapshot)
	}
	return current + snapshot
}

func appendToolDetail(existing json.RawMessage, e storage.ConversationEvent) json.RawMessage {
	detail, _ := json.Marshal(struct {
		Kind   string          `json:"kind"`
		Title  string          `json:"title,omitempty"`
		Status string          `json:"status,omitempty"`
		Text   string          `json:"text,omitempty"`
		Data   json.RawMessage `json:"data,omitempty"`
	}{e.Event.Kind, e.Event.Title, e.Event.Status, e.Event.Text, e.Event.Data})
	var details []json.RawMessage
	if json.Unmarshal(existing, &details) != nil || details == nil {
		details = []json.RawMessage{}
	}
	details = append(details, detail)
	out, _ := json.Marshal(details)
	return out
}

func sameEvent(a, b conversation.Event) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return bytes.Equal(ra, rb)
}

func cloneRaw(raw json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), raw...) }

func cloneEvent(event conversation.Event) conversation.Event {
	event.Data = cloneRaw(event.Data)
	event.Options = append([]conversation.PermissionOption(nil), event.Options...)
	return event
}
