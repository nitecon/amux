package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/identity/agent"
)

func nativeAuthorityFixture(t *testing.T, draft bool) (*Store, *Task, *Conversation, *agent.Origin, *fism.Registry) {
	t.Helper()
	s, c, actor := commandConversation(t)
	task, err := readTask(context.Background(), s.db, c.ProjectIdent, c.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	reportNativeDeliveryAgent(t, s, actor, 1, draft)
	return s, task, c, actor, nativeDeliveryRegistry(t)
}

func TestNativeDeliveryPendingReceiptSurvivesReopenAndIgnoresPayloadAuthority(t *testing.T) {
	ctx := context.Background()
	s, task, c, actor, registry := nativeAuthorityFixture(t, true)
	var payload map[string]any
	if err := json.Unmarshal(nativeNoticePayload(t, task, "recipient", false, false), &payload); err != nil {
		t.Fatal(err)
	}
	payload["source"], payload["principal"] = "forged:source", "forged:principal"
	emitted, err := s.EmitRouterEvent(ctx, registry, fism.EmitRequest{ID: controllers.NotifyEventID, Version: 1, Source: "plugin:notifier", Payload: mustJSONBytes(t, payload)})
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := s.ReconcileNativeDelivery(ctx, registry, emitted.Event); err != nil || !pending {
		t.Fatalf("draft should defer the frozen receipt: %v %v", pending, err)
	}
	var seq int
	var name, path string
	if err := s.db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reportNativeDeliveryAgent(t, reopened, actor, 2, false)
	for range 2 {
		if pending, err := reopened.ReconcileNativeDelivery(ctx, registry, emitted.Event); err != nil || pending {
			t.Fatalf("resumed notification: %v %v", pending, err)
		}
	}
	assertEqual(t, reopened.db, "SELECT COUNT(*) FROM amux_conversation_commands WHERE conversation_id='"+c.ID+"'", int64(1))
	assertEqual(t, reopened.db, "SELECT principal FROM amux_native_delivery_receipts WHERE notify_uid='"+emitted.Event.UID+"'", "client:delivery-test")
	assertEqual(t, reopened.db, "SELECT source FROM router_events WHERE id='agent.notify_result' AND correlation='"+emitted.Event.UID+"'", "native:delivery-test")
	assertEqual(t, reopened.db, "SELECT COUNT(*) FROM router_events WHERE id='agent.notify_result' AND correlation='"+emitted.Event.UID+"'", int64(1))
}

func TestNativeDeliveryReplacementExecutionCannotReceiveFrozenNotice(t *testing.T) {
	for _, change := range []string{"instance", "generation"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s, task, c, actor, registry := nativeAuthorityFixture(t, true)
			event := emitNativeNotice(t, s, registry, task, "plugin:notifier", "recipient", false)
			if pending, err := s.ReconcileNativeDelivery(ctx, registry, event); err != nil || !pending {
				t.Fatalf("freeze before replacement: %v %v", pending, err)
			}
			replacement := *actor
			generation := uuid.NewString()
			if change == "instance" {
				replacement.InstanceID = uuid.NewString()
				if _, err := s.ResumeConversation(ctx, c.ProjectIdent, c.ID, actor.InstanceID, &replacement); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ReportPresenceWithPrincipal(ctx, PresenceReport{Source: "native:delivery-test", Sequence: 2,
				Agents: []PresenceAgent{{ProjectIdent: c.ProjectIdent, Origin: replacement, Generation: &generation, Busy: true}}}, time.Now(), "client:delivery-test"); err != nil {
				t.Fatal(err)
			}
			if pending, err := s.ReconcileNativeDelivery(ctx, registry, event); err != nil || pending {
				t.Fatalf("known replacement should skip the old execution: %v %v", pending, err)
			}
			assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands", int64(0))
			assertEqual(t, s.db, "SELECT json_extract(payload,'$.status') FROM router_events WHERE id='agent.notify_result' AND correlation='"+event.UID+"'", "skipped")
		})
	}
}

func TestNativeDeliveryCannotReuseAuthorityAfterTrustedInternalReport(t *testing.T) {
	ctx := context.Background()
	s, task, _, actor, registry := nativeAuthorityFixture(t, true)
	event := emitNativeNotice(t, s, registry, task, "plugin:notifier", "recipient", false)
	if pending, err := s.ReconcileNativeDelivery(ctx, registry, event); err != nil || !pending {
		t.Fatalf("freeze before authority change: %v %v", pending, err)
	}
	generation := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "native:delivery-test", Sequence: 2,
		Agents: []PresenceAgent{{ProjectIdent: "tasks", Origin: *actor, Generation: &generation, Busy: true}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileNativeDelivery(ctx, registry, event); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands", int64(0))
}

func TestNativeDeliveryRawObservationCannotAuthorizeExistingConversation(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	actor := *taskActor()
	task := makeTask(t, s, nil)
	c, err := s.CreateConversation(ctx, task.ProjectIdent, task.ID, &actor)
	if err != nil {
		t.Fatal(err)
	}
	task, err = readTask(ctx, s.db, task.ProjectIdent, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindConversationSession(ctx, task.ProjectIdent, c.ID, "raw-observed-session", &actor); err != nil {
		t.Fatal(err)
	}
	body := mustJSONBytes(t, map[string]any{"principal": "client:delivery-test", "agents": []map[string]any{{
		"project": task.ProjectIdent, "session_id": actor.SessionID, "instance_id": actor.InstanceID,
		"provider": actor.Provider, "os": actor.OS, "auto_assign": true,
	}}})
	if err := s.ReconcilePresenceEvent(ctx, rawPresenceRegistry(t), rawPresenceEvent(controllers.ClientHeartbeatID, "raw:principal-spoof", 1, time.Now().UnixMilli(), string(body))); err != nil {
		t.Fatal(err)
	}
	registry := nativeDeliveryRegistry(t)
	event := emitNativeNotice(t, s, registry, task, "plugin:notifier", "recipient", false)
	if _, err := s.ReconcileNativeDelivery(ctx, registry, event); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands", int64(0))
}

func TestNativeDeliveryResultFailureRollsBackPromptAndKeepsFrozenReceipt(t *testing.T) {
	ctx := context.Background()
	s, task, _, actor, registry := nativeAuthorityFixture(t, true)
	event := emitNativeNotice(t, s, registry, task, "plugin:notifier", "recipient", false)
	if pending, err := s.ReconcileNativeDelivery(ctx, registry, event); err != nil || !pending {
		t.Fatalf("freeze before delivery failure: %v %v", pending, err)
	}
	reportNativeDeliveryAgent(t, s, actor, 2, false)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_native_result BEFORE INSERT ON router_events
		WHEN NEW.id='agent.notify_result' BEGIN SELECT RAISE(ABORT,'injected native result failure'); END`); err != nil {
		t.Fatal(err)
	}
	sub := s.EventBus().Subscribe(8)
	defer sub.Unsubscribe()
	if _, err := s.ReconcileNativeDelivery(ctx, registry, event); err == nil {
		t.Fatal("native result failure was swallowed")
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands", int64(0))
	assertEqual(t, s.db, "SELECT status FROM amux_native_delivery_receipts WHERE notify_uid='"+event.UID+"'", "pending")
	select {
	case event := <-sub.C:
		t.Fatalf("failed delivery announced an uncommitted effect: %+v", event)
	default:
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_native_result"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if pending, err := s.ReconcileNativeDelivery(ctx, registry, event); err != nil || pending {
			t.Fatalf("delivery retry: %v %v", pending, err)
		}
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands", int64(1))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events WHERE id='agent.notify_result' AND correlation='"+event.UID+"'", int64(1))
}

func TestNativeDeliveryRequiresDurableResultSubscriber(t *testing.T) {
	s, task, _, _, _ := nativeAuthorityFixture(t, false)
	registry, err := fism.NewRegistry(controllers.NotificationEventDefinitions(), []fism.ControllerDefinition{controllers.NativeDeliveryController(nil).Definition})
	if err != nil {
		t.Fatal(err)
	}
	event := emitNativeNotice(t, s, registry, task, "plugin:notifier", "recipient", false)
	if _, err := s.ReconcileNativeDelivery(context.Background(), registry, event); err == nil {
		t.Fatal("notification effect committed without a durable result")
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_native_delivery_receipts", int64(0))
}

func TestNativeDeliveryExpiredUnadoptedOfferCanCompensateWithoutStartingWork(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	actor := taskActor()
	now := time.Now()
	activity := now.Add(-time.Minute).UnixMilli()
	if _, err := s.ReportPresenceWithPrincipal(ctx, PresenceReport{Source: "native:delivery-test", Sequence: 1,
		Agents: []PresenceAgent{{ProjectIdent: "tasks", Origin: *actor, AutoAssign: true, LastActivityAt: &activity}}}, now, "client:delivery-test"); err != nil {
		t.Fatal(err)
	}
	task := makeTask(t, s, nil)
	registry := nativeDeliveryRegistry(t)
	if err := s.ReconcileRouterAssignmentsWithRegistry(ctx, registry, fism.Event{ID: "clock.tick", Version: 1, UID: uuid.NewString(), EmittedAt: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	var uid string
	if err := s.db.QueryRow("SELECT notify_uid FROM assignment_offers WHERE task_id=?", task.ID).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	event, err := readRouterEventByUID(ctx, s.db, uid)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := s.ReconcileNativeDelivery(ctx, registry, event); err != nil || !pending {
		t.Fatalf("unadopted offer should wait: %v %v", pending, err)
	}
	if err := s.ReconcilePresence(ctx, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.ReconcileNativeDelivery(ctx, registry, event); err != nil || pending {
		t.Fatalf("expired offer should settle: %v %v", pending, err)
	}
	var resultUID string
	if err := s.db.QueryRow("SELECT uid FROM router_events WHERE id='agent.notify_result' AND correlation=?", uid).Scan(&resultUID); err != nil {
		t.Fatal(err)
	}
	result, err := readRouterEventByUID(ctx, s.db, resultUID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Source != "native:delivery-test" {
		t.Fatalf("expiry lost captured source: %+v", result)
	}
	if err := s.ReconcileRouterAssignmentFeedback(ctx, result); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, s.db, "SELECT status FROM tasks WHERE id='"+task.ID+"'", "todo")
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversations", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands", int64(0))
}
