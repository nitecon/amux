package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/nitecon/amux/fism"
)

func TestFinishRouterDeliveryFailureReplayIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	registry := routerTestRegistry(t, "worker")
	result, err := s.EmitRouterEvent(ctx, registry, routerTestRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	now := result.Event.EmittedAt
	token, err := s.AcquireRouterRuntime(ctx, registry, now)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := s.ClaimRouterDeliveries(ctx, token, registry, now, 1)
	if err != nil || len(batch.Runs) != 1 {
		t.Fatalf("claim = %+v, %v", batch, err)
	}
	run := batch.Runs[0]
	outcome := fism.Outcome{Kind: fism.Fail, Error: "fixture failure"}
	if err := s.FinishRouterDelivery(ctx, token, registry, run, outcome, now); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRouterDelivery(ctx, token, registry, run, outcome, now+1); err != nil {
		t.Fatalf("replayed finish: %v", err)
	}
	deliveries, err := s.RouterDeliveries(ctx, result.Event.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].State != "retrying" || deliveries[0].Attempts != 1 {
		t.Fatalf("failure was recorded more than once: %+v", deliveries)
	}
}

func TestRouterRetryCapMarshalsControlCharactersAsJSON(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	registry, err := fism.NewRegistry([]fism.EventDefinition{{ID: "test.event", Version: 1, Validate: fism.Object}}, []fism.ControllerDefinition{{ID: "worker", Subscriptions: []fism.Subscription{
		{Event: "test.event", MinVersion: 1, MaxVersion: 1},
		{Event: "router.retrying", MinVersion: 1, MaxVersion: 1},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	emitted, err := s.EmitRouterEvent(ctx, registry, fism.EmitRequest{ID: "test.event", Version: 1, Source: "test", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	now := emitted.Event.EmittedAt
	token, err := s.AcquireRouterRuntime(ctx, registry, now)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := s.ClaimRouterDeliveries(ctx, token, registry, now, 1)
	if err != nil || len(batch.Runs) != 1 {
		t.Fatalf("claim = %+v, %v", batch, err)
	}
	run := batch.Runs[0]
	controlError := "offline\x00\x1b\n"
	for failure := int64(1); failure <= 7; failure++ {
		if failure > 1 {
			if _, err := s.db.ExecContext(ctx, `UPDATE router_deliveries SET state='running' WHERE event_seq=? AND plugin='worker'`, run.Event.Seq); err != nil {
				t.Fatal(err)
			}
			run.Failures = failure - 1
		}
		if err := s.FinishRouterDelivery(ctx, token, registry, run, fism.Outcome{Kind: fism.Fail, Error: controlError}, now+failure); err != nil {
			t.Fatalf("finish failure %d: %v", failure, err)
		}
		if failure == 5 {
			assertEqual(t, s.db, `SELECT COUNT(*) FROM router_events WHERE id='router.retrying'`, int64(0))
		}
	}
	assertEqual(t, s.db, `SELECT COUNT(*) FROM router_events WHERE id='router.retrying'`, int64(1))
	events, err := s.RouterEvents(ctx, emitted.Event.Seq, 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, event := range events {
		if event.ID != "router.retrying" {
			continue
		}
		var payload struct {
			Error    string `json:"error"`
			Attempts int64  `json:"attempts"`
			Plugin   string `json:"plugin"`
			EventUID string `json:"event_uid"`
			Event    string `json:"event"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("retry payload is not JSON: %q: %v", event.Payload, err)
		}
		if payload.Attempts != 6 || payload.Plugin != "worker" || payload.EventUID != emitted.Event.UID || payload.Event != "test.event" || event.Correlation == nil || *event.Correlation != emitted.Event.UID {
			t.Fatalf("retry visibility provenance %+v event %+v", payload, event)
		}
		if payload.Error != controlError {
			t.Fatalf("retry error = %q, want %q", payload.Error, controlError)
		}
		found = true
	}
	if !found {
		t.Fatal("router.retrying event missing")
	}
}

func TestCanceledRouterReleaseFencesOldTokenAndRecoversRunningDelivery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	registry := routerTestRegistry(t, "worker")
	result, err := s.EmitRouterEvent(ctx, registry, routerTestRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	now := result.Event.EmittedAt
	oldToken, err := s.AcquireRouterRuntime(ctx, registry, now)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimRouterDeliveries(ctx, oldToken, registry, now, 1)
	if err != nil || len(claimed.Runs) != 1 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.ReleaseRouterRuntime(canceled, oldToken); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled release error = %v", err)
	}
	if _, err := s.ClaimRouterDeliveries(ctx, oldToken, registry, now+1, 1); !errors.Is(err, fism.ErrRuntimeInactive) {
		t.Fatalf("old claim error = %v", err)
	}
	newToken, err := s.AcquireRouterRuntime(ctx, registry, now+2)
	if err != nil {
		t.Fatalf("new acquire: %v", err)
	}
	if newToken == oldToken {
		t.Fatal("runtime token was reused")
	}
	newBatch, err := s.ClaimRouterDeliveries(ctx, newToken, registry, now+2, 1)
	if err != nil || len(newBatch.Runs) != 1 {
		t.Fatalf("recovered claim = %+v, %v", newBatch, err)
	}
	if newBatch.Runs[0].Event.UID != result.Event.UID {
		t.Fatal("recovery claimed a different event")
	}
}

func TestRouterFIFOHeadDoesNotBlockParallelPlugin(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	registry, err := fism.NewRegistry([]fism.EventDefinition{{ID: "test.event", Version: 1, Validate: fism.Object}}, []fism.ControllerDefinition{
		{ID: "a-fifo", FIFO: true, Subscriptions: []fism.Subscription{{Event: "test.event", MinVersion: 1, MaxVersion: 1}}},
		{ID: "z-parallel", Subscriptions: []fism.Subscription{{Event: "test.event", MinVersion: 1, MaxVersion: 1}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.EmitRouterEvent(ctx, registry, fism.EmitRequest{ID: "test.event", Version: 1, Source: "test", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		if _, err := s.EmitRouterEvent(ctx, registry, fism.EmitRequest{ID: "test.event", Version: 1, Source: "test", Payload: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	baseNow := first.Event.EmittedAt
	if _, err := s.db.ExecContext(ctx, `UPDATE router_deliveries SET state='retrying',next_attempt_at=? WHERE event_seq=? AND plugin='a-fifo'`, baseNow+5000, first.Event.Seq); err != nil {
		t.Fatal(err)
	}
	token, err := s.AcquireRouterRuntime(ctx, registry, baseNow)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := s.ClaimRouterDeliveries(ctx, token, registry, baseNow, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Runs) != 1 || batch.Runs[0].Plugin != "z-parallel" || batch.Runs[0].Event.Seq != first.Event.Seq {
		t.Fatalf("parallel claim blocked by FIFO backlog: %+v", batch)
	}
	if batch.NextAttemptAt == nil || *batch.NextAttemptAt != baseNow+5000 {
		t.Fatalf("next attempt = %v", batch.NextAttemptAt)
	}
}

func TestRouterDependenciesReadyVetoAndFailureExhaustion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	registry, err := fism.NewRegistry([]fism.EventDefinition{{ID: "test.event", Version: 1, Validate: fism.Object}}, []fism.ControllerDefinition{
		{ID: "base", MaxAttempts: 1, Subscriptions: []fism.Subscription{{Event: "test.event", MinVersion: 1, MaxVersion: 1}}},
		{ID: "dependent", Subscriptions: []fism.Subscription{{Event: "test.event", MinVersion: 1, MaxVersion: 1, DependsOn: []string{"base"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.EmitRouterEvent(ctx, registry, fism.EmitRequest{ID: "test.event", Version: 1, Source: "test", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	now := first.Event.EmittedAt
	token, err := s.AcquireRouterRuntime(ctx, registry, now)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimRouterDeliveries(ctx, token, registry, now, 2)
	if err != nil || len(claimed.Runs) != 1 || claimed.Runs[0].Plugin != "base" {
		t.Fatalf("initial dependency claim = %+v, %v", claimed, err)
	}
	if err := s.FinishRouterDelivery(ctx, token, registry, claimed.Runs[0], fism.Outcome{Kind: fism.Continue}, now); err != nil {
		t.Fatal(err)
	}
	claimed, err = s.ClaimRouterDeliveries(ctx, token, registry, now, 2)
	if err != nil || len(claimed.Runs) != 1 || claimed.Runs[0].Plugin != "dependent" {
		t.Fatalf("ready dependency claim = %+v, %v", claimed, err)
	}
	if err := s.FinishRouterDelivery(ctx, token, registry, claimed.Runs[0], fism.Outcome{Kind: fism.Continue}, now); err != nil {
		t.Fatal(err)
	}

	for _, outcome := range []fism.Outcome{{Kind: fism.Stop}, {Kind: fism.Fail, Error: "exhausted"}} {
		emitted, err := s.EmitRouterEvent(ctx, registry, fism.EmitRequest{ID: "test.event", Version: 1, Source: "test", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := s.ClaimRouterDeliveries(ctx, token, registry, emitted.Event.EmittedAt, 2)
		if err != nil || len(claimed.Runs) != 1 || claimed.Runs[0].Plugin != "base" {
			t.Fatalf("veto/exhaustion base claim = %+v, %v", claimed, err)
		}
		if err := s.FinishRouterDelivery(ctx, token, registry, claimed.Runs[0], outcome, emitted.Event.EmittedAt); err != nil {
			t.Fatal(err)
		}
		claimed, err = s.ClaimRouterDeliveries(ctx, token, registry, emitted.Event.EmittedAt, 2)
		if err != nil || len(claimed.Runs) != 0 || !claimed.Skipped {
			t.Fatalf("veto/exhaustion dependent claim = %+v, %v", claimed, err)
		}
		rows, err := s.RouterDeliveries(ctx, emitted.Event.Seq)
		if err != nil {
			t.Fatal(err)
		}
		if rows[1].State != "skipped" {
			t.Fatalf("dependent state after veto/exhaustion = %q", rows[1].State)
		}
		if outcome.Kind == fism.Fail && rows[0].State != "failed" {
			t.Fatalf("exhausted base state = %q", rows[0].State)
		}
	}
}

func TestRouterRecoverySkipsRemovedSubscriptionVersion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	events := []fism.EventDefinition{
		{ID: "test.versioned", Version: 1, Validate: fism.Object},
		{ID: "test.versioned", Version: 2, Validate: fism.Object},
	}
	oldRegistry, err := fism.NewRegistry(events, []fism.ControllerDefinition{{ID: "worker", Subscriptions: []fism.Subscription{{Event: "test.versioned", MinVersion: 1, MaxVersion: 2}}}})
	if err != nil {
		t.Fatal(err)
	}
	emitted, err := s.EmitRouterEvent(ctx, oldRegistry, fism.EmitRequest{ID: "test.versioned", Version: 2, Source: "test", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	newRegistry, err := fism.NewRegistry(events, []fism.ControllerDefinition{{ID: "worker", Subscriptions: []fism.Subscription{{Event: "test.versioned", MinVersion: 1, MaxVersion: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireRouterRuntime(ctx, newRegistry, emitted.Event.EmittedAt+1); err != nil {
		t.Fatal(err)
	}
	rows, err := s.RouterDeliveries(ctx, emitted.Event.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != "skipped" || rows[0].LastError == nil || *rows[0].LastError != "subscription is not registered" {
		t.Fatalf("removed subscription recovery = %+v", rows)
	}
}

func TestRouterClaimPagesDelayedParallelBacklogAndHonorsCapacity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	registry := routerTestRegistry(t, "parallel")
	var last fism.EmitResult
	for i := 0; i < 300; i++ {
		var err error
		last, err = s.EmitRouterEvent(ctx, registry, routerTestRequest(`{}`))
		if err != nil {
			t.Fatal(err)
		}
	}
	now := last.Event.EmittedAt
	if _, err := s.db.ExecContext(ctx, `UPDATE router_deliveries SET state='retrying',next_attempt_at=? WHERE event_seq<?`, now+5000, last.Event.Seq); err != nil {
		t.Fatal(err)
	}
	token, err := s.AcquireRouterRuntime(ctx, registry, now)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := s.ClaimRouterDeliveries(ctx, token, registry, now, 1)
	if err != nil || len(batch.Runs) != 1 || batch.Runs[0].Event.Seq != last.Event.Seq {
		t.Fatalf("late eligible row behind delayed backlog = %+v, %v", batch, err)
	}
	if batch.NextAttemptAt == nil || *batch.NextAttemptAt != now+5000 {
		t.Fatalf("delayed backlog next time = %v", batch.NextAttemptAt)
	}
	if err := s.ReleaseRouterRuntime(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE router_deliveries SET state='pending',next_attempt_at=?`, now); err != nil {
		t.Fatal(err)
	}
	token, err = s.AcquireRouterRuntime(ctx, registry, now)
	if err != nil {
		t.Fatal(err)
	}
	batch, err = s.ClaimRouterDeliveries(ctx, token, registry, now, 7)
	if err != nil || len(batch.Runs) != 7 {
		t.Fatalf("capacity-limited claim = %d runs, %v", len(batch.Runs), err)
	}
}

func TestRouterFIFORunningAndRetryingHeadAcrossEventTypes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	registry, err := fism.NewRegistry([]fism.EventDefinition{
		{ID: "test.first", Version: 1, Validate: fism.Object},
		{ID: "test.second", Version: 1, Validate: fism.Object},
	}, []fism.ControllerDefinition{{ID: "fifo", FIFO: true, Subscriptions: []fism.Subscription{
		{Event: "test.first", MinVersion: 1, MaxVersion: 1},
		{Event: "test.second", MinVersion: 1, MaxVersion: 1},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.EmitRouterEvent(ctx, registry, fism.EmitRequest{ID: "test.first", Version: 1, Source: "test", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.EmitRouterEvent(ctx, registry, fism.EmitRequest{ID: "test.second", Version: 1, Source: "test", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	now := second.Event.EmittedAt
	token, err := s.AcquireRouterRuntime(ctx, registry, now)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := s.ClaimRouterDeliveries(ctx, token, registry, now, 1)
	if err != nil || len(batch.Runs) != 1 || batch.Runs[0].Event.Seq != first.Event.Seq {
		t.Fatalf("initial FIFO claim = %+v, %v", batch, err)
	}
	blocked, err := s.ClaimRouterDeliveries(ctx, token, registry, now, 1)
	if err != nil || len(blocked.Runs) != 0 {
		t.Fatalf("running FIFO head failed to block next event type: %+v, %v", blocked, err)
	}
	if err := s.FinishRouterDelivery(ctx, token, registry, batch.Runs[0], fism.Outcome{Kind: fism.Fail, Error: "retry"}, now); err != nil {
		t.Fatal(err)
	}
	blocked, err = s.ClaimRouterDeliveries(ctx, token, registry, now+500, 1)
	if err != nil || len(blocked.Runs) != 0 || blocked.NextAttemptAt == nil {
		t.Fatalf("retrying FIFO head failed to block next event type: %+v, %v", blocked, err)
	}
	ready, err := s.ClaimRouterDeliveries(ctx, token, registry, now+1000, 1)
	if err != nil || len(ready.Runs) != 1 || ready.Runs[0].Event.Seq != first.Event.Seq {
		t.Fatalf("due FIFO head was not retried before next event: %+v, %v", ready, err)
	}
	if ready.Runs[0].Event.ID != "test.first" || second.Event.ID != "test.second" {
		t.Fatal("FIFO order did not span event types")
	}
}

func TestCanceledQueuedRouterClaimCannotMutateAfterRelease(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	registry := routerTestRegistry(t, "worker")
	emitted, err := s.EmitRouterEvent(ctx, registry, routerTestRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.AcquireRouterRuntime(ctx, registry, emitted.Event.EmittedAt)
	if err != nil {
		t.Fatal(err)
	}
	lease := s.routerLease()
	lease.mu.Lock()
	claimCtx, cancel := context.WithCancel(ctx)
	started := make(chan struct{})
	claimDone := make(chan error, 1)
	go func() {
		close(started)
		_, err := s.ClaimRouterDeliveries(claimCtx, token, registry, emitted.Event.EmittedAt+1, 1)
		claimDone <- err
	}()
	<-started
	cancel()
	lease.mu.Unlock()
	if err := <-claimDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued claim error = %v", err)
	}
	rows, err := s.RouterDeliveries(ctx, emitted.Event.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != "pending" {
		t.Fatalf("canceled queued claim mutated delivery: %+v", rows)
	}
}
