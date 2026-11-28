package storage

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/identity/agent"
)

func reminderTestRegistry(t *testing.T) *fism.Registry {
	t.Helper()
	definitions := append(RouterPresenceEventDefinitions(), controllers.ClockDefinition(), controllers.ClientHeartbeatDefinition())
	definitions = append(definitions, controllers.NotificationEventDefinitions()...)
	observer := fism.ControllerDefinition{ID: "reminder-test-observer", Subscriptions: []fism.Subscription{
		{Event: controllers.ClientHeartbeatID, MinVersion: 1, MaxVersion: 1},
		{Event: controllers.NotifyEventID, MinVersion: 1, MaxVersion: 1},
	}}
	registry, err := fism.NewRegistry(definitions, []fism.ControllerDefinition{observer})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func reminderOwner(session, instance string) *agent.Origin {
	return &agent.Origin{SessionID: session, InstanceID: instance, Provider: "codex", OS: "macos"}
}

func reminderInProgressTask(t *testing.T, s *Store, owner *agent.Origin, updatedAt int64) *Task {
	t.Helper()
	task, err := s.InsertTask(context.Background(), TaskInsert{Project: "tasks", Title: "Reminder work", Reporter: "fixture"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE tasks SET status='in_progress',updated_at=? WHERE id=?", updatedAt, task.ID); err != nil {
		t.Fatal(err)
	}
	return task
}

func reminderHeartbeat(t *testing.T, ctx context.Context, s *Store, registry *fism.Registry, source string, sequence int64, run string, agents []PresenceAgent, at time.Time) fism.Event {
	t.Helper()
	accepted, err := s.ReportPresence(ctx, PresenceReport{Source: source, Sequence: sequence, ClientRun: &run, Agents: agents}, at)
	if err != nil || !accepted {
		t.Fatalf("report native presence seq=%d accepted=%v err=%v", sequence, accepted, err)
	}
	if n, err := s.DrainRouterHeartbeatOutbox(ctx, registry, 20); err != nil || n != 1 {
		t.Fatalf("drain native heartbeat seq=%d n=%d err=%v", sequence, n, err)
	}
	var event fism.Event
	var payload []byte
	if err := s.db.QueryRowContext(ctx, `SELECT seq,uid,id,version,payload,source,emitted_at,correlation FROM router_events
		WHERE id='client.heartbeat' AND source=? AND json_extract(payload,'$.report_sequence')=?`, source, sequence).Scan(
		&event.Seq, &event.UID, &event.ID, &event.Version, &payload, &event.Source, &event.EmittedAt, &event.Correlation); err != nil {
		t.Fatalf("load captured heartbeat: %v", err)
	}
	event.Payload = append(json.RawMessage(nil), payload...)
	return event
}

func reminderClock(at time.Time) fism.Event {
	return fism.Event{UID: uuid.NewString(), ID: "clock.tick", Version: 1, EmittedAt: at.UnixMilli(), Payload: json.RawMessage(`{}`)}
}

func reminderNoticeCount(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM router_events WHERE id='agent.notify'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestReconcileRouterRemindersPeriodicExactOwnerAndQuietReset(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	registry := reminderTestRegistry(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	owner := reminderOwner("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	wrongInstance := reminderOwner(owner.SessionID, "cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	currentTask := reminderInProgressTask(t, s, owner, now.Add(-time.Duration(reminderQuietForMS)*time.Millisecond).UnixMilli())
	oldOwnerTask := reminderInProgressTask(t, s, wrongInstance, now.Add(-time.Duration(reminderQuietForMS)*time.Millisecond).UnixMilli())
	if err := s.InsertProject(ctx, fixtureProject("other")); err != nil {
		t.Fatal(err)
	}
	otherProjectTask, err := s.InsertTask(ctx, TaskInsert{Project: "other", Title: "Other project", Reporter: "fixture"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE tasks SET status='in_progress',updated_at=? WHERE id=?", now.Add(-time.Duration(reminderQuietForMS)*time.Millisecond).UnixMilli(), otherProjectTask.ID); err != nil {
		t.Fatal(err)
	}
	// The source reports only the current exact execution. Busy presence remains
	// eligible for a reminder; delivery applies require_idle later.
	agent := PresenceAgent{ProjectIdent: "tasks", Origin: *owner, Busy: true}
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "native:reminder", Sequence: 1, Agents: []PresenceAgent{agent}}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, reminderClock(now)); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 1 {
		t.Fatalf("periodic reminder count=%d, want exact owner only", got)
	}
	var taskID string
	if err := s.db.QueryRow(`SELECT json_extract(payload,'$.event.task.id') FROM router_events WHERE id='agent.notify'`).Scan(&taskID); err != nil || taskID != currentTask.ID {
		t.Fatalf("reminded task=%q err=%v, want %q", taskID, err, currentTask.ID)
	}
	var requireIdle bool
	if err := s.db.QueryRow(`SELECT json_extract(payload,'$.require_idle') FROM router_events WHERE id='agent.notify'`).Scan(&requireIdle); err != nil || !requireIdle {
		t.Fatalf("require_idle=%v err=%v", requireIdle, err)
	}
	var periodicInstructions string
	if err := s.db.QueryRow(`SELECT json_extract(payload,'$.instructions') FROM router_events WHERE id='agent.notify'`).Scan(&periodicInstructions); err != nil {
		t.Fatal(err)
	}
	assertQuestionDecisionGuidance(t, "periodic", periodicInstructions)
	if err := s.db.QueryRow("SELECT COUNT(*) FROM reminder_nudges WHERE task_id=?", oldOwnerTask.ID).Scan(&taskID); err != nil || taskID != "0" {
		t.Fatalf("old instance nudge consumed: count=%q err=%v", taskID, err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM reminder_nudges WHERE task_id=?", otherProjectTask.ID).Scan(&taskID); err != nil || taskID != "0" {
		t.Fatalf("other project nudge consumed: count=%q err=%v", taskID, err)
	}

	// Replaying the same tick cannot send again. With no task change, the
	// reminder repeats after another full ten-minute quiet period.
	firstTick := reminderClock(now)
	firstTick.EmittedAt = now.UnixMilli()
	if err := s.ReconcileRouterReminders(ctx, registry, firstTick); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 1 {
		t.Fatalf("replay notice count=%d", got)
	}
	repeatAt := now.Add(time.Duration(reminderQuietForMS) * time.Millisecond)
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "native:reminder", Sequence: 2, Agents: []PresenceAgent{agent}}, repeatAt); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, firstTick); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 1 {
		t.Fatalf("replaying the original tick after a later presence report emitted again: %d", got)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, reminderClock(repeatAt)); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 2 {
		t.Fatalf("quiet task did not receive next periodic reminder: notices=%d", got)
	}
	// A task update resets its wait, including after a prior nudge.
	updatedAt := repeatAt.UnixMilli() + 1
	if _, err := s.db.Exec("UPDATE tasks SET updated_at=? WHERE id=?", updatedAt, currentTask.ID); err != nil {
		t.Fatal(err)
	}
	beforeQuiet := time.UnixMilli(updatedAt + reminderQuietForMS - 1)
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "native:reminder", Sequence: 3, Agents: []PresenceAgent{agent}}, beforeQuiet); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, reminderClock(beforeQuiet)); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 2 {
		t.Fatalf("task update did not reset quiet window: notices=%d", got)
	}
	quietAt := time.UnixMilli(updatedAt + reminderQuietForMS)
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "native:reminder", Sequence: 4, Agents: []PresenceAgent{agent}}, quietAt); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, reminderClock(quietAt)); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 3 {
		t.Fatalf("quiet updated task was not nudged: notices=%d", got)
	}
}

func TestReconcileRouterRemindersCommentResetsQuietWindow(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	registry := reminderTestRegistry(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	owner := reminderOwner("abababab-abab-4bab-8bab-abababababab", "cdcdcdcd-cdcd-4dcd-8dcd-cdcdcdcdcdcd")
	task := reminderInProgressTask(t, s, owner, now.Add(-time.Duration(reminderQuietForMS)*time.Millisecond).UnixMilli())
	agent := PresenceAgent{ProjectIdent: "tasks", Origin: *owner}
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "native:comment", Sequence: 1, Agents: []PresenceAgent{agent}}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, reminderClock(now)); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 1 {
		t.Fatalf("initial comment fixture reminder count=%d", got)
	}
	// Remove the prior periodic deadline so this assertion measures the task
	// timestamp written by AddTaskComment itself.
	if _, err := s.db.Exec("DELETE FROM reminder_nudges WHERE task_id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskComment(ctx, "tasks", task.ID, "fixture", "user", "Progress update", nil, nil); err != nil {
		t.Fatal(err)
	}
	var updatedAt int64
	if err := s.db.QueryRow("SELECT updated_at FROM tasks WHERE id=?", task.ID).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	beforeBoundary := time.UnixMilli(updatedAt + reminderQuietForMS - 1)
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "native:comment", Sequence: 2, Agents: []PresenceAgent{agent}}, beforeBoundary); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, reminderClock(beforeBoundary)); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 1 {
		t.Fatalf("comment did not reset quiet window: notices=%d", got)
	}
	boundary := time.UnixMilli(updatedAt + reminderQuietForMS)
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "native:comment", Sequence: 3, Agents: []PresenceAgent{agent}}, boundary); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, reminderClock(boundary)); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 2 {
		t.Fatalf("comment quiet boundary notices=%d, want 2", got)
	}
}

func TestReconcileRouterRemindersStartupFreshnessDisabledPeriodicAndDurableRun(t *testing.T) {
	ctx := context.Background()
	registry := reminderTestRegistry(t)
	path := filepath.Join(t.TempDir(), "reminders.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.InsertProject(ctx, fixtureProject("tasks")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	owner := reminderOwner("dddddddd-dddd-4ddd-8ddd-dddddddddddd", "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee")
	previousOwner := reminderOwner(owner.SessionID, "44444444-4444-4444-8444-444444444444")
	oldTask := reminderInProgressTask(t, s, previousOwner, now.Add(-time.Duration(reminderQuietForMS)*time.Millisecond).UnixMilli())
	run1, run2 := uuid.NewString(), uuid.NewString()
	stale := reminderHeartbeat(t, ctx, s, registry, "native:shared", 1, run1, []PresenceAgent{{ProjectIdent: "tasks", Origin: *owner}}, now)
	current := reminderHeartbeat(t, ctx, s, registry, "native:shared", 2, run2, []PresenceAgent{{ProjectIdent: "tasks", Origin: *owner}}, now.Add(time.Second))
	if _, err := s.db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('plugin.reminder.enabled','false')"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, reminderClock(now.Add(2*time.Second))); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 0 {
		t.Fatalf("disabled periodic reminders emitted %d notices", got)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, stale); err != nil {
		t.Fatal(err)
	}
	var restarts int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM reminder_restarts").Scan(&restarts); err != nil || restarts != 0 {
		t.Fatalf("stale heartbeat consumed lifetime: rows=%d err=%v", restarts, err)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, current); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 1 {
		t.Fatalf("current startup heartbeat notices=%d, want 1", got)
	}
	var payload []byte
	if err := s.db.QueryRow("SELECT payload FROM router_events WHERE id='agent.notify'").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var notice map[string]json.RawMessage
	if err := json.Unmarshal(payload, &notice); err != nil {
		t.Fatal(err)
	}
	var eventPayload struct {
		Task json.RawMessage `json:"task"`
	}
	var receiver struct {
		SessionID  string `json:"session_id"`
		InstanceID string `json:"instance_id"`
		Project    string `json:"project"`
	}
	if err := json.Unmarshal(notice["receiver"], &receiver); err != nil {
		t.Fatal(err)
	}
	var instructions string
	if err := json.Unmarshal(notice["instructions"], &instructions); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(notice["event"], &eventPayload); err != nil {
		t.Fatal(err)
	}
	var notifiedTask Task
	if err := json.Unmarshal(eventPayload.Task, &notifiedTask); err != nil || notifiedTask.ID != oldTask.ID || notifiedTask.OwnerOrigin == nil || *notifiedTask.OwnerOrigin != *previousOwner {
		t.Fatalf("startup notice changed task owner: task=%+v err=%v", notifiedTask, err)
	}
	if receiver.SessionID != owner.SessionID || receiver.InstanceID != owner.InstanceID || receiver.Project != "tasks" {
		t.Fatalf("previous-execution notice receiver=%+v, want exact current execution", receiver)
	}
	if instructions != reminderPreviousExecution {
		t.Fatalf("previous-execution instructions=%q", instructions)
	}
	if _, err := s.TransitionTask(ctx, "tasks", oldTask.ID, "todo", owner); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("current execution mutated previous owner's task: %v", err)
	}
	var durableOwner string
	if err := s.db.QueryRow("SELECT owner_origin_json FROM tasks WHERE id=?", oldTask.ID).Scan(&durableOwner); err != nil {
		t.Fatal(err)
	}
	var durableOrigin agent.Origin
	if err := json.Unmarshal([]byte(durableOwner), &durableOrigin); err != nil || durableOrigin != *previousOwner {
		t.Fatalf("task owner changed or disappeared: %q err=%v", durableOwner, err)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, current); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 1 {
		t.Fatalf("heartbeat replay notices=%d", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.ReconcileRouterReminders(ctx, registry, current); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 1 {
		t.Fatalf("durable replay notices=%d", got)
	}
}

// assertQuestionDecisionGuidance verifies owner prompts route user decisions
// to task_ask (the Questions center) rather than parking the task.
func assertQuestionDecisionGuidance(t *testing.T, route, instructions string) {
	t.Helper()
	if !strings.Contains(instructions, "task_ask") || !strings.Contains(instructions, "wait for its answer") ||
		strings.Contains(instructions, "user-review") || strings.Contains(instructions, "User review action") {
		t.Fatalf("%s decision guidance=%q, want task_ask workflow", route, instructions)
	}
}

func TestReconcileRouterRemindersRestartedOwnerGetsQuestionGuidance(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	registry := reminderTestRegistry(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	owner := reminderOwner("12121212-1212-4212-8212-121212121212", "34343434-3434-4434-8434-343434343434")
	task := reminderInProgressTask(t, s, owner, now.UnixMilli())
	if _, err := s.db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('plugin.reminder.enabled','false')"); err != nil {
		t.Fatal(err)
	}
	heartbeat := reminderHeartbeat(t, ctx, s, registry, "native:owner", 1, uuid.NewString(), []PresenceAgent{{ProjectIdent: "tasks", Origin: *owner}}, now)
	if err := s.ReconcileRouterReminders(ctx, registry, heartbeat); err != nil {
		t.Fatal(err)
	}
	var taskID, instructions string
	if err := s.db.QueryRow(`SELECT json_extract(payload,'$.event.task.id'),json_extract(payload,'$.instructions') FROM router_events WHERE id='agent.notify'`).Scan(&taskID, &instructions); err != nil {
		t.Fatal(err)
	}
	if taskID != task.ID {
		t.Fatalf("restart reminder task=%q, want %q", taskID, task.ID)
	}
	assertQuestionDecisionGuidance(t, "restart", instructions)
}

func TestReconcileRouterRemindersIndependentClientRunsAndReconnect(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	registry := reminderTestRegistry(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	ownerA := reminderOwner("55555555-5555-4555-8555-555555555555", "66666666-6666-4666-8666-666666666666")
	ownerB := reminderOwner("77777777-7777-4777-8777-777777777777", "88888888-8888-4888-8888-888888888888")
	taskA := reminderInProgressTask(t, s, ownerA, now.UnixMilli())
	taskB := reminderInProgressTask(t, s, ownerB, now.UnixMilli())
	runA, runB := uuid.NewString(), uuid.NewString()
	firstA := reminderHeartbeat(t, ctx, s, registry, "native:shared-clients", 1, runA, []PresenceAgent{{ProjectIdent: "tasks", Origin: *ownerA}}, now)
	if err := s.ReconcileRouterReminders(ctx, registry, firstA); err != nil {
		t.Fatal(err)
	}
	firstB := reminderHeartbeat(t, ctx, s, registry, "native:shared-clients", 2, runB, []PresenceAgent{{ProjectIdent: "tasks", Origin: *ownerB}}, now.Add(time.Second))
	if err := s.ReconcileRouterReminders(ctx, registry, firstB); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 2 {
		t.Fatalf("independent clients sharing source notices=%d, want 2", got)
	}
	// A reconnect in the same client run, including omitting and restoring the
	// agent from successive snapshots, must not repeat startup continuity.
	_ = reminderHeartbeat(t, ctx, s, registry, "native:shared-clients", 3, runA, nil, now.Add(2*time.Second))
	reconnectedA := reminderHeartbeat(t, ctx, s, registry, "native:shared-clients", 4, runA, []PresenceAgent{{ProjectIdent: "tasks", Origin: *ownerA}}, now.Add(3*time.Second))
	if err := s.ReconcileRouterReminders(ctx, registry, reconnectedA); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 2 {
		t.Fatalf("same-run reconnect emitted another notice: %d", got)
	}
	newRunA := reminderHeartbeat(t, ctx, s, registry, "native:shared-clients", 5, uuid.NewString(), []PresenceAgent{{ProjectIdent: "tasks", Origin: *ownerA}}, now.Add(4*time.Second))
	if err := s.ReconcileRouterReminders(ctx, registry, newRunA); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 3 {
		t.Fatalf("new client run notices=%d, want 3", got)
	}
	for _, task := range []*Task{taskA, taskB} {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM reminder_nudges WHERE task_id=?", task.ID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("task %s nudge rows=%d err=%v", task.ID, count, err)
		}
	}
}

func TestReconcileRouterRemindersRejectedUnresolvedReportDoesNotConsumeRun(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	registry := reminderTestRegistry(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	owner := reminderOwner("99999999-9999-4999-8999-999999999999", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	task := reminderInProgressTask(t, s, owner, now.UnixMilli())
	run := uuid.NewString()
	unresolved := PresenceAgent{ProjectIdent: "", Origin: *owner}
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "native:binding", Sequence: 1, ClientRun: &run, Agents: []PresenceAgent{unresolved}}, now); err == nil || accepted {
		t.Fatalf("unresolved project report accepted=%v err=%v", accepted, err)
	}
	var outbox, receipts int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM amux_heartbeat_outbox").Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM reminder_restarts").Scan(&receipts); err != nil || outbox != 0 || receipts != 0 {
		t.Fatalf("rejected report left heartbeat outbox=%d restart receipts=%d err=%v", outbox, receipts, err)
	}
	bound := reminderHeartbeat(t, ctx, s, registry, "native:binding", 1, run, []PresenceAgent{{ProjectIdent: "tasks", Origin: *owner}}, now)
	if err := s.ReconcileRouterReminders(ctx, registry, bound); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 1 {
		t.Fatalf("valid project binding after rejected report notices=%d", got)
	}
	var nudged string
	if err := s.db.QueryRow("SELECT task_id FROM reminder_nudges").Scan(&nudged); err != nil || nudged != task.ID {
		t.Fatalf("valid binding nudge task=%q err=%v", nudged, err)
	}
}

func TestReconcileRouterRemindersExpiredOwnerTTLBoundary(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	registry := reminderTestRegistry(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	owner := reminderOwner("bbbbbbbb-cccc-4ddd-8eee-ffffffffffff", "cccccccc-dddd-4eee-8fff-aaaaaaaaaaaa")
	task := reminderInProgressTask(t, s, owner, now.Add(-time.Duration(reminderQuietForMS)*time.Millisecond).UnixMilli())
	expiredAt := now.Add(-time.Duration(presenceGoneAfter) * time.Millisecond)
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "native:ttl", Sequence: 1, Agents: []PresenceAgent{{ProjectIdent: "tasks", Origin: *owner}}}, expiredAt); err != nil || !accepted {
		t.Fatalf("seed expired native owner accepted=%v err=%v", accepted, err)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, reminderClock(now)); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 0 {
		t.Fatalf("expired owner was nudged at TTL boundary: %d", got)
	}
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "native:ttl", Sequence: 2, Agents: []PresenceAgent{{ProjectIdent: "tasks", Origin: *owner}}}, now); err != nil || !accepted {
		t.Fatalf("refresh native owner accepted=%v err=%v", accepted, err)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, reminderClock(now.Add(time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	if got := reminderNoticeCount(t, s); got != 1 {
		t.Fatalf("refreshed owner reminder count=%d", got)
	}
	var nudgedTask string
	if err := s.db.QueryRow("SELECT task_id FROM reminder_nudges").Scan(&nudgedTask); err != nil || nudgedTask != task.ID {
		t.Fatalf("refreshed owner nudge task=%q err=%v", nudgedTask, err)
	}
}

func TestReconcileRouterRemindersRollsBackDedupeAndNudgeOnEmitFailure(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	registry := reminderTestRegistry(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	owner := reminderOwner("ffffffff-ffff-4fff-8fff-ffffffffffff", "11111111-1111-4111-8111-111111111111")
	taskA := reminderInProgressTask(t, s, owner, now.Add(-time.Duration(reminderQuietForMS)*time.Millisecond).UnixMilli())
	taskB := reminderInProgressTask(t, s, owner, now.Add(-time.Duration(reminderQuietForMS)*time.Millisecond).UnixMilli())
	event := reminderHeartbeat(t, ctx, s, registry, "native:rollback", 1, uuid.NewString(), []PresenceAgent{{ProjectIdent: "tasks", Origin: *owner}}, now)
	if _, err := s.db.Exec(`CREATE TRIGGER reminder_emit_failure BEFORE INSERT ON router_events WHEN NEW.id='agent.notify'
		AND (SELECT COUNT(*) FROM router_events WHERE id='agent.notify')>0
		BEGIN SELECT RAISE(ABORT,'injected reminder event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, event); err == nil {
		t.Fatal("startup reminder succeeded after injected event failure")
	}
	for table, query := range map[string]string{
		"restart dedupe": "SELECT COUNT(*) FROM reminder_restarts",
		"nudges":         "SELECT COUNT(*) FROM reminder_nudges WHERE task_id IN ('" + taskA.ID + "','" + taskB.ID + "')",
		"notifications":  "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'",
	} {
		var count int
		if err := s.db.QueryRow(query).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed transaction retained %s: count=%d err=%v", table, count, err)
		}
	}
	if _, err := s.db.Exec("DROP TRIGGER reminder_emit_failure"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterReminders(ctx, registry, event); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	if got := reminderNoticeCount(t, s); got != 2 {
		t.Fatalf("retry notifications=%d", got)
	}
}

func TestReconcileRouterRemindersUsesMonotonicStartupSentAt(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	registry := reminderTestRegistry(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	owner := reminderOwner("22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333")
	task := reminderInProgressTask(t, s, owner, now.Add(-time.Duration(reminderQuietForMS)*time.Millisecond).UnixMilli())
	// A previously advanced presence clock must win over a backwards wall clock.
	if err := s.ReconcileRouterPresence(ctx, registry, reminderClock(now.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	event := reminderHeartbeat(t, ctx, s, registry, "native:monotonic", 1, uuid.NewString(), []PresenceAgent{{ProjectIdent: "tasks", Origin: *owner}}, now)
	if err := s.ReconcileRouterReminders(ctx, registry, event); err != nil {
		t.Fatal(err)
	}
	var sentAt, clock int64
	if err := s.db.QueryRow("SELECT sent_at FROM reminder_nudges WHERE task_id=?", task.ID).Scan(&sentAt); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT observed_at FROM amux_presence_clock WHERE id=1").Scan(&clock); err != nil {
		t.Fatal(err)
	}
	if sentAt < clock {
		t.Fatalf("startup sent_at=%d fell behind effective clock=%d", sentAt, clock)
	}
}
