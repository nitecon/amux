package chat

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/storage"
)

func loadFixture(t *testing.T, name string) []storage.ConversationEvent {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []storage.ConversationEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event storage.ConversationEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func applyAll(t *testing.T, m *Model, events []storage.ConversationEvent) {
	t.Helper()
	for _, event := range events {
		if err := m.Apply(event); err != nil {
			t.Fatalf("Apply(%d): %v", event.Sequence, err)
		}
	}
}

func TestACPFixtureToolGroupAndPermissionResolution(t *testing.T) {
	events := loadFixture(t, "acp-events.jsonl")
	m := New()
	m.SetDraft("keep this draft")
	applyAll(t, m, events[:7])
	snapshot := m.Snapshot()
	if snapshot.Sequence != 7 || snapshot.Draft != "keep this draft" || len(snapshot.Items) != 3 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	tool := snapshot.Items[2]
	if tool.Kind != "tool" || tool.Expanded || tool.Text != "found files" || tool.Status != "completed" || tool.Title != "Inspect files" {
		t.Fatalf("unexpected tool item: %+v", tool)
	}
	var details []json.RawMessage
	if err := json.Unmarshal(tool.Details, &details); err != nil || len(details) != 3 {
		t.Fatalf("tool details not retained: %s (%v)", tool.Details, err)
	}
	if len(snapshot.Permissions) != 1 || len(snapshot.Permissions[0].Options) != 1 {
		t.Fatalf("permission options missing: %+v", snapshot.Permissions)
	}
	if err := m.SetExpanded("tool-1", true); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(events[7]); err != nil {
		t.Fatal(err)
	}
	snapshot = m.Snapshot()
	if len(snapshot.Permissions) != 0 || !snapshot.Items[2].Expanded || snapshot.Draft != "keep this draft" {
		t.Fatalf("resolution lost model state: %+v", snapshot)
	}
}

func TestCodexFixtureReconcileReplayAndCopyIsolation(t *testing.T) {
	events := loadFixture(t, "codex-events.jsonl")
	m := New()
	applyAll(t, m, events)
	for _, event := range events {
		if err := m.Apply(event); err != nil {
			t.Fatalf("exact replay of %d: %v", event.Sequence, err)
		}
	}
	snapshot := m.Snapshot()
	if len(snapshot.Items) != 4 {
		t.Fatalf("expected one user, assistant, reasoning, and tool item; got %+v", snapshot.Items)
	}
	if snapshot.Items[0].Role != "user" || snapshot.Items[0].Text != "Explain this change" {
		t.Fatalf("user echo was duplicated or lost: %+v", snapshot.Items[0])
	}
	if snapshot.Items[1].Text != "The change updates chat." || snapshot.Items[1].Status != "completed" {
		t.Fatalf("snapshot did not reconcile deltas: %+v", snapshot.Items[1])
	}
	if snapshot.Items[2].Kind != "reasoning" || snapshot.Items[2].Text != "Reviewing behavior" {
		t.Fatalf("reasoning was not projected: %+v", snapshot.Items[2])
	}
	if len(snapshot.Permissions) != 1 || len(snapshot.Permissions[0].Options) != 2 {
		t.Fatalf("Codex permission options missing: %+v", snapshot.Permissions)
	}
	snapshot.Items[3].Details[0] = 'x'
	snapshot.Permissions[0].Options[0].Name = "mutated"
	snapshot.Permissions[0].Details[0] = 'x'
	if got := m.Snapshot(); got.Items[3].Details[0] == 'x' || got.Permissions[0].Options[0].Name == "mutated" || got.Permissions[0].Details[0] == 'x' {
		t.Fatal("snapshot exposed mutable model buffers")
	}
}

func TestApplyRejectsGapAndConflictingDuplicate(t *testing.T) {
	events := loadFixture(t, "acp-events.jsonl")
	m := New()
	if err := m.Apply(events[1]); !errors.Is(err, ErrSequenceGap) {
		t.Fatalf("gap error = %v", err)
	}
	if err := m.Apply(events[0]); err != nil {
		t.Fatal(err)
	}
	conflict := events[0]
	conflict.Event.Text = "different"
	if err := m.Apply(conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting duplicate error = %v", err)
	}
	if m.Snapshot().Sequence != 1 {
		t.Fatal("conflicting replay advanced sequence")
	}
}

func TestRepeatedUserPromptsRemainDistinct(t *testing.T) {
	m := New()
	events := []storage.ConversationEvent{
		{ID: "worker-1", ConversationID: "conversation", Sequence: 1, Event: conversation.Event{Kind: "user_message", SessionID: "session", MessageID: "prompt-1", Text: "same prompt"}},
		{ID: "native-1", ConversationID: "conversation", Sequence: 2, Event: conversation.Event{Kind: "user_snapshot", SessionID: "session", MessageID: "native-prompt-1", Text: "same prompt"}},
		{ID: "worker-2", ConversationID: "conversation", Sequence: 3, Event: conversation.Event{Kind: "user_message", SessionID: "session", MessageID: "prompt-2", Text: "same prompt"}},
		{ID: "native-2", ConversationID: "conversation", Sequence: 4, Event: conversation.Event{Kind: "user_snapshot", SessionID: "session", MessageID: "native-prompt-2", Text: "same prompt"}},
	}
	applyAll(t, m, events)
	snapshot := m.Snapshot()
	if len(snapshot.Items) != 2 || snapshot.Items[0].Text != "same prompt" || snapshot.Items[1].Text != "same prompt" {
		t.Fatalf("repeated prompts were collapsed or echoed: %+v", snapshot.Items)
	}
}

func TestACPAnonymousDeltasGroupUntilBoundaries(t *testing.T) {
	events := loadFixture(t, "acp-anonymous-deltas.jsonl")
	m := New()
	applyAll(t, m, events)
	snapshot := m.Snapshot()
	if len(snapshot.Items) != 4 {
		t.Fatalf("expected two grouped answers, a tool, and the next-turn answer: %+v", snapshot.Items)
	}
	if snapshot.Items[0].Kind != "message" || snapshot.Items[0].Role != "assistant" || snapshot.Items[0].Text != "First answer" {
		t.Fatalf("first anonymous message was not grouped: %+v", snapshot.Items[0])
	}
	if snapshot.Items[1].Kind != "tool" || snapshot.Items[2].Text != "Second answer" || snapshot.Items[3].Text != "Next turn" {
		t.Fatalf("tool/turn boundaries did not split anonymous chunks: %+v", snapshot.Items)
	}
}

func TestApplyRejectsCrossConversationAndSession(t *testing.T) {
	events := loadFixture(t, "acp-events.jsonl")
	m := New()
	if err := m.Apply(events[0]); err != nil {
		t.Fatal(err)
	}
	crossConversation := events[0]
	crossConversation.ConversationID = "other-conversation"
	if err := m.Apply(crossConversation); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-conversation replay was accepted: %v", err)
	}
	crossSession := events[1]
	crossSession.Event.SessionID = "other-session"
	if err := m.Apply(crossSession); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-session event was accepted: %v", err)
	}
	if m.Snapshot().Sequence != 1 {
		t.Fatal("rejected event changed sequence")
	}
}

func TestClearPermissionsDoesNotAdvanceSequenceAndHandoffRetiresReplay(t *testing.T) {
	events := loadFixture(t, "acp-events.jsonl")
	m := New()
	applyAll(t, m, events[:7])
	sequence := m.Snapshot().Sequence
	m.ClearPermissions()
	if got := m.Snapshot(); got.Sequence != sequence || len(got.Permissions) != 0 {
		t.Fatalf("live permission clear changed durable state: %+v", got)
	}
	handoff := storage.ConversationEvent{ID: "handoff-1", ConversationID: "conversation", Sequence: 8, Event: conversation.Event{Kind: "owner_handoff", SessionID: "session"}}
	if err := m.Apply(handoff); err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot(); got.Sequence != 8 || len(got.Permissions) != 0 {
		t.Fatalf("durable handoff did not retire permissions: %+v", got)
	}
}

func TestSnapshotAndDraftSurviveReplayAndNewEvents(t *testing.T) {
	events := loadFixture(t, "codex-events.jsonl")
	m := New()
	applyAll(t, m, events[:7])
	if err := m.SetExpanded("item-1", true); err != nil {
		t.Fatal(err)
	}
	m.SetDraft("unsent")
	applyAll(t, m, events[7:])
	for _, event := range events {
		if err := m.Apply(event); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := m.Snapshot()
	if !snapshot.Items[3].Expanded || snapshot.Draft != "unsent" {
		t.Fatalf("replay reset local UI state: %+v", snapshot)
	}
	options := snapshot.Permissions[0].Options
	options[0] = conversation.PermissionOption{ID: "changed"}
	if m.Snapshot().Permissions[0].Options[0].ID != "accept" {
		t.Fatal("permission options share backing storage")
	}
}
