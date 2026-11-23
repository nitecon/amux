package storage

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/router"
)

func assignmentClock(at int64) router.Event {
	return router.Event{UID: uuid.NewString(), ID: "clock.tick", Version: 1, EmittedAt: at}
}

func TestReconcileRouterAssignmentsDoesNotReconcilePresence(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	now := time.UnixMilli(1_800_500_000_000)
	task := makeTask(t, s, nil)
	agent := idleAssignmentAgent("tasks", assignmentOrigin("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "codex", "linux"), now)
	// A recent activity is still busy in stored state. The clock event must not
	// derive idle state as ReconcileRouterPresence would.
	activity := now.UnixMilli()
	agent.LastActivityAt = &activity
	reportAssignmentPresence(t, s, now, "router-workers", 1, agent)
	var before int64
	if err := s.db.QueryRowContext(ctx, "SELECT observed_at FROM amux_presence_clock WHERE id=1").Scan(&before); err != nil {
		t.Fatal(err)
	}
	event := assignmentClock(now.Add(time.Minute).UnixMilli())
	if err := s.ReconcileRouterAssignments(ctx, event); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err := s.db.QueryRowContext(ctx, "SELECT observed_at FROM amux_presence_clock WHERE id=1").Scan(&after); err != nil || after != before {
		t.Fatalf("assignment advanced presence clock: before=%d after=%d err=%v", before, after, err)
	}
	got, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || got.Task.Status != "todo" {
		t.Fatalf("assignment derived idle from clock event: task=%+v err=%v", got, err)
	}
	items, err := s.Presence(ctx, "tasks")
	if err != nil || len(items) != 1 || items[0].State != "busy" {
		t.Fatalf("presence state changed: items=%+v err=%v", items, err)
	}
}

func TestReconcileRouterAssignmentsDisabledAndGuards(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	now := time.UnixMilli(1_800_510_000_000)
	task := makeTask(t, s, nil)
	idle := idleAssignmentAgent("tasks", assignmentOrigin("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "codex", "linux"), now)
	busy := idleAssignmentAgent("tasks", assignmentOrigin("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "codex", "linux"), now)
	busy.Busy = true
	staleAt := now.Add(-time.Duration(presenceGoneAfter) * time.Millisecond)
	stale := idleAssignmentAgent("tasks", assignmentOrigin("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", "ffffffff-ffff-4fff-8fff-ffffffffffff", "codex", "linux"), staleAt)
	reportAssignmentPresence(t, s, now, "router-workers", 1, idle, busy)
	reportAssignmentPresence(t, s, staleAt, "stale-worker", 1, stale)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('plugin.assignment.enabled','false')"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterAssignments(ctx, assignmentClock(now.UnixMilli())); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || got.Task.Status != "todo" {
		t.Fatalf("disabled assignment claimed task: task=%+v err=%v", got, err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM settings WHERE key='plugin.assignment.enabled'"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterAssignments(ctx, assignmentClock(now.UnixMilli())); err != nil {
		t.Fatal(err)
	}
	got, err = s.Task(ctx, "tasks", task.ID)
	if err != nil || got.Task.Status != "in_progress" || got.Task.OwnerOrigin == nil || got.Task.OwnerOrigin.SessionID != idle.Origin.SessionID {
		t.Fatalf("busy or expired session won allocation: task=%+v err=%v", got, err)
	}
}

func TestPluginEnabledWhitelistAndDefaults(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "router-assignment-plugins.db"))
	for _, id := range []string{"assignment", "notifier", "reminder", "review", "reclaim"} {
		enabled, err := s.PluginEnabled(ctx, id)
		if err != nil || !enabled {
			t.Fatalf("plugin %q default enabled=%v err=%v", id, enabled, err)
		}
	}
	if _, err := s.PluginEnabled(ctx, "unknown"); err == nil {
		t.Fatal("unknown plugin ID accepted")
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('plugin.review.enabled','0')"); err != nil {
		t.Fatal(err)
	}
	if enabled, err := s.PluginEnabled(ctx, "review"); err != nil || !enabled {
		t.Fatalf("only literal false should disable: enabled=%v err=%v", enabled, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE settings SET value='false' WHERE key='plugin.review.enabled'"); err != nil {
		t.Fatal(err)
	}
	if enabled, err := s.PluginEnabled(ctx, "review"); err != nil || enabled {
		t.Fatalf("literal false did not disable: enabled=%v err=%v", enabled, err)
	}
}

func TestLivePresenceSnapshotUsesMaxClockAndNow(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "router-assignment-snapshot.db"))
	now := time.UnixMilli(1_800_520_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	agent.LastActivityAt = int64Pointer(now.UnixMilli())
	reportAssignmentPresence(t, s, now, "snapshot-worker", 1, agent)
	if _, err := s.db.ExecContext(ctx, "UPDATE amux_presence_clock SET observed_at=? WHERE id=1", now.Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	items, observed, err := livePresenceSnapshot(ctx, s.db, now.UnixMilli())
	if err != nil || observed != now.Add(time.Minute).UnixMilli() || len(items) != 1 {
		t.Fatalf("snapshot did not use stored clock: observed=%d items=%+v err=%v", observed, items, err)
	}
	items, observed, err = livePresenceSnapshot(ctx, s.db, now.Add(2*time.Minute).UnixMilli())
	if err != nil || observed != now.Add(2*time.Minute).UnixMilli() || len(items) != 0 {
		t.Fatalf("snapshot did not use newer supplied time for TTL: observed=%d items=%+v err=%v", observed, items, err)
	}
	items, observed, err = livePresenceSnapshot(ctx, s.db, now.Add(-time.Minute).UnixMilli())
	if err != nil || observed != now.Add(time.Minute).UnixMilli() || len(items) != 1 {
		t.Fatalf("out-of-order snapshot regressed clock or resurrected state: observed=%d items=%+v err=%v", observed, items, err)
	}
	_, observed, err = livePresenceSnapshot(ctx, s.db, now.Add(3*time.Minute).UnixMilli())
	if err != nil || observed != now.Add(3*time.Minute).UnixMilli() {
		t.Fatalf("empty snapshot observed=%d err=%v", observed, err)
	}
}

func TestReconcileRouterAssignmentsReplayAndConcurrentClock(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	now := time.UnixMilli(1_800_530_000_000)
	task := makeTask(t, s, nil)
	agent := idleAssignmentAgent("tasks", assignmentOrigin("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "codex", "linux"), now)
	reportAssignmentPresence(t, s, now, "router-workers", 1, agent)
	event := assignmentClock(now.UnixMilli())
	var group sync.WaitGroup
	errCh := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() { defer group.Done(); errCh <- s.ReconcileRouterAssignments(ctx, event) }()
	}
	group.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := s.ReconcileRouterAssignments(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || got.Task.Status != "in_progress" || got.Task.OwnerOrigin == nil || got.Task.OwnerOrigin.SessionID != agent.Origin.SessionID {
		t.Fatalf("idempotent event produced invalid claim: task=%+v err=%v", got, err)
	}
	var taskStatus string
	if err := s.db.QueryRowContext(ctx, "SELECT status FROM tasks WHERE id=?", task.ID).Scan(&taskStatus); err != nil || taskStatus != "in_progress" {
		t.Fatalf("replayed clock changed the claimed task: status=%q err=%v", taskStatus, err)
	}
}

func int64Pointer(value int64) *int64 { return &value }
