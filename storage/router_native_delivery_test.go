package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/identity/agent"
)

func nativeDeliveryRegistry(t *testing.T) *fism.Registry {
	t.Helper()
	r, err := fism.NewRegistry(controllers.NotificationEventDefinitions(), []fism.ControllerDefinition{
		controllers.NativeDeliveryController(nil).Definition,
		{ID: "assignment", Subscriptions: []fism.Subscription{{Event: controllers.NotifyResultEventID, MinVersion: 1, MaxVersion: 1}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func nativeNoticePayload(t *testing.T, task *Task, mode string, reminder bool, draft bool) []byte {
	t.Helper()
	return mustJSONBytes(t, map[string]any{
		"target": task.OwnerOrigin.SessionID, "project": task.ProjectIdent, "mode": mode,
		"require_idle": true, "reminder": reminder, "heading": "Continue task",
		"instructions": "Preserve the review only boundary.",
		"event":        map[string]any{"kind": "task_commented", "task": task, "comment": map[string]any{"author": "reviewer", "content": "Please inspect this"}},
		"draft":        draft,
	})
}

func mustJSONBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func emitNativeNotice(t *testing.T, s *Store, r *fism.Registry, task *Task, source, mode string, reminder bool) fism.Event {
	t.Helper()
	payload := nativeNoticePayload(t, task, mode, reminder, false)
	result, err := s.EmitRouterEvent(context.Background(), r, fism.EmitRequest{ID: controllers.NotifyEventID, Version: 1, Source: source, Payload: payload})
	if err != nil || !result.Stored {
		t.Fatalf("emit notice: %+v %v", result, err)
	}
	return result.Event
}

func reportNativeDeliveryAgent(t *testing.T, s *Store, actor *agent.Origin, sequence int64, draft bool) {
	t.Helper()
	reportNativeDeliveryAgentGeneration(t, s, actor, sequence, draft, "cccccccc-cccc-4ccc-8ccc-cccccccccccc")
}

func reportNativeDeliveryAgentGeneration(t *testing.T, s *Store, actor *agent.Origin, sequence int64, draft bool, generation string) {
	t.Helper()
	now := time.Now()
	activity := now.Add(-2 * time.Minute).UnixMilli()
	_, err := s.ReportPresenceWithPrincipal(context.Background(), PresenceReport{Source: "native:delivery-test", Sequence: sequence, Agents: []PresenceAgent{{ProjectIdent: "tasks", Origin: *actor, Generation: &generation, Busy: true, Draft: draft, LastActivityAt: &activity}}}, now, "client:delivery-test")
	if err != nil {
		t.Fatal(err)
	}
}

func TestNativeDeliveryReminderWaitsForDraftThenQueuesOnceDespiteTaskBusy(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	actor := taskActor()
	task := ownedInProgressTask(t, s, actor)
	conversation, err := s.CreateConversation(ctx, "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	task, err = readTask(ctx, s.db, "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	reportNativeDeliveryAgent(t, s, actor, 1, true)
	r := nativeDeliveryRegistry(t)
	event := emitNativeNotice(t, s, r, task, "plugin:reminder", "assigned", true)
	pending, err := s.ReconcileNativeDelivery(ctx, r, event)
	if err != nil || !pending {
		t.Fatalf("drafted notice should remain pending: pending=%t err=%v", pending, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands WHERE conversation_id='"+conversation.ID+"' AND kind='prompt'", int64(0))
	reportNativeDeliveryAgent(t, s, actor, 2, false)
	pending, err = s.ReconcileNativeDelivery(ctx, r, event)
	if err != nil || pending {
		t.Fatalf("idle conversation with task-busy presence should deliver: pending=%t err=%v", pending, err)
	}
	commands, err := s.ConversationCommands(ctx, "tasks", conversation.ID, 0, 10)
	if err != nil || len(commands) != 1 {
		t.Fatalf("prompt delivery commands=%+v err=%v", commands, err)
	}
	if commands[0].ID != event.UID || commands[0].Command.Kind != "prompt" {
		t.Fatalf("command identity/kind=%+v", commands[0])
	}
	if pending, err = s.ReconcileNativeDelivery(ctx, r, event); err != nil || pending {
		t.Fatalf("replay should be terminal: pending=%t err=%v", pending, err)
	}
	commands, err = s.ConversationCommands(ctx, "tasks", conversation.ID, 0, 10)
	if err != nil || len(commands) != 1 {
		t.Fatalf("replay duplicated prompt: commands=%+v err=%v", commands, err)
	}
	var resultCount int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM router_events WHERE id=? AND correlation=?", controllers.NotifyResultEventID, event.UID).Scan(&resultCount); err != nil || resultCount != 1 {
		t.Fatalf("correlated result count=%d err=%v", resultCount, err)
	}
	var resultSource string
	if err := s.db.QueryRowContext(ctx, "SELECT source FROM router_events WHERE id=? AND correlation=?", controllers.NotifyResultEventID, event.UID).Scan(&resultSource); err != nil || resultSource != "native:delivery-test" {
		t.Fatalf("result source=%q err=%v", resultSource, err)
	}
	assertEqual(t, s.db, `SELECT COUNT(*) FROM amux_heartbeat_outbox h JOIN router_events e ON e.uid=h.uid WHERE e.id='agent.notify_result'`, int64(1))
	assertEqual(t, s.db, `SELECT COUNT(*) FROM router_deliveries d JOIN router_events e ON e.seq=d.event_seq WHERE e.id='agent.notify_result'`, int64(0))

}

func TestNativeDeliveryAssignmentWithoutOfferSkipsInsteadOfRetargeting(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	actor := taskActor()
	task := ownedInProgressTask(t, s, actor)
	reportNativeDeliveryAgent(t, s, actor, 1, false)
	r := nativeDeliveryRegistry(t)
	event := emitNativeNotice(t, s, r, task, "plugin:assignment", "assigned", false)
	pending, err := s.ReconcileNativeDelivery(ctx, r, event)
	if err != nil || pending {
		t.Fatalf("missing assignment offer should terminate as skipped: pending=%t err=%v", pending, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_native_delivery_receipts WHERE notify_uid='"+event.UID+"' AND status='skipped'", int64(1))
	var resultSource string
	if err := s.db.QueryRowContext(ctx, "SELECT source FROM router_events WHERE id=? AND correlation=?", controllers.NotifyResultEventID, event.UID).Scan(&resultSource); err != nil || resultSource != "plugin:native-delivery" {
		t.Fatalf("unproven assignment result source=%q err=%v", resultSource, err)
	}
}

func TestNativeDeliveryValidUnsupportedNotificationSkipsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	r := nativeDeliveryRegistry(t)
	cases := []struct {
		name, target, project string
		task                  json.RawMessage
	}{
		{name: "blank target and project with empty task", target: "", project: "", task: json.RawMessage(`{}`)},
		{name: "missing task ID", target: taskActor().SessionID, project: "tasks", task: json.RawMessage(`{}`)},
		{name: "nonstring task ID", target: taskActor().SessionID, project: "tasks", task: json.RawMessage(`{"id":17}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := mustJSONBytes(t, map[string]any{"target": tc.target, "project": tc.project, "mode": "recipient", "require_idle": true, "event": map[string]any{"kind": "task_created", "task": tc.task}})
			emitted, err := s.EmitRouterEvent(ctx, r, fism.EmitRequest{ID: controllers.NotifyEventID, Version: 1, Source: "plugin:reminder", Payload: payload})
			if err != nil || !emitted.Stored {
				t.Fatalf("valid notification emit: %+v %v", emitted, err)
			}
			for i := 0; i < 2; i++ {
				pending, err := s.ReconcileNativeDelivery(ctx, r, emitted.Event)
				if err != nil || pending {
					t.Fatalf("unsupported context must terminate: pending=%t err=%v", pending, err)
				}
			}
			assertEqual(t, s.db, "SELECT status FROM amux_native_delivery_receipts WHERE notify_uid='"+emitted.Event.UID+"'", "skipped")
			assertEqual(t, s.db, "SELECT source FROM router_events WHERE id='agent.notify_result' AND correlation='"+emitted.Event.UID+"'", "plugin:native-delivery")
			assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events WHERE id='agent.notify_result' AND correlation='"+emitted.Event.UID+"'", int64(1))
			assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands WHERE kind='prompt'", int64(0))
		})
	}
}

func TestNativeDeliveryMalformedPublicNotificationStillFails(t *testing.T) {
	s := taskStore(t)
	r := nativeDeliveryRegistry(t)
	event := fism.Event{UID: uuid.NewString(), ID: controllers.NotifyEventID, Version: 1, Source: "plugin:reminder", EmittedAt: time.Now().UnixMilli(), Payload: json.RawMessage(`{"target":"","project":"","mode":"unsupported","require_idle":true,"event":{"kind":"task_created","task":{}}}`)}
	if _, err := s.ReconcileNativeDelivery(context.Background(), r, event); err == nil {
		t.Fatal("malformed public notification was silently skipped")
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_native_delivery_receipts", int64(0))
}

func TestNativeDeliveryStartupContinuityTargetsCurrentExplicitReceiver(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	previous := taskActor()
	task := ownedInProgressTask(t, s, previous)
	current := *previous
	current.InstanceID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	c, err := s.CreateConversation(ctx, "tasks", task.ID, &current)
	if err != nil {
		t.Fatal(err)
	}
	reportNativeDeliveryAgent(t, s, &current, 1, false)
	payload := nativeNoticePayload(t, task, "recipient", false, false)
	var notice map[string]any
	if err := json.Unmarshal(payload, &notice); err != nil {
		t.Fatal(err)
	}
	notice["target"] = current.SessionID
	notice["receiver"] = map[string]any{"session_id": current.SessionID, "instance_id": current.InstanceID, "provider": current.Provider, "os": current.OS, "project": "tasks"}
	notice["instructions"] = "Informational continuity only; inspect prior progress."
	r := nativeDeliveryRegistry(t)
	emitted, err := s.EmitRouterEvent(ctx, r, fism.EmitRequest{ID: controllers.NotifyEventID, Version: 1, Source: "plugin:reminder", Payload: mustJSONBytes(t, notice)})
	if err != nil || !emitted.Stored {
		t.Fatalf("emit continuity notice: %+v %v", emitted, err)
	}
	if pending, err := s.ReconcileNativeDelivery(ctx, r, emitted.Event); err != nil || pending {
		t.Fatalf("continuity notice should use explicit current receiver: pending=%t err=%v", pending, err)
	}
	commands, err := s.ConversationCommands(ctx, "tasks", c.ID, 0, 10)
	if err != nil || len(commands) != 1 {
		t.Fatalf("continuity prompt=%+v err=%v", commands, err)
	}
}

func TestNativeDeliveryAdoptionReceiptSurvivesOfferInvalidationAndExpiry(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	migrateTestAssignmentAdoption(t, s)
	actor := taskActor()
	task := ownedInProgressTask(t, s, actor)
	r := nativeDeliveryRegistry(t)
	uid := uuid.NewString()
	payload := nativeNoticePayload(t, task, "assigned", false, false)
	origin, _ := json.Marshal(actor)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO assignment_offers(notify_uid,task_id,project,session_id,reporter_source) VALUES(?,?,?,?,NULL)`, uid, task.ID, "tasks", actor.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO amux_assignment_offer_claims(notify_uid,origin_json,claim_updated_at,reporter_principal) VALUES(?,?,?,NULL)`, uid, string(origin), task.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	result, err := s.EmitRouterEvent(ctx, r, fism.EmitRequest{UID: uid, ID: controllers.NotifyEventID, Version: 1, Source: "plugin:assignment", Payload: payload})
	if err != nil || !result.Stored {
		t.Fatalf("emit assignment offer: %+v %v", result, err)
	}
	old := time.Now().Add(-10 * time.Minute).UnixMilli()
	if _, err := s.db.ExecContext(ctx, "UPDATE router_events SET emitted_at=? WHERE uid=?", old, uid); err != nil {
		t.Fatal(err)
	}
	result.Event.EmittedAt = old
	c, err := s.CreateConversation(ctx, "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	promptID, runID := uuid.NewString(), uuid.NewString()
	if _, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, promptID, conversation.Command{Kind: "prompt", Text: "atomic initial assignment prompt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO amux_assignment_runs(run_id,task_id,session_id,conversation_id,prompt_id) VALUES(?,?,?,?,?)`, runID, task.ID, actor.SessionID, c.ID, promptID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := captureNativeDeliveryAdoptionIn(ctx, tx, task, *actor, runID, c.ID, promptID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// A task terminal transition removes its assignment mapping and offer; the
	// captured initial prompt remains a historical accepted intent.
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET status='done',owner_origin_json=NULL,owner_agent_id=NULL WHERE id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_assignment_runs WHERE task_id='"+task.ID+"'", int64(0))
	pending, err := s.ReconcileNativeDelivery(ctx, r, result.Event)
	if err != nil || pending {
		t.Fatalf("captured adoption should ack even after expiry/offer deletion: pending=%t err=%v", pending, err)
	}
	commands, err := s.ConversationCommands(ctx, "tasks", c.ID, 0, 10)
	if err != nil || len(commands) != 1 || commands[0].ID != promptID {
		t.Fatalf("adoption must not enqueue a second prompt: %+v err=%v", commands, err)
	}
	assertEqual(t, s.db, "SELECT status FROM amux_native_delivery_receipts WHERE notify_uid='"+uid+"'", "completed")
	var results int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM router_events WHERE id=? AND correlation=?", controllers.NotifyResultEventID, uid).Scan(&results); err != nil || results != 1 {
		t.Fatalf("adoption result count=%d err=%v", results, err)
	}
}

func TestNativeDeliveryClockFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	actor := taskActor()
	task := ownedInProgressTask(t, s, actor)
	r := nativeDeliveryRegistry(t)
	event := emitNativeNotice(t, s, r, task, "plugin:reminder", "recipient", false)
	if _, err := s.db.ExecContext(ctx, "DROP TABLE amux_presence_clock"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileNativeDelivery(ctx, r, event); err == nil {
		t.Fatal("missing monotonic clock must fail and retry")
	}
	var receipts int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_native_delivery_receipts WHERE notify_uid=?", event.UID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 0 {
		t.Fatalf("failed clock read left a receipt: %d", receipts)
	}
}
