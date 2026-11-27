package storage

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
)

func TestTaskProgressTwoPingsRequeueAndAssignAnotherAgent(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	registry := reminderTestRegistry(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	owner := assignmentOrigin(uuid.NewString(), uuid.NewString(), "codex", "linux")
	other := assignmentOrigin(uuid.NewString(), uuid.NewString(), "claude", "linux")
	task := reminderInProgressTask(t, s, &owner, now.UnixMilli())
	conv, err := s.CreateConversation(ctx, "tasks", task.ID, &owner)
	if err != nil {
		t.Fatal(err)
	}
	command, err := s.EnqueueConversationCommand(ctx, "tasks", conv.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: "Work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimConversationCommand(ctx, "tasks", conv.ID, command.ID, &owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE tasks SET updated_at=? WHERE id=?", now.UnixMilli(), task.ID); err != nil {
		t.Fatal(err)
	}
	for i, offset := range []time.Duration{5*time.Minute - time.Millisecond, 5 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute} {
		at := now.Add(offset)
		// A fresh heartbeat and a permanent busy flag are not progress.
		agent := idleAssignmentAgent("tasks", owner, at)
		agent.LastActivityAt = ptrI64(now.UnixMilli())
		agent.Busy = true
		reportAssignmentPresence(t, s, at, "stalled", int64(i+1), agent)
		event := reminderClock(at)
		if err := s.ReconcileRouterProgress(ctx, registry, event); err != nil {
			t.Fatal(err)
		}
		if err := s.ReconcileRouterProgress(ctx, registry, event); err != nil {
			t.Fatal(err)
		}
		wantStatus, wantPings := "stalled", 1
		if i == 0 {
			wantStatus, wantPings = "in_progress", 0
		}
		if i == 3 {
			wantPings = 2
		}
		if i == 4 {
			wantStatus, wantPings = "todo", 2
		}
		assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", wantStatus, task.ID)
		if count := reminderNoticeCount(t, s); count != wantPings {
			t.Fatalf("step %d: pings %d want %d", i, count, wantPings)
		}
	}
	assertReclaimValue(t, s, "SELECT COUNT(*) FROM task_revoked_conversations WHERE conversation_id=?", int64(1), conv.ID)
	if _, err := s.TransitionTask(WithConversationFence(ctx, "tasks", conv.ID), "tasks", task.ID, "in_progress", &owner); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("revoked task mutation: %v", err)
	}
	if _, err := s.ResumeConversation(ctx, "tasks", conv.ID, owner.InstanceID, &owner); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("revoked conversation resumed: %v", err)
	}
	if _, err := s.ClaimConversationCommand(ctx, "tasks", conv.ID, command.ID, &owner); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("revoked command claimed: %v", err)
	}
	// Existing command outcomes remain flushable after stopping the old process.
	if _, err := s.FinishConversationCommand(ctx, "tasks", conv.ID, command.ID, "interrupted", &owner); err != nil {
		t.Fatal(err)
	}
	at := now.Add(15 * time.Minute)
	rotated := owner
	rotated.InstanceID = uuid.NewString()
	reportAssignmentPresence(t, s, at, "stalled", 6, idleAssignmentAgent("tasks", rotated, at), idleAssignmentAgent("tasks", other, at))
	if err := s.ReconcileAssignments(ctx, at); err != nil {
		t.Fatal(err)
	}
	detail, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || detail.Task.OwnerOrigin == nil || *detail.Task.OwnerOrigin != other || detail.Task.Status != "in_progress" {
		t.Fatalf("reassignment: %+v %v", detail, err)
	}
}

func TestTaskProgressQueuedReservationNeverBecomesStartedFromOwnerOutput(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	owner := assignmentOrigin(uuid.NewString(), uuid.NewString(), "codex", "linux")
	task := makeTask(t, s, &owner)
	if _, err := s.db.Exec("UPDATE tasks SET updated_at=? WHERE id=?", now.UnixMilli(), task.ID); err != nil {
		t.Fatal(err)
	}
	for i, offset := range []time.Duration{5 * time.Minute, 6 * time.Minute, 10 * time.Minute, 15 * time.Minute} {
		at := now.Add(offset)
		agent := idleAssignmentAgent("tasks", owner, at)
		agent.LastActivityAt = ptrI64(at.UnixMilli()) // output on unrelated work
		reportAssignmentPresence(t, s, at, "reserved", int64(i+1), agent)
		if err := s.ReconcileRouterProgress(ctx, reminderTestRegistry(t), reminderClock(at)); err != nil {
			t.Fatal(err)
		}
		assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", "todo", task.ID)
		assertReclaimValue(t, s, "SELECT started_at FROM tasks WHERE id=?", nil, task.ID)
		want := 1
		if i >= 2 {
			want = 2
		}
		if count := reminderNoticeCount(t, s); count != want {
			t.Fatalf("pings=%d want %d", count, want)
		}
	}
	assertReclaimValue(t, s, "SELECT owner_origin_json FROM tasks WHERE id=?", nil, task.ID)
	assertReclaimValue(t, s, "SELECT COUNT(*) FROM task_recovery_sessions WHERE task_id=? AND session_id=?", int64(1), task.ID, owner.SessionID)
}

func TestTaskProgressLegacyClaimsUseTheSameTwoPingRecovery(t *testing.T) {
	for _, kind := range []string{"unowned", "legacy-id", "partial-origin"} {
		t.Run(kind, func(t *testing.T) {
			s := taskStore(t)
			now := time.Now().UTC().Truncate(time.Millisecond)
			task := makeTask(t, s, nil)
			var owner, origin any
			if kind == "legacy-id" {
				owner = "legacy"
			}
			if kind == "partial-origin" {
				origin = `{"session_id":"legacy","instance_id":"old"}`
			}
			if _, err := s.db.Exec("UPDATE tasks SET status='in_progress',updated_at=?,owner_agent_id=?,owner_origin_json=? WHERE id=?", now.UnixMilli(), owner, origin, task.ID); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 3; i++ {
				event := reminderClock(now.Add(time.Duration(i) * 5 * time.Minute))
				if err := s.ReconcileRouterProgress(context.Background(), reminderTestRegistry(t), event); err != nil {
					t.Fatal(err)
				}
				if err := s.ReconcileRouterProgress(context.Background(), reminderTestRegistry(t), event); err != nil {
					t.Fatal(err)
				}
				want := "stalled"
				if i == 3 {
					want = "todo"
				}
				assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", want, task.ID)
				assertReclaimValue(t, s, "SELECT COUNT(*) FROM task_comments WHERE task_id=?", int64(i), task.ID)
			}
		})
	}
}

func TestTaskProgressIgnoresForeignExecutionAndPreservesDispatchAuthority(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.Now()
	owner := assignmentOrigin(uuid.NewString(), uuid.NewString(), "codex", "linux")
	task := reminderInProgressTask(t, s, &owner, now.Add(-5*time.Minute).UnixMilli())
	other := owner
	other.InstanceID = uuid.NewString()
	agent := idleAssignmentAgent("tasks", other, now)
	agent.LastActivityAt = ptrI64(now.UnixMilli())
	reportAssignmentPresence(t, s, now, "foreign", 1, agent)
	if err := s.ReconcileRouterProgress(ctx, reminderTestRegistry(t), reminderClock(now)); err != nil {
		t.Fatal(err)
	}
	assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", "stalled", task.ID)
	if _, err := s.db.Exec("CREATE TABLE task_dispatches(task_id TEXT PRIMARY KEY); INSERT INTO task_dispatches VALUES(?)", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterProgress(ctx, reminderTestRegistry(t), reminderClock(now.Add(20*time.Minute))); err != nil {
		t.Fatal(err)
	}
	assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", "stalled", task.ID)
	assertReclaimValue(t, s, "SELECT ping_count FROM task_progress WHERE task_id=?", int64(1), task.ID)
}

func TestTaskProgressHonorsDisabledSettingAndRejectsInvalidTicks(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	owner := assignmentOrigin(uuid.NewString(), uuid.NewString(), "codex", "linux")
	task := reminderInProgressTask(t, s, &owner, 0)
	if _, err := s.db.Exec("INSERT INTO settings(key,value) VALUES('plugin.reclaim.enabled','false')"); err != nil {
		t.Fatal(err)
	}
	event := reminderClock(time.Now())
	if err := s.ReconcileRouterProgress(ctx, reminderTestRegistry(t), event); err != nil {
		t.Fatal(err)
	}
	assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", "in_progress", task.ID)
	event.UID = "invalid"
	if err := s.ReconcileRouterProgress(ctx, reminderTestRegistry(t), event); err == nil {
		t.Fatal("invalid tick accepted")
	}
}

func TestTaskProgressPingCountersSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "progress.sqlite")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, fixtureProject("tasks")); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	owner := assignmentOrigin(uuid.NewString(), uuid.NewString(), "codex", "linux")
	task := reminderInProgressTask(t, s, &owner, now.Add(-5*time.Minute).UnixMilli())
	if err := s.ReconcileRouterProgress(ctx, reminderTestRegistry(t), reminderClock(now)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ReconcileRouterProgress(ctx, reminderTestRegistry(t), reminderClock(now.Add(5*time.Minute))); err != nil {
		t.Fatal(err)
	}
	assertReclaimValue(t, s, "SELECT ping_count FROM task_progress WHERE task_id=?", int64(2), task.ID)
	if err := s.ReconcileRouterProgress(ctx, reminderTestRegistry(t), reminderClock(now.Add(10*time.Minute))); err != nil {
		t.Fatal(err)
	}
	assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", "todo", task.ID)
	if count := reminderNoticeCount(t, s); count != 2 {
		t.Fatalf("restart duplicated pings: %d", count)
	}
}

func TestTaskProgressOwnerCommentResumesBeforeSubmission(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.Now()
	owner := assignmentOrigin(uuid.NewString(), uuid.NewString(), "codex", "linux")
	task := reminderInProgressTask(t, s, &owner, now.Add(-5*time.Minute).UnixMilli())
	if err := s.ReconcileRouterProgress(ctx, reminderTestRegistry(t), reminderClock(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskComment(ctx, "tasks", task.ID, owner.SessionID, "agent", "Submitted changes for review", nil, &owner); err != nil {
		t.Fatal(err)
	}
	assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", "in_progress", task.ID)
	status, action := "in_progress", "Review the submitted changes"
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "user_review", ExpectedStatus: &status, ReviewAction: &action, Changeset: json.RawMessage(`null`)}, &owner); err != nil {
		t.Fatalf("submission from recovered stalled work failed: %v", err)
	}
}

func TestTaskProgressProviderToolActivityResumesAndResetsPings(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	registry := reminderTestRegistry(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	owner := assignmentOrigin(uuid.NewString(), uuid.NewString(), "codex", "linux")
	task := reminderInProgressTask(t, s, &owner, now.Add(-5*time.Minute).UnixMilli())
	conv, err := s.CreateConversation(ctx, "tasks", task.ID, &owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindConversationSession(ctx, "tasks", conv.ID, "native", &owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE tasks SET updated_at=? WHERE id=?", now.Add(-5*time.Minute).UnixMilli(), task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterProgress(ctx, registry, reminderClock(now)); err != nil {
		t.Fatal(err)
	}
	assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", "stalled", task.ID)
	eventID := uuid.NewString()
	if _, err := s.AppendConversationEvent(ctx, "tasks", conv.ID, eventID, conversation.Event{Kind: "tool_call", SessionID: "native", ToolCallID: "read"}, &owner); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterProgress(ctx, registry, reminderClock(now.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", "in_progress", task.ID)
	assertReclaimValue(t, s, "SELECT ping_count FROM task_progress WHERE task_id=?", int64(0), task.ID)
	// Replaying the same event never renews the server receipt timestamp.
	var received int64
	if err := s.db.QueryRow("SELECT created_at FROM amux_conversation_events WHERE event_id=?", eventID).Scan(&received); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendConversationEvent(ctx, "tasks", conv.ID, eventID, conversation.Event{Kind: "tool_call", SessionID: "native", ToolCallID: "read"}, &owner); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterProgress(ctx, registry, reminderClock(time.UnixMilli(received+taskQuietMS))); err != nil {
		t.Fatal(err)
	}
	assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", "stalled", task.ID)
	assertReclaimValue(t, s, "SELECT ping_count FROM task_progress WHERE task_id=?", int64(1), task.ID)
}

func TestTaskProgressNotificationFailureRollsBackStatusAndCounters(t *testing.T) {
	s := taskStore(t)
	now := time.Now()
	owner := assignmentOrigin(uuid.NewString(), uuid.NewString(), "codex", "linux")
	task := reminderInProgressTask(t, s, &owner, now.Add(-5*time.Minute).UnixMilli())
	if _, err := s.db.Exec(`CREATE TRIGGER fail_progress_notification BEFORE INSERT ON router_events WHEN NEW.id='agent.notify' BEGIN SELECT RAISE(ABORT,'injected progress failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterProgress(context.Background(), reminderTestRegistry(t), reminderClock(now)); err == nil {
		t.Fatal("expected failure")
	}
	assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", "in_progress", task.ID)
	assertReclaimValue(t, s, "SELECT COUNT(*) FROM task_progress WHERE task_id=?", int64(0), task.ID)
}

func TestAssignmentStartsSessionReservedQueueWithIdleCapacity(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.Now()
	owner := assignmentOrigin(uuid.NewString(), uuid.NewString(), "codex", "linux")
	task := makeTask(t, s, nil)
	encoded, _ := json.Marshal(owner)
	routing, _ := json.Marshal(TaskRouting{Mode: "session", Target: &RegisteredTargetIdentity{Origin: owner, Generation: owner.InstanceID}})
	if _, err := s.db.Exec("UPDATE tasks SET owner_agent_id=?,owner_origin_json=?,routing_json=? WHERE id=?", owner.SessionID, string(encoded), string(routing), task.ID); err != nil {
		t.Fatal(err)
	}
	reportAssignmentPresence(t, s, now, "ready", 1, idleAssignmentAgent("tasks", owner, now))
	if err := s.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", "in_progress", task.ID)
}
