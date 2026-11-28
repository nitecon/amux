package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/identity/agent"
)

func TestNotifierTaskSourceRoutesOwnerAndRequesterThenRejectsChangedGeneration(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	owner := taskActor()
	requester := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "claude", OS: "linux"}
	actor := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "antigravity", OS: "linux"}
	goal := "selected-goal"
	task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Notifier source work", Reporter: "fixture", Goal: &goal, Labels: []string{"os:linux"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", owner); err != nil {
		t.Fatal(err)
	}
	requesterBinding, err := json.Marshal(map[string]any{"origin": requester, "generation": requester.InstanceID})
	if err != nil {
		t.Fatal(err)
	}
	// The existing requester/routing records are retained across the port; their
	// full authoring API is separate from this source/recipient integration.
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET requester_json=?,rank=? WHERE id=?", string(requesterBinding), int64(9007199254740993), task.ID); err != nil {
		t.Fatal(err)
	}
	agents := []PresenceAgent{
		{ProjectIdent: "tasks", Origin: *owner, Goal: &goal, Generation: &owner.InstanceID, Busy: true},
		{ProjectIdent: "tasks", Origin: requester, Goal: &goal, Generation: &requester.InstanceID, Busy: true},
	}
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "native:notifier", Sequence: 1, Agents: agents}, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("report routing identities: %v %v", ok, err)
	}
	definitions := append(RouterTaskEventDefinitions(), controllers.NotificationEventDefinitions()...)
	runtime, err := fism.NewRuntime(s, s.EventBus(), definitions, []fism.Controller{controllers.NotifierController(s), {
		Definition: fism.ControllerDefinition{ID: "notification-sink", Subscriptions: []fism.Subscription{{Event: controllers.NotifyEventID, MinVersion: 1, MaxVersion: 1}}},
	}}, fism.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	stopRuntime := runRouterFixture(t, runtime)
	defer stopRuntime()
	stopSource := runOwnedRouterFixture(t, func(ctx context.Context) error {
		return controllers.RunTaskSource(ctx, s, runtime.Registry(), s.EventBus())
	})
	defer stopSource()
	comment, err := s.AddTaskComment(ctx, "tasks", task.ID, "fixture", "agent", "Progress for both recipients", nil, &actor)
	if err != nil {
		t.Fatal(err)
	}
	cause := awaitSourceEvent(t, s, "task.commented")
	waitRouterDeliveryState(t, s, cause.Seq, "notifier", "completed")
	if err := s.ReconcileRouterNotifiers(ctx, runtime.Registry(), cause); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT payload,source,correlation FROM router_events WHERE id='agent.notify' ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for rows.Next() {
		var raw, source, correlation string
		if err := rows.Scan(&raw, &source, &correlation); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		var notice struct {
			Target string `json:"target"`
			Idle   bool   `json:"require_idle"`
			Event  struct {
				Origin agent.Origin `json:"origin"`
				Task   struct {
					Rank json.Number `json:"rank"`
				} `json:"task"`
				Comment TaskComment `json:"comment"`
			} `json:"event"`
		}
		if err := json.Unmarshal([]byte(raw), &notice); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if source != "plugin:notifier" || correlation != cause.UID || notice.Event.Origin != actor || notice.Event.Comment.ID != comment.ID || notice.Event.Task.Rank.String() != "9007199254740993" {
			rows.Close()
			t.Fatalf("notifier source provenance: %s %s %s", raw, source, correlation)
		}
		if _, duplicate := seen[notice.Target]; duplicate {
			rows.Close()
			t.Fatalf("replay notified target twice: %s", notice.Target)
		}
		seen[notice.Target] = notice.Idle
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	ownerIdle, ownerPresent := seen[owner.SessionID]
	requesterIdle, requesterPresent := seen[requester.SessionID]
	if len(seen) != 2 || !ownerPresent || !requesterPresent || ownerIdle || !requesterIdle {
		t.Fatalf("owner/requester idle routing: %+v", seen)
	}
	action := "Review the submitted result."
	// Commit the review and its outbox entry without the post-commit bus
	// wakeup, as if the publishing process lost that hint.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	current, err := readTask(ctx, tx, "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyTaskWorkflow(ctx, tx, current, TaskWorkflowUpdate{
		Status: "user_review", ReviewAction: &action, Changeset: json.RawMessage(`null`),
	}, owner, s.routerReviews.Load()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_task_outbox WHERE kind='user_review'", int64(1))
	if err := s.EventBus().Publish(eventbus.Event{Topic: eventbus.Tick}); err != nil {
		t.Fatal(err)
	}
	review := awaitSourceEvent(t, s, "task.user_review")
	waitRouterDeliveryState(t, s, review.Seq, "notifier", "completed")
	if err := s.ReconcileRouterNotifiers(ctx, runtime.Registry(), review); err != nil {
		t.Fatal(err)
	}
	var count int
	var target string
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(json_extract(payload,'$.target')),'')
		FROM router_events WHERE id='agent.notify' AND correlation=? AND json_extract(payload,'$.event.kind')='task_user_review'`,
		review.UID).Scan(&count, &target); err != nil || count != 1 || target != requester.SessionID {
		t.Fatalf("review did not reach the requester once: count=%d target=%s error=%v", count, target, err)
	}
	newGeneration := uuid.NewString()
	agents[1].Generation = &newGeneration
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "native:notifier", Sequence: 2, Agents: agents}, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("new requester generation: %v %v", ok, err)
	}
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`null`)}, &actor); err != nil {
		t.Fatal(err)
	}
	completed := awaitSourceEvent(t, s, "task.completed")
	waitRouterDeliveryState(t, s, completed.Seq, "notifier", "completed")
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(MAX(json_extract(payload,'$.target')),'') FROM router_events WHERE id='agent.notify' AND correlation=?", completed.UID).Scan(&count, &target); err != nil || count != 1 || target != owner.SessionID {
		t.Fatalf("completion reached a stale requester: count=%d target=%s error=%v", count, target, err)
	}
}
