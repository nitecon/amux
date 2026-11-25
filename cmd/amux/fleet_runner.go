package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

func runFleetConversations(ctx context.Context, store *storage.Store, client *gatewayclient.Client, open worker.Open, archive worker.EventArchive, observe worker.EventObserver,
	checkout func(string, string) string, changed <-chan struct{}, stderr io.Writer,
) {
	ctx, cancel := context.WithCancel(ctx)
	var jobs sync.WaitGroup
	defer func() {
		cancel()
		jobs.Wait()
	}()
	snapshots := make(chan []storage.FleetConversation)
	jobs.Add(1)
	go func() {
		defer jobs.Done()
		for ctx.Err() == nil {
			err := client.WatchFleetConversations(ctx, func(items []storage.FleetConversation) error {
				select {
				case snapshots <- items:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			var response *gatewayclient.HTTPError
			if errors.As(err, &response) && (response.Status == 404 || response.Status == 403) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()

	type runExit struct {
		id  string
		err error
	}
	exits := make(chan runExit)
	active := make(map[string]context.CancelFunc)
	var available []storage.FleetConversation
	reconcile := func() {
		pools, err := store.WorkerPools(ctx, client.Endpoint())
		if err != nil {
			return
		}
		type workerKey struct{ project, session string }
		local := make(map[workerKey]storage.WorkerPool)
		for _, pool := range pools {
			if directory := checkout(pool.Project, pool.Provider); directory == "" || directory != pool.Directory {
				continue
			}
			slots, err := store.WorkerPoolSlots(ctx, pool.ID)
			if err != nil {
				continue
			}
			for _, slot := range slots {
				local[workerKey{pool.Project, slot.Owner.SessionID}] = pool
			}
		}
		desired := make(map[string]bool)
		for _, chat := range available {
			pool, ok := local[workerKey{chat.ProjectIdent, chat.Owner.SessionID}]
			if !ok || pool.Provider != chat.Provider {
				continue
			}
			desired[chat.ID] = true
			if active[chat.ID] != nil {
				continue
			}
			id, err := store.ConversationWorkerRunID(ctx, client.Endpoint(), chat.ProjectIdent, chat.ID)
			if err != nil {
				continue
			}
			if id == "" {
				id = chat.ID
				if err := store.SaveWorkerRun(ctx, storage.WorkerRun{
					ID: id, ConversationID: chat.ID, Server: client.Endpoint(), Project: pool.Project,
					Directory: pool.Directory, Command: pool.Command, Owner: chat.Owner,
				}); err != nil {
					fmt.Fprintf(stderr, "prepare fleet conversation %s: %v\n", chat.ID, err)
					continue
				}
			}
			runCtx, stop := context.WithCancel(ctx)
			active[chat.ID] = stop
			jobs.Add(1)
			go func() {
				defer jobs.Done()
				err := worker.Run(runCtx, worker.Config{Store: store, Client: client, RunID: id, Open: open, Archive: archive, Observe: observe})
				select {
				case exits <- runExit{chat.ID, err}:
				case <-ctx.Done():
				}
			}()
		}
		for id, stop := range active {
			if !desired[id] {
				stop()
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case available = <-snapshots:
			reconcile()
		case <-changed:
			reconcile()
		case exit := <-exits:
			active[exit.id]()
			delete(active, exit.id)
			if exit.err != nil && ctx.Err() == nil {
				fmt.Fprintf(stderr, "fleet conversation %s stopped: %v\n", exit.id, exit.err)
			}
		}
	}
}
