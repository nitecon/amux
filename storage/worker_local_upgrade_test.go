package storage

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestUpgradeLoopbackWorkerOriginsPreservesActiveRun(t *testing.T) {
	ctx := context.Background()
	s, pool, _ := workerPoolFixture(t, 1)
	other := pool
	other.ID = uuid.NewString()
	createWorkerPool(t, s, other)
	pool.Server = "http://127.0.0.1:7913"
	createWorkerPool(t, s, pool)
	slot := onlyPoolSlot(t, s, pool.ID)
	run := WorkerRun{ID: uuid.NewString(), Server: pool.Server, Project: pool.Project, ConversationID: uuid.NewString(), Directory: pool.Directory, Command: pool.Command, Owner: slot.Owner}
	if err := s.AttachWorkerPoolRun(ctx, pool.ID, slot.ID, run); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueWorkerEvent(ctx, run.ID, structEventForPoolTest(run.Owner.InstanceID)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.UpgradeLoopbackWorkerOrigins(ctx); err != nil {
			t.Fatal(err)
		}
	}
	const newOrigin = "https://127.0.0.1:7913"
	saved, err := s.WorkerRun(ctx, run.ID)
	if err != nil || saved.Server != newOrigin || saved.Owner != run.Owner || saved.NativeSessionID != run.Owner.InstanceID {
		t.Fatalf("migrated run: %+v, %v", saved, err)
	}
	migrated := onlyPoolSlot(t, s, pool.ID)
	if migrated.Owner != slot.Owner || migrated.RunID != run.ID {
		t.Fatalf("migrated slot: %+v", migrated)
	}
	pools, err := s.WorkerPools(ctx, newOrigin)
	if err != nil || len(pools) != 1 || pools[0].ID != pool.ID {
		t.Fatalf("migrated pools: %+v, %v", pools, err)
	}
	outbox, err := s.WorkerOutbox(ctx, run.ID, 10)
	if err != nil || len(outbox) != 2 {
		t.Fatalf("migrated outbox: %+v, %v", outbox, err)
	}
	unrelated, err := s.WorkerPool(ctx, other.ID)
	if err != nil || unrelated.Server != other.Server {
		t.Fatalf("unrelated pool changed: %+v, %v", unrelated, err)
	}
}
