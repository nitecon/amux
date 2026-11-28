package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nitecon/amux/fism"
)

func TestTaskReconciliationCommitsOnlyCompleteOutcomes(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if err := store.InsertProject(ctx, Project{Ident: "fixture", ChannelName: "fixture", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertTask(ctx, TaskInsert{Project: "fixture", Title: "accepted state", Reporter: "test"}, nil); err != nil {
		t.Fatal(err)
	}
	changes, err := store.PendingTaskChanges(ctx, 10)
	if err != nil || len(changes) != 1 {
		t.Fatalf("accepted changes=%+v err=%v", changes, err)
	}
	change := changes[0]
	registry, err := fism.NewRegistry(RouterTaskEventDefinitions(), []fism.ControllerDefinition{
		{ID: "audit", Subscriptions: []fism.Subscription{{Event: "task.created", MinVersion: 1, MaxVersion: 1}}},
		{ID: "notify", Subscriptions: []fism.Subscription{{Event: "task.created", MinVersion: 1, MaxVersion: 1}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := store.PublishTaskChange(ctx, registry, change)
	if err != nil {
		t.Fatal(err)
	}
	// A publication with an unsettled source must survive retention even
	// though it has no intermediate delivery records to protect it.
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := pruneRouterRetentionIn(ctx, tx, time.Now().Add(time.Hour).UnixMilli(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	published, err := store.PublishTaskChange(ctx, registry, change)
	if err != nil || published.Seq != event.Seq {
		t.Fatalf("publication was not retained: %+v %v", published, err)
	}
	outcomes := []fism.Delivery{{Plugin: "audit", State: "completed"}, {Plugin: "notify", State: "running"}}
	if err := store.SettleTaskChange(ctx, change.ID, event, outcomes); err == nil {
		t.Fatal("settlement accepted intermediate controller state")
	}
	deliveries, err := store.RouterDeliveries(ctx, event.Seq)
	if err != nil || len(deliveries) != 0 {
		t.Fatalf("partial outcome committed: %+v %v", deliveries, err)
	}
	changes, err = store.PendingTaskChanges(ctx, 10)
	if err != nil || len(changes) != 1 {
		t.Fatalf("failed settlement deleted accepted state: %+v %v", changes, err)
	}
	outcomes[1].State = "completed"
	if err := store.SettleTaskChange(ctx, change.ID, event, outcomes); err != nil {
		t.Fatal(err)
	}
	deliveries, err = store.RouterDeliveries(ctx, event.Seq)
	if err != nil || len(deliveries) != 2 {
		t.Fatalf("settled outcomes=%+v err=%v", deliveries, err)
	}
	changes, err = store.PendingTaskChanges(ctx, 10)
	if err != nil || len(changes) != 0 {
		t.Fatalf("settled changes still pending: %+v %v", changes, err)
	}
	if _, err := store.InsertTask(ctx, TaskInsert{Project: "fixture", Title: "new accepted state", Reporter: "test"}, nil); err != nil {
		t.Fatal(err)
	}
	changes, err = store.PendingTaskChanges(ctx, 10)
	if err != nil || len(changes) != 1 {
		t.Fatalf("new pending change: %+v %v", changes, err)
	}
	newID := changes[0].ID
	if err := store.SettleTaskChange(ctx, newID, event, outcomes); err == nil {
		t.Fatal("stale controller outcomes settled a different accepted change")
	}
	changes, err = store.PendingTaskChanges(ctx, 10)
	if err != nil || len(changes) != 1 || changes[0].ID != newID {
		t.Fatalf("stale settlement changed pending state: %+v %v", changes, err)
	}
}

func TestTaskReconciliationRestartsWithCurrentControllers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "recovery.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	if err := s.InsertProject(ctx, Project{Ident: "fixture", ChannelName: "fixture", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"settled", "before settlement", "during execution"} {
		if _, err := s.InsertTask(ctx, TaskInsert{Project: "fixture", Title: title, Reporter: "fixture"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, "CREATE TABLE fixture_effects(uid TEXT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	var recognized bool
	var newEvents []string
	var interruptUID string
	var interrupt context.CancelFunc
	makeRuntime := func(addController bool) *fism.Runtime {
		sub := []fism.Subscription{{Event: "task.created", MinVersion: 1, MaxVersion: 1}}
		controllers := []fism.Controller{{
			Definition: fism.ControllerDefinition{ID: "effect", Subscriptions: sub},
			Exec: []fism.Step{func(ctx context.Context, event fism.Event, _ *fism.StepContext) fism.Outcome {
				var exists bool
				err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM fixture_effects WHERE uid=?)", event.UID).Scan(&exists)
				if err != nil {
					return fism.Outcome{Kind: fism.Fail, Error: err.Error()}
				}
				if exists {
					recognized = true
					return fism.Outcome{Kind: fism.Continue}
				}
				if _, err := s.db.ExecContext(ctx, "INSERT INTO fixture_effects(uid) VALUES(?)", event.UID); err != nil {
					return fism.Outcome{Kind: fism.Fail, Error: err.Error()}
				}
				if event.UID == interruptUID {
					interrupt()
					return fism.Outcome{Kind: fism.Fail, Error: "interrupted during controller execution"}
				}
				return fism.Outcome{Kind: fism.Continue}
			}},
		}}
		if addController {
			controllers = append(controllers, fism.Controller{
				Definition: fism.ControllerDefinition{ID: "new", Subscriptions: sub},
				Exec: []fism.Step{func(_ context.Context, event fism.Event, _ *fism.StepContext) fism.Outcome {
					newEvents = append(newEvents, event.UID)
					return fism.Outcome{Kind: fism.Continue}
				}},
			})
		}
		runtime, err := fism.NewRuntime(s, s.EventBus(), RouterTaskEventDefinitions(), controllers, fism.RunConfig{})
		if err != nil {
			t.Fatal(err)
		}
		return runtime
	}

	initial := makeRuntime(false)
	pending, err := s.PendingTaskChanges(ctx, 10)
	if err != nil || len(pending) != 3 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	for i, change := range pending {
		event, err := s.PublishTaskChange(ctx, initial.Registry(), change)
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			pass, cancelPass := context.WithCancel(ctx)
			interruptUID, interrupt = event.UID, cancelPass
			outcomes, err := initial.ReconcileEvent(pass, event)
			cancelPass()
			if !errors.Is(err, context.Canceled) || len(outcomes) != 0 {
				t.Fatalf("interrupted controller returned outcomes=%+v error=%v", outcomes, err)
			}
			assertReviewCutoverCount(t, s, "SELECT COUNT(*) FROM router_deliveries WHERE event_seq=?", 0, event.Seq)
			continue
		}
		outcomes, err := initial.ReconcileEvent(ctx, event)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := s.SettleTaskChange(ctx, change.ID, event, outcomes); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s = reopened
	restarted := makeRuntime(true)
	pending, err = s.PendingTaskChanges(ctx, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("recovered pending=%+v err=%v", pending, err)
	}
	for i, change := range pending {
		recognized = false
		event, err := s.PublishTaskChange(ctx, restarted.Registry(), change)
		if err != nil {
			t.Fatal(err)
		}
		outcomes, err := restarted.ReconcileEvent(ctx, event)
		if err != nil {
			t.Fatal(err)
		}
		if !recognized || len(newEvents) != i+1 || newEvents[i] != event.UID || len(outcomes) != 2 {
			t.Fatalf("recognized=%t new events=%v outcomes=%+v", recognized, newEvents, outcomes)
		}
		if err := s.SettleTaskChange(ctx, change.ID, event, outcomes); err != nil {
			t.Fatal(err)
		}
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM fixture_effects", int64(3))
}
