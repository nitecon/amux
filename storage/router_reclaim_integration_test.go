package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/identity/agent"
)

func TestReclaimRuntimeUsesCommittedPresenceAndStallsOfflineNativeWork(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "reclaim-runtime.db"))
	create := func(title string) *Task {
		t.Helper()
		task, err := s.InsertTask(ctx, TaskInsert{Project: "presence", Title: title, Reporter: "fixture"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	native, legacy, busyLegacy := create("Offline native work"), create("Unbound legacy work"), create("Busy legacy work")
	owner := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	if _, err := s.TransitionTask(ctx, "presence", native.ID, "in_progress", &owner); err != nil {
		t.Fatal(err)
	}
	conv, err := s.CreateConversation(ctx, "presence", native.ID, &owner)
	if err != nil {
		t.Fatal(err)
	}
	command, err := s.EnqueueConversationCommand(ctx, "presence", conv.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: "Continue durable work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimConversationCommand(ctx, "presence", conv.ID, command.ID, &owner); err != nil {
		t.Fatal(err)
	}
	run := WorkerRun{ID: uuid.NewString(), Server: "http://127.0.0.1:1", Project: "presence", ConversationID: conv.ID, Directory: t.TempDir(), Owner: owner}
	if err := s.SaveWorkerRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	changes, err := s.ConversationCommandChanges(ctx, "presence", conv.ID, 0, 10)
	if err != nil || len(changes) == 0 {
		t.Fatalf("revisioned command feed: %+v %v", changes, err)
	}
	for _, change := range changes {
		if err := s.ObserveWorkerCommand(ctx, run.ID, change); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.StartWorkerCommand(ctx, run.ID, command.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueWorkerEvent(ctx, run.ID, conversation.Event{Kind: "session_ready", SessionID: "offline-native"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueWorkerEvent(ctx, run.ID, conversation.Event{Kind: "text_delta", SessionID: "offline-native", Text: "Unflushed offline output"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stale := now.Add(-16 * time.Minute).UnixMilli()
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "native:offline", Sequence: 1,
		Agents: []PresenceAgent{{ProjectIdent: "presence", Origin: owner, Busy: true}}}, time.UnixMilli(stale)); err != nil || !accepted {
		t.Fatalf("offline presence seed: %v %v", accepted, err)
	}
	if _, err := s.db.Exec("UPDATE tasks SET updated_at=? WHERE id=?", stale, native.ID); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, session string }{{legacy.ID, "legacy-session"}, {busyLegacy.ID, "busy-session"}} {
		origin, _ := json.Marshal(map[string]string{"session_id": item.session, "instance_id": "previous"})
		if _, err := s.db.Exec("UPDATE tasks SET status='in_progress',owner_origin_json=?,updated_at=? WHERE id=?", string(origin), stale, item.id); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct{ project, session, instance string }{{"presence", "busy-session", "previous"}, {"", "legacy-session", "replacement"}} {
		raw, _ := json.Marshal(map[string]string{"project": item.project, "session_id": item.session, "instance_id": item.instance, "provider": "legacy", "os": "windows", "source": "raw:reclaim"})
		if _, err := s.db.Exec(`INSERT INTO amux_presence_agents(project_ident,instance_id,session_id,provider,os,source,sequence,
			seen_at,last_activity_at,busy,draft,auto_assign,review_context_clear,state,report_kind,event_agent_json)
			VALUES(?,?,?,'legacy','windows','raw:reclaim',1,?,?,0,0,0,0,'idle','event',?)`,
			item.project, item.instance, item.session, now.UnixMilli(), now.UnixMilli(), string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	var nativeEvents int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM task_events WHERE json_extract(task_json,'$.id')=?", native.ID).Scan(&nativeEvents); err != nil {
		t.Fatal(err)
	}
	var outboxCount int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM amux_worker_outbox WHERE run_id=?", run.ID).Scan(&outboxCount); err != nil || outboxCount == 0 {
		t.Fatalf("offline outbox fixture: %d %v", outboxCount, err)
	}
	gate := &gatedReminderPresence{Store: s, entered: make(chan struct{}), released: make(chan struct{})}
	runtime, err := fism.NewRuntime(s, s.EventBus(), append(nativePresenceDefinitions(), controllers.NotificationEventDefinitions()...), []fism.Controller{
		controllers.PresenceController(gate), controllers.ReclaimController(s),
	}, fism.RunConfig{Capacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	tick, err := s.EmitRouterEvent(ctx, runtime.Registry(), fism.EmitRequest{ID: "clock.tick", Version: 1, Source: "clock", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	stop := runRouterFixture(t, runtime)
	defer stop()
	awaitRouterSignal(t, gate.entered)
	waitRouterDeliveryState(t, s, tick.Event.Seq, "reclaim", "pending")
	assertEqual(t, s.db, "SELECT status FROM tasks WHERE id='"+legacy.ID+"'", "in_progress")
	close(gate.released)
	waitRouterDeliveryState(t, s, tick.Event.Seq, "reclaim", "completed")
	assertEqual(t, s.db, "SELECT status FROM tasks WHERE id='"+legacy.ID+"'", "stalled")
	assertEqual(t, s.db, "SELECT status FROM tasks WHERE id='"+busyLegacy.ID+"'", "in_progress")
	assertEqual(t, s.db, "SELECT COUNT(*) FROM task_comments WHERE task_id='"+legacy.ID+"' AND author='amux' AND author_type='system'", int64(1))
	assertEqual(t, s.db, "SELECT state FROM amux_presence_agents WHERE source='native:offline'", "gone")
	detail, err := s.Task(ctx, "presence", native.ID)
	if err != nil || detail.Task.Status != "stalled" || detail.Task.OwnerOrigin == nil || *detail.Task.OwnerOrigin != owner || detail.Task.UpdatedAt != stale {
		t.Fatalf("clock did not stall inactive native work: %+v %v", detail, err)
	}
	assertEqual(t, s.db, "SELECT state FROM amux_conversation_commands WHERE id='"+command.ID+"'", "claimed")
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_worker_outbox WHERE run_id='"+run.ID+"'", outboxCount)
	assertEqual(t, s.db, "SELECT COUNT(*) FROM task_events WHERE json_extract(task_json,'$.id')='"+native.ID+"'", nativeEvents)
	if err := s.ReconcileRouterProgress(ctx, runtime.Registry(), tick.Event); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM task_comments WHERE task_id='"+legacy.ID+"'", int64(1))
}
