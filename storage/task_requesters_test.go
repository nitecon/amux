package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
)

func requesterTestFixture(t *testing.T) (*Store, context.Context, *AgentOrigin, RegisteredTargetIdentity) {
	t.Helper()
	ctx := context.Background()
	s := taskStore(t)
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='adhoc' WHERE ident='tasks'"); err != nil {
		t.Fatal(err)
	}
	actor := taskActor()
	registered, err := s.RegisterTarget(ctx, "tasks", TargetRegistration{
		Origin: *actor, Generation: "99999999-9999-4999-8999-999999999999",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, actor, registered.Target
}

func TestInsertTaskRequesterValidatesAndSnapshotsCurrentTarget(t *testing.T) {
	s, ctx, actor, requester := requesterTestFixture(t)
	created, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Subscribe", Reporter: "fixture", Requester: &requester}, actor)
	if err != nil {
		t.Fatal(err)
	}
	var stored RegisteredTargetIdentity
	if err := json.Unmarshal(created.Requester, &stored); err != nil || stored != requester {
		t.Fatalf("stored requester=%+v err=%v", stored, err)
	}
	events, err := s.TaskEvents(ctx, EventQuery{TaskID: &created.ID})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, event := range events {
		if event.Kind != "task_created" {
			continue
		}
		var snapshot Task
		if err := json.Unmarshal(event.Task, &snapshot); err != nil {
			t.Fatal(err)
		}
		var eventRequester RegisteredTargetIdentity
		if err := json.Unmarshal(snapshot.Requester, &eventRequester); err != nil || eventRequester != requester {
			t.Fatalf("created event requester=%+v err=%v", eventRequester, err)
		}
		found = true
	}
	if !found {
		t.Fatal("task creation event missing")
	}

	foreign := *taskActor()
	foreign.SessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	before := 0
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE project_ident='tasks'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Wrong origin", Reporter: "fixture", Requester: &requester}, &foreign); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("foreign native requester origin error=%v", err)
	}
	var after int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE project_ident='tasks'").Scan(&after); err != nil || after != before {
		t.Fatalf("invalid requester insert changed task count %d -> %d err=%v", before, after, err)
	}
}

func TestSetTaskRequesterRequiresOwnerOrPriorRequesterAndChangesOnlySubscription(t *testing.T) {
	s, ctx, actor, requester := requesterTestFixture(t)
	task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Watch result", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assigned, err := s.SetTaskRequester(ctx, "tasks", task.ID, &requester, nil)
	if err != nil || len(assigned.Requester) == 0 || assigned.Status != "todo" || assigned.OwnerOrigin != nil {
		t.Fatalf("human requester assignment changed unrelated task state: %+v err=%v", assigned, err)
	}

	foreign := *actor
	foreign.InstanceID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	if _, err := s.SetTaskRequester(ctx, "tasks", task.ID, &requester, &foreign); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("different native origin changed requester: %v", err)
	}

	// The current requester can manage its subscription, but native writes may
	// only name that same exact origin.
	updated, err := s.SetTaskRequester(ctx, "tasks", task.ID, &requester, actor)
	if err != nil || updated.Status != "todo" || updated.OwnerOrigin != nil {
		t.Fatalf("requester update changed task workflow: %+v err=%v", updated, err)
	}
	other := requester
	other.Origin.InstanceID = foreign.InstanceID
	if _, err := s.SetTaskRequester(ctx, "tasks", task.ID, &other, actor); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("native requester rebound to another origin: %v", err)
	}
	cleared, err := s.SetTaskRequester(ctx, "tasks", task.ID, nil, actor)
	if err != nil || len(cleared.Requester) != 0 || cleared.Status != "todo" || cleared.OwnerOrigin != nil {
		t.Fatalf("requester clear changed task workflow: %+v err=%v", cleared, err)
	}
	var raw any
	if err := s.db.QueryRowContext(ctx, "SELECT requester_json FROM tasks WHERE id=?", task.ID).Scan(&raw); err != nil || raw != nil {
		t.Fatalf("cleared requester persisted as %#v err=%v", raw, err)
	}

	owned, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Owner manages own requester", Reporter: "fixture"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	owned, err = s.SetTaskRequester(ctx, "tasks", owned.ID, &requester, actor)
	if err != nil || owned.OwnerOrigin == nil || *owned.OwnerOrigin != *actor || !jsonEqualRequester(owned.Requester, requester) {
		t.Fatalf("owner could not select self as requester: %+v err=%v", owned, err)
	}
	owned, err = s.SetTaskRequester(ctx, "tasks", owned.ID, nil, actor)
	if err != nil || owned.Status != "todo" || owned.OwnerOrigin == nil || *owned.OwnerOrigin != *actor || len(owned.Requester) != 0 {
		t.Fatalf("owner could not clear requester without changing work: %+v err=%v", owned, err)
	}
}

func TestNativeRequesterResultSkipsWhenSubscriptionChangesWhileDrafted(t *testing.T) {
	for _, change := range []string{"removed", "replaced", "OS retagged", "goal retagged"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s, ctx, requesterOrigin, requester := requesterTestFixture(t)
			goal := "review"
			task, err := s.InsertTask(ctx, TaskInsert{
				Project: "tasks", Title: "Requested review", Reporter: "fixture",
				Labels: []string{"os:linux"}, Goal: &goal, Requester: &requester,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			owner := *taskActor()
			owner.SessionID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
			owner.InstanceID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
			if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", &owner); err != nil {
				t.Fatal(err)
			}
			requesterWork, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Requester current work", Reporter: "fixture", Goal: &goal}, requesterOrigin)
			if err != nil {
				t.Fatal(err)
			}
			conversation, err := s.CreateConversationWithGoal(ctx, "tasks", requesterWork.ID, requesterOrigin, &goal)
			if err != nil {
				t.Fatal(err)
			}
			task, err = readTask(ctx, s.db, "tasks", task.ID)
			if err != nil {
				t.Fatal(err)
			}
			generation := requester.Generation
			draft := true
			presenceGoal := goal
			if _, err := s.ReportPresenceWithPrincipal(ctx, PresenceReport{Source: "native:delivery-test", Sequence: 1, Agents: []PresenceAgent{{
				ProjectIdent: "tasks", Origin: *requesterOrigin, Generation: &generation, Goal: &presenceGoal,
				Busy: true, Draft: draft,
			}}}, time.Now(), "client:delivery-test"); err != nil {
				t.Fatal(err)
			}
			registry := nativeDeliveryRegistry(t)
			payload := mustJSONBytes(t, map[string]any{
				"target": requesterOrigin.SessionID, "project": "tasks", "mode": "recipient", "require_idle": true,
				"heading": "Review result", "instructions": "Inspect the requested result.",
				"event": map[string]any{"kind": "task_commented", "task": task, "comment": map[string]any{"author": "owner", "content": "Review is ready"}},
			})
			emitted, err := s.EmitRouterEvent(ctx, registry, router.EmitRequest{ID: plugins.NotifyEventID, Version: 1, Source: "plugin:reminder", Payload: payload})
			if err != nil || !emitted.Stored {
				t.Fatalf("emit requester result: %+v err=%v", emitted, err)
			}
			if pending, err := s.ReconcileNativeDelivery(ctx, registry, emitted.Event); err != nil || !pending {
				t.Fatalf("drafted result must wait: pending=%t err=%v", pending, err)
			}

			switch change {
			case "removed":
				if _, err := s.SetTaskRequester(ctx, "tasks", task.ID, nil, nil); err != nil {
					t.Fatal(err)
				}
			case "replaced":
				replacement := *taskActor()
				replacement.SessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
				replacement.InstanceID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
				registered, err := s.RegisterTarget(ctx, "tasks", TargetRegistration{Origin: replacement, Generation: "88888888-8888-4888-8888-888888888888"}, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.SetTaskRequester(ctx, "tasks", task.ID, &registered.Target, nil); err != nil {
					t.Fatal(err)
				}
			case "OS retagged":
				if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET labels='["os:windows"]' WHERE id=?`, task.ID); err != nil {
					t.Fatal(err)
				}
			case "goal retagged":
				if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET goal='different' WHERE id=?", task.ID); err != nil {
					t.Fatal(err)
				}
			}

			draft = false
			if _, err := s.ReportPresenceWithPrincipal(ctx, PresenceReport{Source: "native:delivery-test", Sequence: 2, Agents: []PresenceAgent{{
				ProjectIdent: "tasks", Origin: *requesterOrigin, Generation: &generation, Goal: &presenceGoal,
				Busy: true, Draft: draft,
			}}}, time.Now(), "client:delivery-test"); err != nil {
				t.Fatal(err)
			}
			if pending, err := s.ReconcileNativeDelivery(ctx, registry, emitted.Event); err != nil || pending {
				t.Fatalf("changed requester result should terminate skipped: pending=%t err=%v", pending, err)
			}
			assertEqual(t, s.db, "SELECT status FROM amux_native_delivery_receipts WHERE notify_uid='"+emitted.Event.UID+"'", "skipped")
			assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands WHERE conversation_id='"+conversation.ID+"' AND kind='prompt'", int64(0))
			assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events WHERE id='agent.notify_result' AND correlation='"+emitted.Event.UID+"'", int64(1))
		})
	}
}
