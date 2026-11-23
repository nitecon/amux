package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/router"
)

func insertNativeReceiptRetentionFixture(t *testing.T, s *Store, uid, status string, at int64, conversationID, commandID *string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO amux_native_delivery_receipts
		(notify_uid,project,task_id,receiver_json,expires_at,status,created_at,updated_at,conversation_id,command_id)
		VALUES(?,'tasks','retention-fixture','{}',?,?,?,?,?,?)`, uid, at, status, at, at, conversationID, commandID); err != nil {
		t.Fatal(err)
	}
}

func TestNativeDeliveryReceiptRetentionPreservesReplayAndUnfinishedCommands(t *testing.T) {
	ctx := context.Background()
	s, c, owner := commandConversation(t)
	done, err := s.EnqueueConversationCommand(ctx, c.ProjectIdent, c.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: "Completed intent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimConversationCommand(ctx, c.ProjectIdent, c.ID, done.ID, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishConversationCommand(ctx, c.ProjectIdent, c.ID, done.ID, "done", owner); err != nil {
		t.Fatal(err)
	}
	pending, err := s.EnqueueConversationCommand(ctx, c.ProjectIdent, c.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: "Offline durable intent"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	old := now - retentionDayMS - 1
	held, err := s.EmitRouterEvent(ctx, notificationRegistry(t), router.EmitRequest{ID: "agent.notify", Version: 1, Source: "fixture:retained",
		Payload: []byte(`{"target":"fixture","project":"tasks","mode":"recipient","require_idle":true,"event":{"kind":"task_created","task":{}}}`)})
	if err != nil || !held.Stored {
		t.Fatalf("held journal fixture: %+v %v", held, err)
	}
	if _, err := s.db.Exec("UPDATE router_events SET emitted_at=? WHERE uid=?", old, held.Event.UID); err != nil {
		t.Fatal(err)
	}
	insertNativeReceiptRetentionFixture(t, s, "old-closed", "completed", old, nil, nil)
	insertNativeReceiptRetentionFixture(t, s, "old-pending", "pending", old, nil, nil)
	insertNativeReceiptRetentionFixture(t, s, "old-queued-effect", "completed", old, &c.ID, &pending.ID)
	insertNativeReceiptRetentionFixture(t, s, "old-done-effect", "completed", old, &c.ID, &done.ID)
	insertNativeReceiptRetentionFixture(t, s, "recent-closed", "completed", now, nil, nil)
	insertNativeReceiptRetentionFixture(t, s, held.Event.UID, "completed", old, nil, nil)
	event := router.Event{ID: "retention.startup", Version: 1, UID: uuid.NewString(), EmittedAt: now}
	if ran, _, err := s.ReconcileRouterRetention(ctx, event, 30, 30); err != nil || !ran {
		t.Fatalf("receipt cleanup: ran=%v %v", ran, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_native_delivery_receipts", int64(4))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_native_delivery_receipts WHERE notify_uid IN ('old-closed','old-done-effect')", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands WHERE state='queued'", int64(1))
	if _, err := s.ClaimConversationCommand(ctx, c.ProjectIdent, c.ID, pending.ID, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishConversationCommand(ctx, c.ProjectIdent, c.ID, pending.ID, "done", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE router_deliveries SET state='completed' WHERE event_seq=?", held.Event.Seq); err != nil {
		t.Fatal(err)
	}
	event.UID, event.EmittedAt = uuid.NewString(), now+1
	if ran, _, err := s.ReconcileRouterRetention(ctx, event, 30, 30); err != nil || !ran {
		t.Fatalf("finished receipt cleanup: ran=%v %v", ran, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_native_delivery_receipts", int64(2))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_native_delivery_receipts WHERE notify_uid IN ('old-pending','recent-closed')", int64(2))
}

func TestNativeDeliveryReceiptRetentionRollsBackWithCheckpoint(t *testing.T) {
	s := taskStore(t)
	now := time.Now().UnixMilli()
	insertNativeReceiptRetentionFixture(t, s, "rollback-receipt", "completed", now-retentionDayMS-1, nil, nil)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_native_retention_checkpoint BEFORE INSERT ON retention_runs
		BEGIN SELECT RAISE(ABORT,'injected checkpoint failure'); END`); err != nil {
		t.Fatal(err)
	}
	event := router.Event{ID: "retention.startup", Version: 1, UID: uuid.NewString(), EmittedAt: now}
	if _, _, err := s.ReconcileRouterRetention(context.Background(), event, 30, 30); err == nil {
		t.Fatal("receipt/checkpoint failure was swallowed")
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_native_delivery_receipts", int64(1))
	if _, err := s.db.Exec("DROP TRIGGER reject_native_retention_checkpoint"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReconcileRouterRetention(context.Background(), event, 30, 30); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_native_delivery_receipts", int64(0))
}
