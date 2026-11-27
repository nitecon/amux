package worker

import (
	"context"
	"errors"
	"time"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
)

// WatchAssignments owns the desktop's single gateway subscription. It
// acknowledges only after the local worker inbox and cursor commit together.
func WatchAssignments(ctx context.Context, store *storage.Store, client *gatewayclient.Client) {
	server := client.Endpoint()
	for ctx.Err() == nil {
		consumer, committed, err := store.WorkerTaskConsumer(ctx, server)
		if err == nil {
			var bootstrap func(int64) error
			if committed < 0 {
				bootstrap = func(cursor int64) error {
					return bootstrapAssignments(ctx, store, client, cursor)
				}
			}
			_ = client.WatchTaskEvents(ctx, consumer, nil, func(event storage.TaskEvent) (string, error) {
				queued, err := store.RecordWorkerTaskEvent(ctx, server, event)
				if err != nil {
					return "", err
				}
				if queued {
					return "queued", nil
				}
				return "received", nil
			}, bootstrap)
		}
		if pause(ctx, time.Second) != nil {
			return
		}
	}
}

func bootstrapAssignments(ctx context.Context, store *storage.Store, client *gatewayclient.Client, cursor int64) error {
	server := client.Endpoint()
	pools, err := store.WorkerPools(ctx, server)
	if err != nil {
		return err
	}
	owners := make(map[string]map[string]storage.AgentOrigin)
	for _, pool := range pools {
		slots, err := store.WorkerPoolSlots(ctx, pool.ID)
		if err != nil {
			return err
		}
		if owners[pool.Project] == nil {
			owners[pool.Project] = make(map[string]storage.AgentOrigin)
		}
		for _, slot := range slots {
			owners[pool.Project][slot.Owner.SessionID] = slot.Owner
		}
	}
	var live []storage.Task
	for project, bySession := range owners {
		tasks, err := client.Tasks(ctx, project, []string{"todo", "in_progress", "stalled"})
		if err != nil {
			var response *gatewayclient.HTTPError
			if errors.As(err, &response) && (response.Status == 404 || response.Status == 410) {
				continue
			}
			return err
		}
		for _, summary := range tasks {
			if summary.OwnerAgentID == nil {
				continue
			}
			owner, ok := bySession[*summary.OwnerAgentID]
			if !ok {
				continue
			}
			task, err := client.OwnedActiveTask(ctx, project, summary.ID, owner)
			if err != nil {
				return err
			}
			if task != nil {
				live = append(live, *task)
			}
		}
	}
	return store.BootstrapWorkerTasks(ctx, server, cursor, live)
}
