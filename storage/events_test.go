package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/nitecon/amux/eventbus"
)

func TestStorePublishesCommittedTaskAndConversationChanges(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	sub := s.EventBus().Subscribe(8)
	task := makeTask(t, s, taskActor())
	select {
	case event := <-sub.C:
		if event.Topic != eventbus.TaskChanged {
			t.Fatalf("task event topic = %q", event.Topic)
		}
		if _, err := s.Task(ctx, "tasks", task.ID); err != nil {
			t.Fatalf("task was not committed before publication: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("task commit did not publish")
	}
	conversation, err := s.CreateConversation(ctx, "tasks", task.ID, taskActor())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-sub.C:
		if event.Topic != eventbus.TaskChanged {
			t.Fatalf("initial claim event topic = %q", event.Topic)
		}
		detail, err := s.Task(ctx, "tasks", task.ID)
		if err != nil || detail.Task.Status != "in_progress" {
			t.Fatalf("initial task claim was not committed before publication: %+v %v", detail, err)
		}
	case <-time.After(time.Second):
		t.Fatal("initial task claim did not publish")
	}
	select {
	case event := <-sub.C:
		if event.Topic != eventbus.ConversationChanged {
			t.Fatalf("conversation event topic = %q", event.Topic)
		}
		if _, err := s.Conversation(ctx, "tasks", conversation.ID); err != nil {
			t.Fatalf("conversation was not committed before publication: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("conversation commit did not publish")
	}
	sub.Unsubscribe()
}

func appendFixtureEvent(t *testing.T, s *Store) int64 {
	t.Helper()
	id, err := s.AppendTaskEvent(context.Background(), TaskEvent{Kind: "task_created", ProjectIdent: "example", Task: json.RawMessage(`{"id":"task","status":"todo"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Receipt/replay portion of Rust task_events.rs::consumer_replay_fencing_and_idempotent_receipts_do_not_change_tasks.
// Canonical task mutation/trigger integration is separately pending.
func TestConsumerReplayAndReceiptFences(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first := appendFixtureEvent(t, s)
	sub, err := s.SubscribeTaskEvents(ctx, "a", "consumer", nil)
	if err != nil || sub.Cursor != first {
		t.Fatalf("subscribe %+v, %v", sub, err)
	}
	if _, err := s.SubscribeTaskEvents(ctx, "b", "consumer", nil); err == nil {
		t.Fatal("duplicate live connection accepted")
	}
	second := appendFixtureEvent(t, s)
	if _, err := s.AcknowledgeTaskEvent(ctx, "a", "consumer", EventReceipt{EventID: first, Status: "injected"}); err == nil {
		t.Fatal("unoffered event acknowledged")
	}
	event, err := s.OfferTaskEvent(ctx, "a", "consumer", first)
	if err != nil || event == nil || event.ID != second || len(event.Deliveries) != 0 {
		t.Fatalf("offer %+v, %v", event, err)
	}
	for _, pair := range [][2]string{{"queued", "queued"}, {"received", "queued"}, {"injected", "injected"}, {"queued", "injected"}} {
		status, err := s.AcknowledgeTaskEvent(ctx, "a", "consumer", EventReceipt{EventID: second, Status: pair[0]})
		if err != nil || status != pair[1] {
			t.Fatalf("ack %s = %s, %v", pair[0], status, err)
		}
	}
	if err := s.DisconnectTaskConsumer(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	sub, err = s.SubscribeTaskEvents(ctx, "b", "consumer", nil)
	if err != nil || sub.Cursor != second {
		t.Fatalf("resume %+v, %v", sub, err)
	}
	if _, err := s.OfferTaskEvent(ctx, "a", "consumer", second); err == nil {
		t.Fatal("stale connection retained ownership")
	}
	if err := s.DisconnectTaskConsumer(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TouchTaskConsumer(ctx, "b", "consumer"); err != nil {
		t.Fatal("stale disconnect revoked successor:", err)
	}
	if err := s.DisconnectTaskConsumer(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	sub, err = s.SubscribeTaskEvents(ctx, "c", "consumer", &zero)
	if err != nil || sub.Cursor != 0 {
		t.Fatalf("rewind %+v, %v", sub, err)
	}
	event, err = s.OfferTaskEvent(ctx, "c", "consumer", 0)
	if err != nil || event == nil || event.ID != first {
		t.Fatalf("rewind offer %+v, %v", event, err)
	}
}

func TestEventsPersistAndSubscriptionCannotSkip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "events.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	first := appendFixtureEvent(t, s)
	second := appendFixtureEvent(t, s)
	zero := int64(0)
	if _, err := s.SubscribeTaskEvents(ctx, "old", "persisted", &zero); err != nil {
		t.Fatal(err)
	}
	if err := s.DisconnectTaskConsumer(ctx, "old"); err != nil {
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
	if _, err := s.SubscribeTaskEvents(ctx, "new", "persisted", &second); err == nil {
		t.Fatal("consumer skipped unacknowledged events")
	}
	sub, err := s.SubscribeTaskEvents(ctx, "new", "persisted", nil)
	if err != nil || sub.Cursor != 0 {
		t.Fatalf("lost cursor: %+v, %v", sub, err)
	}
	events, err := s.TaskEvents(ctx, EventQuery{After: &zero})
	if err != nil || len(events) != 2 || events[0].ID != first || events[1].ID != second {
		t.Fatalf("replay %+v, %v", events, err)
	}
	events, err = s.TaskEvents(ctx, EventQuery{})
	if err != nil || len(events) != 2 || events[0].ID != second {
		t.Fatalf("newest %+v, %v", events, err)
	}
}

func TestEventWakeAndRollback(t *testing.T) {
	s := testStore(t)
	wake := s.TaskEventChanges()
	if _, err := s.AppendTaskEvent(context.Background(), TaskEvent{Kind: "bad", Task: json.RawMessage(`{`)}); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	select {
	case <-wake:
		t.Fatal("invalid append announced a change")
	default:
	}
	appendFixtureEvent(t, s)
	select {
	case <-wake:
	default:
		t.Fatal("committed event did not wake subscribers")
	}
	if next := s.TaskEventChanges(); next == wake {
		t.Fatal("closed change channel reused")
	}
}

func TestRestartRecoversConsumerLease(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	appendFixtureEvent(t, s)
	zero := int64(0)
	if _, err := s.SubscribeTaskEvents(ctx, "old", "worker", &zero); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverTaskConsumers(ctx); err != nil {
		t.Fatal(err)
	}
	sub, err := s.SubscribeTaskEvents(ctx, "new", "worker", nil)
	if err != nil || sub.Cursor != 0 {
		t.Fatalf("restart lost cursor: %+v, %v", sub, err)
	}
	if _, err := s.TouchTaskConsumer(ctx, "old", "worker"); err == nil {
		t.Fatal("old process lease survived restart")
	}
}
