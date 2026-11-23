package storage

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
)

func workerPoolFixture(t *testing.T, slots int) (*Store, WorkerPool, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "worker-pool.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrateWorkerPool(ctx, tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	pool := WorkerPool{
		ID: uuid.NewString(), Server: "https://gateway.example", Project: "remote/project",
		Directory: t.TempDir(), Provider: "codex", OS: localWorkerOS(),
		Command: []string{"codex", "--full-auto"}, SlotCount: slots,
	}
	return s, pool, path
}

func createWorkerPool(t *testing.T, s *Store, pool WorkerPool) *WorkerPool {
	t.Helper()
	created, err := s.CreateWorkerPool(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func TestWorkerPoolPersistsConfigurationSlotsAndReportsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	s, pool, path := workerPoolFixture(t, 2)
	pool.Project = " Remote/Project "
	created := createWorkerPool(t, s, pool)
	if created.Project != "remote/project" {
		t.Fatalf("project was not normalized: %q", created.Project)
	}
	slots, err := s.WorkerPoolSlots(ctx, pool.ID)
	if err != nil || len(slots) != 2 {
		t.Fatalf("pool slots %+v: %v", slots, err)
	}
	first := slots[0]
	if first.Owner.Provider != pool.Provider || first.Owner.OS != pool.OS || first.Owner.SessionID == "" || first.Owner.InstanceID == "" || first.LastActivityAt == 0 {
		t.Fatalf("invalid initial slot %+v", first)
	}
	if _, err = s.NextWorkerPoolReport(ctx, pool.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.WorkerPool(ctx, created.ID)
	if err != nil || loaded.Project != "remote/project" || loaded.SlotCount != 2 || len(loaded.Command) != len(pool.Command) {
		t.Fatalf("reopened pool %+v: %v", loaded, err)
	}
	next, err := reopened.NextWorkerPoolReport(ctx, pool.ID, first.ID)
	if err != nil || next.ReportSequence != 2 {
		t.Fatalf("reopened report sequence %+v: %v", next, err)
	}
}

func TestWorkerPoolAttachRetryPreservesRunStateAndFinishKeepsReportHistory(t *testing.T) {
	ctx := context.Background()
	s, pool, _ := workerPoolFixture(t, 1)
	createWorkerPool(t, s, pool)
	slot := onlyPoolSlot(t, s, pool.ID)
	run := WorkerRun{
		ID: uuid.NewString(), Server: pool.Server, Project: pool.Project, ConversationID: uuid.NewString(),
		Directory: pool.Directory, Command: append([]string(nil), pool.Command...), Owner: slot.Owner,
	}
	if err := s.AttachWorkerPoolRun(ctx, pool.ID, slot.ID, run); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueWorkerEvent(ctx, run.ID, structEventForPoolTest(run.Owner.InstanceID)); err != nil {
		t.Fatal(err)
	}
	queued, err := s.WorkerOutbox(ctx, run.ID, 10)
	if err != nil || len(queued) != 2 {
		t.Fatalf("worker outbox %+v: %v", queued, err)
	}
	for _, entry := range queued {
		if err = s.AckWorkerOutbox(ctx, run.ID, entry.Sequence, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AttachWorkerPoolRun(ctx, pool.ID, slot.ID, run); err != nil {
		t.Fatalf("lost-ack retry: %v", err)
	}
	saved, err := s.WorkerRun(ctx, run.ID)
	if err != nil || saved.NativeSessionID != run.Owner.InstanceID || saved.OutboxCursor != queued[1].Sequence || saved.EventCursor != 1 {
		t.Fatalf("retry changed saved run %+v: %v", saved, err)
	}
	planned, err := s.PrepareWorkerInstance(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rotated := planned.PendingInstanceID
	if err = s.ActivateWorkerInstance(ctx, run.ID, rotated); err != nil {
		t.Fatal(err)
	}
	next, err := s.NextWorkerPoolReport(ctx, pool.ID, slot.ID)
	if err != nil || next.Owner.InstanceID != rotated || next.ReportSequence != 1 {
		t.Fatalf("report did not sync rotated owner %+v: %v", next, err)
	}
	finishedAt := time.UnixMilli(1_800_000_000_000)
	if err = s.FinishWorkerPoolRun(ctx, pool.ID, slot.ID, run.ID, finishedAt); err != nil {
		t.Fatal(err)
	}
	finished, err := s.NextWorkerPoolReport(ctx, pool.ID, slot.ID)
	if err != nil || finished.RunID != "" || finished.Owner.InstanceID != rotated || finished.ReportSequence != 2 || finished.LastActivityAt != finishedAt.UnixMilli() {
		t.Fatalf("finished slot %+v: %v", finished, err)
	}
}

func TestWorkerPoolRejectsInvalidAndConflictingAttachments(t *testing.T) {
	ctx := context.Background()
	s, pool, _ := workerPoolFixture(t, 2)
	createWorkerPool(t, s, pool)
	slots, err := s.WorkerPoolSlots(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	run := WorkerRun{ID: uuid.NewString(), Server: pool.Server, Project: pool.Project, ConversationID: uuid.NewString(), Directory: pool.Directory, Command: pool.Command, Owner: slots[0].Owner}
	preloaded := run
	preloaded.EventCursor = 1
	if err = s.AttachWorkerPoolRun(ctx, pool.ID, slots[0].ID, preloaded); !errors.Is(err, ErrWorkerPoolInput) {
		t.Fatalf("preloaded progress accepted: %v", err)
	}
	badConfig := run
	badConfig.Directory = t.TempDir()
	if err = s.AttachWorkerPoolRun(ctx, pool.ID, slots[0].ID, badConfig); !errors.Is(err, ErrWorkerPoolConflict) {
		t.Fatalf("invalid config accepted: %v", err)
	}
	badOwner := run
	badOwner.Owner.InstanceID = uuid.NewString()
	if err = s.AttachWorkerPoolRun(ctx, pool.ID, slots[0].ID, badOwner); !errors.Is(err, ErrWorkerPoolConflict) {
		t.Fatalf("wrong owner accepted: %v", err)
	}
	if err = s.AttachWorkerPoolRun(ctx, pool.ID, slots[0].ID, run); err != nil {
		t.Fatal(err)
	}
	changedSession := run
	changedSession.Owner.SessionID = uuid.NewString()
	if err = s.AttachWorkerPoolRun(ctx, pool.ID, slots[0].ID, changedSession); !errors.Is(err, ErrWorkerPoolConflict) {
		t.Fatalf("changed stable session accepted on retry: %v", err)
	}
	if err = s.AttachWorkerPoolRun(ctx, pool.ID, slots[1].ID, run); !errors.Is(err, ErrWorkerPoolConflict) {
		t.Fatalf("run attached to two slots: %v", err)
	}
	other := run
	other.ID = uuid.NewString()
	other.ConversationID = uuid.NewString()
	other.Owner = slots[1].Owner
	if err = s.AttachWorkerPoolRun(ctx, pool.ID, slots[0].ID, other); !errors.Is(err, ErrWorkerPoolConflict) {
		t.Fatalf("active run overwritten: %v", err)
	}
	if err = s.FinishWorkerPoolRun(ctx, pool.ID, slots[0].ID, other.ID, time.Now()); !errors.Is(err, ErrWorkerPoolConflict) {
		t.Fatalf("wrong run finished slot: %v", err)
	}
	invalid := pool
	invalid.ID = "not-a-uuid"
	if _, err = s.CreateWorkerPool(ctx, invalid); !errors.Is(err, ErrWorkerPoolInput) {
		t.Fatalf("invalid pool ID accepted: %v", err)
	}
	invalid = pool
	invalid.SlotCount = 33
	if _, err = s.CreateWorkerPool(ctx, invalid); !errors.Is(err, ErrWorkerPoolInput) {
		t.Fatalf("unbounded slots accepted: %v", err)
	}
	invalid = pool
	invalid.Server = "https://user:secret@gateway.example"
	if _, err = s.CreateWorkerPool(ctx, invalid); !errors.Is(err, ErrWorkerPoolInput) {
		t.Fatalf("credential-bearing server accepted: %v", err)
	}
}

func TestWorkerPoolReportSequenceIsMonotonicUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	s, pool, _ := workerPoolFixture(t, 1)
	createWorkerPool(t, s, pool)
	slot := onlyPoolSlot(t, s, pool.ID)
	const count = 24
	values := make(chan int64, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next, err := s.NextWorkerPoolReport(ctx, pool.ID, slot.ID)
			if err != nil {
				t.Errorf("next report: %v", err)
				return
			}
			values <- next.ReportSequence
		}()
	}
	wg.Wait()
	close(values)
	seen := map[int64]bool{}
	for value := range values {
		if seen[value] {
			t.Fatalf("duplicate report sequence %d", value)
		}
		seen[value] = true
	}
	if len(seen) != count || !seen[1] || !seen[count] {
		t.Fatalf("report sequences were not contiguous: %v", seen)
	}
}

func TestValidWorkerPoolServerPreservesStorageOriginRules(t *testing.T) {
	for _, raw := range []string{
		"http://gateway.example",
		"https://gateway.example:8443",
	} {
		if !validWorkerPoolServer(raw) {
			t.Errorf("validWorkerPoolServer(%q) rejected", raw)
		}
	}
	for _, raw := range []string{
		"https://gateway.example/",
		"https://user:secret@gateway.example",
		"https://gateway.example?",
		"https://gateway.example:0",
		"https://gateway.example:65536",
		"https://gateway.example/path",
	} {
		if validWorkerPoolServer(raw) {
			t.Errorf("validWorkerPoolServer(%q) accepted", raw)
		}
	}
}

func onlyPoolSlot(t *testing.T, s *Store, poolID string) WorkerPoolSlot {
	t.Helper()
	slots, err := s.WorkerPoolSlots(context.Background(), poolID)
	if err != nil || len(slots) != 1 {
		t.Fatalf("slots %+v: %v", slots, err)
	}
	return slots[0]
}

func structEventForPoolTest(sessionID string) conversation.Event {
	return conversation.Event{Kind: "session_ready", SessionID: sessionID}
}
