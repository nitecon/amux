package storage

import (
	"context"
	"testing"
	"time"

	"github.com/nitecon/amux/eventbus"
)

func TestAssignmentWorkerAssignsOnTickAndStops(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	task := makeTask(t, s, nil)
	activity := time.Now().Add(-time.Minute).UnixMilli()
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "host", Sequence: 1, Agents: []PresenceAgent{{ProjectIdent: "tasks", Origin: *taskActor(), LastActivityAt: &activity, AutoAssign: true}}}, time.Now()); err != nil || !ok {
		t.Fatalf("report %v %v", ok, err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	ready := make(chan struct{}, 1)
	go func() { done <- s.runAssignmentWorker(runCtx, ready) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("assignment worker did not stop")
		}
	}()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("worker not ready")
	}
	current, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || current.Task.Status != "todo" {
		t.Fatalf("startup assigned task %+v %v", current, err)
	}
	// Unrelated channel notifications must not act as scheduler ticks.
	for _, topic := range []eventbus.Topic{eventbus.TaskChanged, eventbus.ConversationChanged, eventbus.PresenceChanged} {
		if err := s.EventBus().Publish(eventbus.Event{Topic: topic}); err != nil {
			t.Fatal(err)
		}
	}
	changes := s.TaskEventChanges()
	select {
	case <-changes:
		t.Fatal("notification assigned task without tick")
	case <-time.After(50 * time.Millisecond):
	}
	if err := s.EventBus().Publish(eventbus.Event{Topic: eventbus.Tick}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("tick did not assign task")
	}
	current, err = s.Task(ctx, "tasks", task.ID)
	if err != nil || current.Task.Status != "in_progress" || current.Task.OwnerOrigin == nil || *current.Task.OwnerOrigin != *taskActor() {
		t.Fatalf("assignment %+v %v", current, err)
	}
}
