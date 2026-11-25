package storage

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestWorkerTaskInboxReplaysLegacyParentSnapshot(t *testing.T) {
	ctx := context.Background()
	store, pool, _ := workerPoolFixture(t, 1)
	createWorkerPool(t, store, pool)
	slot := onlyPoolSlot(t, store, pool.ID)
	if _, _, err := store.WorkerTaskConsumer(ctx, pool.Server); err != nil {
		t.Fatal(err)
	}
	if err := store.BootstrapWorkerTasks(ctx, pool.Server, 10, nil); err != nil {
		t.Fatal(err)
	}
	for i, project := range []string{"unrelated", pool.Project} {
		id := uuid.NewString()
		snapshot, err := json.Marshal(map[string]any{
			"id": id, "project_ident": project, "status": "todo",
			"owner_origin": slot.Owner, "parent": `{"id":"old-wire-format"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		eventID := int64(11 + i)
		queued, err := store.RecordWorkerTaskEvent(ctx, pool.Server, TaskEvent{
			ID: eventID, ProjectIdent: project, Task: snapshot,
		})
		if err != nil || queued != (project == pool.Project) {
			t.Fatalf("event %d queued=%t: %v", eventID, queued, err)
		}
		_, cursor, err := store.WorkerTaskConsumer(ctx, pool.Server)
		if err != nil || cursor != eventID {
			t.Fatalf("cursor=%d, want %d: %v", cursor, eventID, err)
		}
	}
}
