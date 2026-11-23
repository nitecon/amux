package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
)

func workerRunFixture(t *testing.T) (*Store, *WorkerRun) {
	t.Helper()
	s := testStore(t)
	run := WorkerRun{ID: uuid.NewString(), Server: "http://127.0.0.1:7913", Project: "demo", ConversationID: uuid.NewString(), Directory: t.TempDir(), Owner: *taskActor()}
	if err := s.SaveWorkerRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return s, &run
}
func TestWorkerOutboxPreservesBindingOrderingAndAcknowledgements(t *testing.T) {
	ctx := context.Background()
	s, run := workerRunFixture(t)
	ready := conversation.Event{Kind: "session_ready", SessionID: "native"}
	first, err := s.QueueWorkerEvent(ctx, run.ID, ready)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.QueueWorkerEvent(ctx, run.ID, conversation.Event{Kind: "text_delta", SessionID: "native", Text: "offline output"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("event IDs were reused")
	}
	pending, err := s.WorkerOutbox(ctx, run.ID, 100)
	if err != nil || len(pending) != 3 || pending[0].Kind != "bind" || pending[1].ID != first || pending[2].ID != second {
		t.Fatalf("outbox %+v %v", pending, err)
	}
	if err = s.AckWorkerOutbox(ctx, run.ID, pending[2].Sequence, 0); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("out-of-order ack: %v", err)
	}
	if err = s.AckWorkerOutbox(ctx, run.ID, pending[0].Sequence, 0); err != nil {
		t.Fatal(err)
	}
	if err = s.AckWorkerOutbox(ctx, run.ID, pending[1].Sequence, 17); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.WorkerRun(ctx, run.ID)
	if err != nil || loaded.NativeSessionID != "native" || loaded.EventCursor != 17 {
		t.Fatalf("durable run %+v %v", loaded, err)
	}
	if _, err = s.QueueWorkerEvent(ctx, run.ID, conversation.Event{Kind: "session_ready", SessionID: "different"}); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("silent replacement: %v", err)
	}
}
func TestWorkerCommandJournalFencesRepeatedExecution(t *testing.T) {
	ctx := context.Background()
	s, run := workerRunFixture(t)
	c := ConversationCommand{ID: uuid.NewString(), ConversationID: run.ConversationID, Revision: 12, Sequence: 1, Command: conversation.Command{Kind: "prompt", Text: "Work"}, TargetInstanceID: run.Owner.InstanceID, State: "queued"}
	if err := s.ObserveWorkerCommand(ctx, run.ID, c); err != nil {
		t.Fatal(err)
	}
	if err := s.StartWorkerCommand(ctx, run.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveWorkerCommand(ctx, run.ID, c); err != nil {
		t.Fatal(err)
	}
	if err := s.StartWorkerCommand(ctx, run.ID, c.ID); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("command reexecuted: %v", err)
	}
	if err := s.CompleteWorkerCommand(ctx, run.ID, c.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteWorkerCommand(ctx, run.ID, c.ID, "done"); err != nil {
		t.Fatal(err)
	}
	pending, err := s.WorkerOutbox(ctx, run.ID, 100)
	if err != nil || len(pending) != 1 || pending[0].Kind != "result" {
		t.Fatalf("completion outbox %+v %v", pending, err)
	}
	loaded, err := s.WorkerRun(ctx, run.ID)
	if err != nil || loaded.CommandCursor != 12 {
		t.Fatalf("command cursor %+v %v", loaded, err)
	}
}
func TestWorkerInstanceRecoveryRequiresDrainedOutbox(t *testing.T) {
	ctx := context.Background()
	s, run := workerRunFixture(t)
	if _, err := s.QueueWorkerEvent(ctx, run.ID, conversation.Event{Kind: "session_ready", SessionID: "native"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareWorkerInstance(ctx, run.ID); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("rotated before old events drained: %v", err)
	}
	pending, _ := s.WorkerOutbox(ctx, run.ID, 100)
	for _, entry := range pending {
		if err := s.AckWorkerOutbox(ctx, run.ID, entry.Sequence, 0); err != nil {
			t.Fatal(err)
		}
	}
	prepared, err := s.PrepareWorkerInstance(ctx, run.ID)
	if err != nil || prepared.PendingInstanceID == "" {
		t.Fatalf("prepare %+v %v", prepared, err)
	}
	repeated, err := s.PrepareWorkerInstance(ctx, run.ID)
	if err != nil || repeated.PendingInstanceID != prepared.PendingInstanceID {
		t.Fatalf("planned instance changed %+v %v", repeated, err)
	}
	if err = s.ActivateWorkerInstance(ctx, run.ID, prepared.PendingInstanceID); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.WorkerRun(ctx, run.ID)
	if err != nil || loaded.Owner.InstanceID != prepared.PendingInstanceID || loaded.PendingInstanceID != "" {
		t.Fatalf("activate %+v %v", loaded, err)
	}
}

func TestWorkerRunLeaseRejectsDuplicateAndReleasesOnce(t *testing.T) {
	s := testStore(t)
	release, err := s.AcquireWorkerRun("run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcquireWorkerRun("run"); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("duplicate engine permitted: %v", err)
	}
	release()
	next, err := s.AcquireWorkerRun("run")
	if err != nil {
		t.Fatal(err)
	}
	defer next()
	release() // Old cleanup must not release a replacement engine's lease.
	if _, err = s.AcquireWorkerRun("run"); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("old cleanup released new engine: %v", err)
	}
}
