package amuxruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
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
		ctx     context.Context
		id      string
		run     string
		project string
		started time.Time
		err     error
	}
	exits := make(chan runExit)
	active := make(map[string]context.CancelFunc)
	retries := make(map[string]fleetRetry)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
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
			if time.Now().Before(retries[chat.ID].next) {
				continue
			}
			id, err := store.ConversationWorkerRunID(ctx, client.Endpoint(), chat.ProjectIdent, chat.ID)
			if err != nil {
				continue
			}
			// Snapshots can lag behind the gateway (a failover, a resume), so
			// start from its current owner.
			lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			current, err := client.Conversation(lookupCtx, chat.ProjectIdent, chat.ID)
			cancel()
			if err != nil {
				fmt.Fprintf(stderr, "read fleet conversation %s: %v\n", chat.ID, err)
				continue
			}
			if current.Owner.SessionID != chat.Owner.SessionID {
				// Moved away; the next snapshot drops it.
				continue
			}
			fresh := id == ""
			if !fresh {
				// After a failover (to another slot here, or away and back) the
				// gateway owner instance no longer matches the saved run, which also
				// holds the dead thread: start a new run.
				run, err := store.WorkerRun(ctx, id)
				if err != nil {
					continue
				}
				fresh = run.Owner.SessionID != current.Owner.SessionID ||
					(current.Owner.InstanceID != run.Owner.InstanceID && current.Owner.InstanceID != run.PendingInstanceID)
			}
			if fresh {
				if id == "" {
					id = chat.ID
				} else {
					id = uuid.NewString()
				}
				if err := store.SaveWorkerRun(ctx, storage.WorkerRun{
					ID: id, ConversationID: chat.ID, Server: client.Endpoint(), Project: pool.Project,
					Directory: pool.Directory, Command: pool.Command, Owner: current.Owner,
				}); err != nil {
					fmt.Fprintf(stderr, "prepare fleet conversation %s: %v\n", chat.ID, err)
					continue
				}
			}
			runCtx, stop := context.WithCancel(ctx)
			started := time.Now()
			active[chat.ID] = stop
			jobs.Add(1)
			go func() {
				defer jobs.Done()
				err := worker.Run(runCtx, worker.Config{Store: store, Client: client, RunID: id, Open: open, Archive: archive, Observe: observe})
				select {
				case exits <- runExit{runCtx, chat.ID, id, chat.ProjectIdent, started, err}:
				case <-ctx.Done():
				}
			}()
		}
		for id, stop := range active {
			if !desired[id] {
				stop()
			}
		}
		for id := range retries {
			if !desired[id] {
				delete(retries, id)
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
		case <-ticker.C:
			reconcile()
		case exit := <-exits:
			// Check for a requested stop before cancelling the context ourselves.
			if exit.ctx.Err() != nil {
				exit.err = nil
			}
			active[exit.id]()
			delete(active, exit.id)
			retry := retries[exit.id]
			retry.finished(exit.started, time.Now(), exit.err)
			if exit.err == nil {
				delete(retries, exit.id)
			} else {
				retries[exit.id] = retry
			}
			if exit.err != nil && ctx.Err() == nil && missingRollout(exit.err) {
				// The thread cannot come back; hand the chat to another live agent.
				// Without one, keep the capped retry so the chat is never abandoned.
				failoverCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				next, err := client.FailoverFleetConversation(failoverCtx, exit.project, exit.id)
				cancel()
				if err == nil {
					fmt.Fprintf(stderr, "fleet conversation %s: provider thread missing; moved to %s agent on %s\n", exit.id, next.Provider, next.Hostname)
					delete(retries, exit.id)
					reconcile()
					continue
				}
				fmt.Fprintf(stderr, "fleet conversation %s: failover: %v\n", exit.id, err)
			}
			if exit.err != nil && ctx.Err() == nil {
				text := fmt.Sprintf("Fleet conversation stopped: %v. Retry in %s. Use amux fleet close --project %s --conversation %s to retire this chat; opening the agent again starts a fresh thread.", exit.err, retry.delay, exit.project, exit.id)
				fmt.Fprintf(stderr, "fleet conversation %s: %s\n", exit.id, text)
				publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				run, err := store.WorkerRun(publishCtx, exit.run)
				if err == nil && run.NativeSessionID != "" {
					if _, err = store.QueueWorkerEvent(publishCtx, run.ID, conversation.Event{Kind: "local_error", SessionID: run.NativeSessionID, Text: text}); err == nil {
						if err = worker.Flush(publishCtx, store, client, run.ID); err != nil {
							fmt.Fprintf(stderr, "publish fleet conversation failure %s: %v\n", exit.id, err)
						}
					}
				}
				cancel()
			}
			// A stop requested while the conversation was undesired may finish after
			// it became desired again; failed conversations stay gated by retry.next.
			reconcile()
		}
	}
}

func missingRollout(err error) bool {
	return strings.Contains(err.Error(), "no rollout found")
}

// Keep reconnect snapshots and configuration changes from restarting a failed
// provider every reconciliation. Retry timing belongs to the running client.
type fleetRetry struct {
	delay time.Duration
	next  time.Time
}

func (r *fleetRetry) finished(started, now time.Time, err error) {
	if err == nil || now.Sub(started) > r.delay {
		*r = fleetRetry{}
	}
	if err != nil {
		r.failed(now)
	}
}

func (r *fleetRetry) failed(now time.Time) {
	if r.delay == 0 {
		r.delay = time.Minute
	} else {
		r.delay = min(2*r.delay, 15*time.Minute)
	}
	r.next = now.Add(r.delay)
}
