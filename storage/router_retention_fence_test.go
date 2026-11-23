package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nitecon/amux/router"
)

func TestRouterRetentionFencesUnacknowledgedCompletion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	registry := routerTestRegistry(t, "worker")
	emitted, err := s.EmitRouterEvent(ctx, registry, routerTestRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	oldEnough := emitted.Event.EmittedAt - 2*retentionTestDay
	if _, err := s.db.ExecContext(ctx, `UPDATE router_events SET emitted_at=? WHERE seq=?`, oldEnough, emitted.Event.Seq); err != nil {
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
	// Model a commit that reached SQLite but whose successful acknowledgment was lost.
	if _, err := s.db.ExecContext(ctx, `UPDATE router_deliveries SET state='completed' WHERE event_seq=? AND plugin=?`, run.Event.Seq, run.Plugin); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReconcileRouterRetention(ctx, retentionEvent("retention.startup", now+1), 1, 1); err != nil {
		t.Fatal(err)
	}
	assertRetentionFenceCount(t, s, `SELECT COUNT(*) FROM router_events WHERE seq=?`, 1, run.Event.Seq)

	if err := s.FinishRouterDelivery(ctx, token, registry, run, router.Outcome{Kind: router.Continue}, now+2); err != nil {
		t.Fatalf("idempotent finish acknowledgment: %v", err)
	}
	if err := s.ReleaseRouterRuntime(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireRouterRuntime(ctx, registry, now+3); err != nil {
		t.Fatalf("fresh runtime startup: %v", err)
	}
	if _, _, err := s.ReconcileRouterRetention(ctx, retentionEvent("retention.startup", now+3), 1, 1); err != nil {
		t.Fatal(err)
	}
	assertRetentionFenceCount(t, s, `SELECT COUNT(*) FROM router_events WHERE seq=?`, 0, run.Event.Seq)
}

func TestRouterRetentionFenceTracksPluginAndAttempt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	registry := routerTestRegistry(t, "a", "b")
	emitted, err := s.EmitRouterEvent(ctx, registry, routerTestRequest(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	now := emitted.Event.EmittedAt
	if _, err := s.db.ExecContext(ctx, `UPDATE router_events SET emitted_at=? WHERE seq=?`, now-2*retentionTestDay, emitted.Event.Seq); err != nil {
		t.Fatal(err)
	}
	token, err := s.AcquireRouterRuntime(ctx, registry, now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimRouterDeliveries(ctx, token, registry, now, 2)
	if err != nil || len(first.Runs) != 2 {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	for _, run := range first.Runs {
		if err := s.FinishRouterDelivery(ctx, token, registry, run, router.Outcome{Kind: router.Fail, Error: "retry"}, now); err != nil {
			t.Fatalf("first attempt failure: %v", err)
		}
	}
	second, err := s.ClaimRouterDeliveries(ctx, token, registry, now+1000, 2)
	if err != nil || len(second.Runs) != 2 {
		t.Fatalf("second claim = %+v, %v", second, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE router_deliveries SET state='completed' WHERE event_seq=?`, emitted.Event.Seq); err != nil {
		t.Fatal(err)
	}
	for _, run := range first.Runs {
		if err := s.FinishRouterDelivery(ctx, token, registry, run, router.Outcome{Kind: router.Fail, Error: "stale"}, now+1001); err != nil {
			t.Fatalf("stale finish should be idempotent: %v", err)
		}
	}
	lease := s.routerLease()
	lease.mu.Lock()
	if len(lease.activeClaims) != 2 {
		lease.mu.Unlock()
		t.Fatalf("active claims after stale finish = %d, want 2", len(lease.activeClaims))
	}
	lease.mu.Unlock()
	if _, _, err := s.ReconcileRouterRetention(ctx, retentionEvent("retention.startup", now+1001), 1, 1); err != nil {
		t.Fatal(err)
	}
	assertRetentionFenceCount(t, s, `SELECT COUNT(*) FROM router_events WHERE seq=?`, 1, emitted.Event.Seq)
	for _, run := range second.Runs {
		if err := s.FinishRouterDelivery(ctx, token, registry, run, router.Outcome{Kind: router.Continue}, now+1002); err != nil {
			t.Fatal(err)
		}
	}
	lease.mu.Lock()
	remaining := len(lease.activeClaims)
	lease.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("active claims after matching finishes = %d, want 0", remaining)
	}
	if _, _, err := s.ReconcileRouterRetention(ctx, retentionEvent("retention.startup", now+1002), 1, 1); err != nil {
		t.Fatal(err)
	}
	assertRetentionFenceCount(t, s, `SELECT COUNT(*) FROM router_events WHERE seq=?`, 0, emitted.Event.Seq)
}

func TestRouterRetentionReleaseFailureClearsFenceAndRunningStateProtects(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	registry := routerTestRegistry(t, "worker")
	emitted, err := s.EmitRouterEvent(ctx, registry, routerTestRequest(`{}`))
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
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.ReleaseRouterRuntime(canceled, token); !errors.Is(err, context.Canceled) {
		t.Fatalf("failed release error = %v", err)
	}
	lease := s.routerLease()
	lease.mu.Lock()
	active, claims := lease.active, len(lease.activeClaims)
	lease.mu.Unlock()
	if active || claims != 0 {
		t.Fatalf("lease active=%v claims=%d after failed release", active, claims)
	}
	if _, _, err := s.ReconcileRouterRetention(ctx, retentionEvent("retention.startup", now+1), 1, 1); err != nil {
		t.Fatal(err)
	}
	assertRetentionFenceCount(t, s, `SELECT COUNT(*) FROM router_events WHERE seq=?`, 1, emitted.Event.Seq)
	assertRetentionFenceCount(t, s, `SELECT COUNT(*) FROM router_deliveries WHERE event_seq=? AND state='running'`, 1, emitted.Event.Seq)
}

func assertRetentionFenceCount(t *testing.T, s *Store, query string, want int64, args ...any) {
	t.Helper()
	var got int64
	if err := s.db.QueryRow(query, args...).Scan(&got); err != nil || got != want {
		t.Fatalf("%s = %d, err=%v; want %d", query, got, err, want)
	}
}

func TestRouterRetentionAndClaimLeaseLockOrderDoesNotHang(t *testing.T) {
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
	started := make(chan struct{})
	retentionDone := make(chan error, 1)
	claimDone := make(chan error, 1)
	go func() {
		close(started)
		_, _, err := s.ReconcileRouterRetention(ctx, retentionEvent("retention.startup", emitted.Event.EmittedAt+1), 1, 1)
		retentionDone <- err
	}()
	<-started
	go func() {
		_, err := s.ClaimRouterDeliveries(ctx, token, registry, emitted.Event.EmittedAt+2, 1)
		claimDone <- err
	}()
	lease.mu.Unlock()
	for name, done := range map[string]<-chan error{"retention": retentionDone, "claim": claimDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s hung while sharing runtime lease lock", name)
		}
	}
}
