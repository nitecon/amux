package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/identity/agent"
)

func TestTaskConversationDurableReplayAndRetry(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	task := makeTask(t, s, nil)
	actor := taskActor()
	c, err := s.CreateConversation(ctx, "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if c.TaskID != task.ID || c.Provider != "codex" || c.NativeSessionID != "" {
		t.Fatalf("binding %+v", c)
	}
	if _, err = s.BindConversationSession(ctx, "tasks", c.ID, "native-session", actor); err != nil {
		t.Fatal(err)
	}
	event := conversation.Event{Kind: "text_delta", SessionID: "native-session", MessageID: "message-1", Text: "hello"}
	id := uuid.NewString()
	wake := s.ConversationChanges()
	first, err := s.AppendConversationEvent(ctx, "tasks", c.ID, id, event, actor)
	if err != nil || first.Sequence != 1 {
		t.Fatalf("append %+v %v", first, err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("commit did not wake replay")
	}
	repeated, err := s.AppendConversationEvent(ctx, "tasks", c.ID, id, event, actor)
	if err != nil || repeated.Sequence != first.Sequence {
		t.Fatalf("retry %+v %v", repeated, err)
	}
	event.Text = "different payload"
	if _, err = s.AppendConversationEvent(ctx, "tasks", c.ID, id, event, actor); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	event.Text = "world"
	second, err := s.AppendConversationEvent(ctx, "tasks", c.ID, uuid.NewString(), event, actor)
	if err != nil || second.Sequence != 2 {
		t.Fatalf("sequence after retry %+v %v", second, err)
	}
	replay, err := s.ConversationEvents(ctx, "tasks", c.ID, 1, 100)
	if err != nil || len(replay) != 1 || replay[0].Event.Text != "world" {
		t.Fatalf("replay %+v %v", replay, err)
	}
	// A separate connection sees only committed data and the exact native binding.
	var path string
	rows, err := s.db.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var seq int
		var name string
		if err := rows.Scan(&seq, &name, &path); err != nil {
			t.Fatal(err)
		}
		if name == "main" {
			break
		}
	}
	rows.Close()
	reopened, err := Open(ctx, filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.Conversation(ctx, "tasks", c.ID)
	if err != nil || loaded.NativeSessionID != "native-session" || loaded.TaskID != task.ID {
		t.Fatalf("reload %+v %v", loaded, err)
	}
	replay, err = reopened.ConversationEvents(ctx, "tasks", c.ID, 0, 100)
	if err != nil || len(replay) != 2 {
		t.Fatalf("durable replay %+v %v", replay, err)
	}
}

func TestCreateConversationWithGoalValidatesBeforeClaimAndCreatesAtomically(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	goal := "ship"
	task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Scoped initial claim", Reporter: "fixture", Goal: &goal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := taskActor()
	for _, selected := range []*string{nil, ptr("Ship")} {
		if _, err = s.CreateConversationWithGoal(ctx, "tasks", task.ID, actor, selected); !errors.Is(err, ErrTaskConflict) {
			t.Fatalf("selection %v returned %v", selected, err)
		}
		detail, readErr := s.Task(ctx, "tasks", task.ID)
		var conversations int
		countErr := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_conversations WHERE task_id=?", task.ID).Scan(&conversations)
		if readErr != nil || detail.Task.Status != "todo" || detail.Task.OwnerOrigin != nil || countErr != nil || conversations != 0 {
			t.Fatalf("rejected selection changed state: task=%+v conversations=%d read=%v count=%v", detail, conversations, readErr, countErr)
		}
	}
	selected := " ship "
	conversation, err := s.CreateConversationWithGoal(ctx, "tasks", task.ID, actor, &selected)
	if err != nil || conversation == nil {
		t.Fatalf("matching selection: conversation=%+v err=%v", conversation, err)
	}
	detail, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || detail.Task.Status != "in_progress" || detail.Task.OwnerOrigin == nil || *detail.Task.OwnerOrigin != *actor {
		t.Fatalf("initial claim not committed with conversation: task=%+v err=%v", detail, err)
	}
}

func TestCreateConversationWithGoalChecksAlreadyOwnedTaggedTask(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	actor := taskActor()
	goal := "review"
	task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Already owned", Reporter: "fixture", Goal: &goal}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.TransitionTask(ctx, "tasks", task.ID, "in_progress", actor); err != nil {
		t.Fatal(err)
	}
	for _, selected := range []*string{nil, ptr("other")} {
		if _, err = s.CreateConversationWithGoal(ctx, "tasks", task.ID, actor, selected); !errors.Is(err, ErrTaskConflict) {
			t.Fatalf("already-owned selection %v returned %v", selected, err)
		}
	}
	var conversations int
	if err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_conversations WHERE task_id=?", task.ID).Scan(&conversations); err != nil || conversations != 0 {
		t.Fatalf("rejected already-owned scope created conversation count=%d err=%v", conversations, err)
	}
	selected := " review "
	if _, err = s.CreateConversationWithGoal(ctx, "tasks", task.ID, actor, &selected); err != nil {
		t.Fatalf("matching already-owned scope: %v", err)
	}
}

func TestCreateConversationWithGoalLeavesReviewTaskQueuedForPeerSelection(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	goal := "review"
	task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Review selection", Reporter: "fixture", Labels: []string{"task-review"}, Goal: &goal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := taskActor()
	wrong := "other"
	if _, err = s.CreateConversationWithGoal(ctx, "tasks", task.ID, actor, &wrong); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("review task accepted mismatched goal: %v", err)
	}
	selected := " review "
	c, err := s.CreateConversationWithGoal(ctx, "tasks", task.ID, actor, &selected)
	if err != nil || c == nil {
		t.Fatalf("matching review conversation=%+v err=%v", c, err)
	}
	detail, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || detail.Task.Status != "todo" || detail.Task.OwnerOrigin != nil {
		t.Fatalf("conversation bypassed review peer selection: task=%+v err=%v", detail, err)
	}
}

func TestConversationOwnerFenceAndExactResumeBinding(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	task := makeTask(t, s, nil)
	actor := taskActor()
	c, err := s.CreateConversation(ctx, "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindConversationSession(ctx, "tasks", c.ID, "original", actor); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindConversationSession(ctx, "tasks", c.ID, "replacement", actor); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("silently replaced native session: %v", err)
	}
	successor := *actor
	successor.InstanceID = uuid.NewString()
	if _, err = s.ResumeConversation(ctx, "tasks", c.ID, actor.InstanceID, &successor); err != nil {
		t.Fatal(err)
	}
	event := conversation.Event{Kind: "text_delta", SessionID: "original", Text: "stale"}
	if _, err = s.AppendConversationEvent(ctx, "tasks", c.ID, uuid.NewString(), event, actor); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("stale instance wrote transcript: %v", err)
	}
	if _, err = s.BindConversationSession(ctx, "tasks", c.ID, "original", actor); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("stale instance rebound transcript: %v", err)
	}
	other := successor
	other.InstanceID = uuid.NewString()
	if _, err = s.ResumeConversation(ctx, "tasks", c.ID, actor.InstanceID, &other); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("stale resume won: %v", err)
	}
	event.SessionID = "unrelated"
	if _, err = s.AppendConversationEvent(ctx, "tasks", c.ID, uuid.NewString(), event, &successor); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("foreign native session appended: %v", err)
	}
	event.SessionID = "original"
	if _, err = s.AppendConversationEvent(ctx, "tasks", c.ID, uuid.NewString(), event, &successor); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Conversation(ctx, "another-project", c.ID); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("cross-project lookup: %v", err)
	}
	if _, err = s.CreateConversation(ctx, "another-project", task.ID, actor); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("cross-project task link: %v", err)
	}
}

func TestResumeConversationPersistsOwnerHandoffAtomically(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	task := makeTask(t, s, nil)
	previous := taskActor()
	c, err := s.CreateConversation(ctx, "tasks", task.ID, previous)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindConversationSession(ctx, "tasks", c.ID, "native-session", previous); err != nil {
		t.Fatal(err)
	}
	initial, err := s.AppendConversationEvent(ctx, "tasks", c.ID, uuid.NewString(), conversation.Event{Kind: "text_delta", SessionID: "native-session", Text: "before handoff"}, previous)
	if err != nil || initial.Sequence != 1 {
		t.Fatalf("initial event %+v: %v", initial, err)
	}
	next := *previous
	next.InstanceID = uuid.NewString()
	wake := s.ConversationChanges()
	if _, err = s.ResumeConversation(ctx, "tasks", c.ID, previous.InstanceID, &next); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("committed handoff did not wake conversation replay")
	}
	replay, err := s.ConversationEvents(ctx, "tasks", c.ID, 0, 100)
	if err != nil || len(replay) != 2 {
		t.Fatalf("handoff replay %+v: %v", replay, err)
	}
	handoff := replay[1]
	if _, err := uuid.Parse(handoff.ID); err != nil || handoff.Sequence != 2 || handoff.Event.Kind != "owner_handoff" || handoff.Event.SessionID != "native-session" {
		t.Fatalf("invalid handoff event %+v: %v", handoff, err)
	}
	var data struct {
		NativeSessionID string       `json:"native_session_id"`
		InstanceID      string       `json:"instance_id"`
		PreviousOrigin  agent.Origin `json:"previous_origin"`
		CurrentOrigin   agent.Origin `json:"current_origin"`
	}
	if err = json.Unmarshal(handoff.Event.Data, &data); err != nil || data.NativeSessionID != "native-session" || data.InstanceID != next.InstanceID || data.PreviousOrigin != *previous || data.CurrentOrigin != next {
		t.Fatalf("handoff origin data %+v: %v", data, err)
	}
	var recordedInstance string
	if err = s.db.QueryRowContext(ctx, "SELECT instance_id FROM amux_conversation_events WHERE conversation_id=? AND sequence=2", c.ID).Scan(&recordedInstance); err != nil || recordedInstance != next.InstanceID {
		t.Fatalf("handoff event instance %q: %v", recordedInstance, err)
	}
	// The new owner can retry its resume call without adding another boundary.
	if _, err = s.ResumeConversation(ctx, "tasks", c.ID, "stale-previous-instance", &next); err != nil {
		t.Fatalf("same-owner retry: %v", err)
	}
	replay, err = s.ConversationEvents(ctx, "tasks", c.ID, 0, 100)
	if err != nil || len(replay) != 2 {
		t.Fatalf("same-owner retry duplicated handoff: %d events, %v", len(replay), err)
	}

	unbound, err := s.CreateConversation(ctx, "tasks", task.ID, previous)
	if err != nil {
		t.Fatal(err)
	}
	unboundNext := *previous
	unboundNext.InstanceID = uuid.NewString()
	if _, err = s.ResumeConversation(ctx, "tasks", unbound.ID, previous.InstanceID, &unboundNext); err != nil {
		t.Fatal(err)
	}
	replay, err = s.ConversationEvents(ctx, "tasks", unbound.ID, 0, 100)
	if err != nil || len(replay) != 0 {
		t.Fatalf("unbound takeover emitted handoff: %+v, %v", replay, err)
	}
}

func TestResumeConversationRecoversExactTaskOwner(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	previous := taskActor()
	task := makeTask(t, s, previous)
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='adhoc' WHERE ident='tasks'"); err != nil {
		t.Fatal(err)
	}
	display := "Routing identity fixture"
	target, err := s.RegisterTarget(ctx, "tasks", TargetRegistration{Origin: *previous, Generation: previous.InstanceID, DisplayName: &display}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AssignTaskFromUser(ctx, "tasks", task.ID, target.Target, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", previous); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateConversation(ctx, "tasks", task.ID, previous)
	if err != nil {
		t.Fatal(err)
	}
	next := *previous
	next.InstanceID = uuid.NewString()
	wake := s.TaskEventChanges()
	if _, err = s.ResumeConversation(ctx, "tasks", c.ID, previous.InstanceID, &next); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("committed task owner recovery did not wake task events")
	}
	recovered, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || recovered.Task.OwnerOrigin == nil || *recovered.Task.OwnerOrigin != next {
		t.Fatalf("recovered task owner %+v: %v", recovered, err)
	}
	var route TaskRouting
	if err := json.Unmarshal(recovered.Task.Routing, &route); err != nil || route.Target == nil || route.Target.Origin != next || route.Target.Generation != next.InstanceID {
		t.Fatalf("recovered routing does not match task owner: %s, %v", recovered.Task.Routing, err)
	}
	registered, err := s.Targets(ctx, "tasks", time.Now())
	if err != nil || len(registered) != 1 || registered[0].Target != *route.Target || registered[0].DisplayName == nil || *registered[0].DisplayName != display {
		t.Fatalf("target directory did not replace predecessor: %+v, %v", registered, err)
	}
	// The winner may retry, but cannot transfer the task owner a second time.
	third := next
	third.InstanceID = uuid.NewString()
	if _, err = s.ResumeConversation(ctx, "tasks", c.ID, previous.InstanceID, &next); err != nil {
		t.Fatalf("winning instance retry: %v", err)
	}
	if _, err = s.ResumeConversation(ctx, "tasks", c.ID, next.InstanceID, &third); err != nil {
		t.Fatal(err)
	}
	recovered, err = s.Task(ctx, "tasks", task.ID)
	if err != nil || recovered.Task.OwnerOrigin == nil || *recovered.Task.OwnerOrigin != third {
		t.Fatalf("second exact handoff owner %+v: %v", recovered, err)
	}
}

func TestResumeConversationLeavesForeignTaskOwnerUnchanged(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	foreign := taskActor()
	task := makeTask(t, s, foreign)
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", foreign); err != nil {
		t.Fatal(err)
	}
	conversationOwner := *foreign
	conversationOwner.InstanceID = uuid.NewString()
	c, err := s.CreateConversation(ctx, "tasks", task.ID, &conversationOwner)
	if err != nil {
		t.Fatal(err)
	}
	next := conversationOwner
	next.InstanceID = uuid.NewString()
	if _, err = s.ResumeConversation(ctx, "tasks", c.ID, conversationOwner.InstanceID, &next); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || loaded.Task.OwnerOrigin == nil || *loaded.Task.OwnerOrigin != *foreign {
		t.Fatalf("foreign task owner changed: %+v, %v", loaded, err)
	}
}

func TestResumeConversationDoesNotResurrectTerminalTask(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	previous := taskActor()
	for _, status := range []string{"done", "cancelled"} {
		task := makeTask(t, s, previous)
		if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", previous); err != nil {
			t.Fatal(err)
		}
		c, err := s.CreateConversation(ctx, "tasks", task.ID, previous)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(ctx, "UPDATE tasks SET status=? WHERE id=?", status, task.ID); err != nil {
			t.Fatal(err)
		}
		next := *previous
		next.InstanceID = uuid.NewString()
		if _, err = s.ResumeConversation(ctx, "tasks", c.ID, previous.InstanceID, &next); err != nil {
			t.Fatal(err)
		}
		detail, err := s.Task(ctx, "tasks", task.ID)
		if err != nil || detail.Task.Status != status {
			t.Fatalf("resume changed terminal task %s to %+v: %v", status, detail, err)
		}
	}
}

func TestConversationReplayHasByteBoundAndStableSequences(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	task := makeTask(t, s, nil)
	actor := taskActor()
	c, err := s.CreateConversation(ctx, "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindConversationSession(ctx, "tasks", c.ID, "native", actor); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("x", 3<<20)
	for i := 0; i < 2; i++ {
		if _, err = s.AppendConversationEvent(ctx, "tasks", c.ID, uuid.NewString(), conversation.Event{Kind: "text_delta", SessionID: "native", Text: text}, actor); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.ConversationEvents(ctx, "tasks", c.ID, 0, 1000)
	if err != nil || len(page) != 1 || page[0].Sequence != 1 {
		t.Fatalf("first page length %d: %v", len(page), err)
	}
	page, err = s.ConversationEvents(ctx, "tasks", c.ID, 1, 1000)
	if err != nil || len(page) != 1 || page[0].Sequence != 2 {
		t.Fatalf("next page length %d: %v", len(page), err)
	}
}

func TestConversationConcurrentDuplicateHasOneSequence(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	task := makeTask(t, s, nil)
	actor := taskActor()
	c, err := s.CreateConversation(ctx, "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindConversationSession(ctx, "tasks", c.ID, "native", actor); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			event, err := s.AppendConversationEvent(ctx, "tasks", c.ID, id, conversation.Event{Kind: "text_delta", SessionID: "native", Text: "once"}, actor)
			if err == nil && event.Sequence != 1 {
				err = fmt.Errorf("duplicate sequence %d", event.Sequence)
			}
			results <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.ConversationEvents(ctx, "tasks", c.ID, 0, 100)
	if err != nil || len(events) != 1 {
		t.Fatalf("duplicate count %d: %v", len(events), err)
	}
}

func TestTaskConversationsStaySeparateAndPageWithoutRepeats(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	task := makeTask(t, s, nil)
	other := makeTask(t, s, nil)
	for _, id := range []string{task.ID, task.ID, other.ID} {
		if _, err := s.CreateConversation(ctx, "tasks", id, taskActor()); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.TaskConversations(ctx, "tasks", task.ID, "", 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first page %+v %v", first, err)
	}
	second, err := s.TaskConversations(ctx, "tasks", task.ID, first[0].ID, 1)
	if err != nil || len(second) != 1 || second[0].ID == first[0].ID || second[0].TaskID != task.ID {
		t.Fatalf("second page %+v %v", second, err)
	}
	last, err := s.TaskConversations(ctx, "tasks", task.ID, second[0].ID, 1)
	if err != nil || len(last) != 0 {
		t.Fatalf("last page %+v %v", last, err)
	}
}

func TestTaskConversationsCarryOwnerReportedHostname(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	task := makeTask(t, s, nil)
	if _, err := s.CreateConversation(ctx, "tasks", task.ID, taskActor()); err != nil {
		t.Fatal(err)
	}
	items, err := s.TaskConversations(ctx, "tasks", task.ID, "", 10)
	if err != nil || len(items) != 1 || items[0].OwnerHostname != nil || items[0].FleetHostname != nil {
		t.Fatalf("unknown host %+v %v", items, err)
	}
	host := "Defcon2"
	if accepted, err := s.ReportPresence(ctx, PresenceReport{
		Source: "fixture-pool", Sequence: 1, Hostname: &host,
		Agents: []PresenceAgent{{ProjectIdent: "tasks", Origin: *taskActor()}},
	}, time.Now().UTC()); err != nil || !accepted {
		t.Fatalf("presence accepted %v, %v", accepted, err)
	}
	items, err = s.TaskConversations(ctx, "tasks", task.ID, "", 10)
	if err != nil || len(items) != 1 || items[0].OwnerHostname == nil || *items[0].OwnerHostname != host || items[0].FleetHostname != nil {
		t.Fatalf("reported host %+v %v", items, err)
	}
}
