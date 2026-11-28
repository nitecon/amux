package fism

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/queues"
)

type controllerProgress struct {
	delivery Delivery
	running  bool
	settled  bool
	retryAt  time.Time
}

type controllerResult struct {
	id      string
	outcome Outcome
}

// ReconcileEvent runs the active controllers for a freshly emitted event. The
// complete result belongs to the caller's state commit. Controller admission,
// dependency waits and retries stay in memory for the lifetime of the pass.
func (r *Runtime) ReconcileEvent(ctx context.Context, event Event) ([]Delivery, error) {
	return r.reconcileEvent(ctx, event, true)
}

// ReconcileSignal handles a recurring wakeup. The next wakeup observes fresh
// state and tries again if a controller failed on this pass.
func (r *Runtime) ReconcileSignal(ctx context.Context, event Event) error {
	deliveries, err := r.reconcileEvent(ctx, event, false)
	if err != nil {
		return err
	}
	var failures []error
	for _, delivery := range deliveries {
		if delivery.State == "failed" && delivery.LastError != nil {
			failures = append(failures, fmt.Errorf("%s: %s", delivery.Plugin, *delivery.LastError))
		}
	}
	return errors.Join(failures...)
}

func (r *Runtime) reconcileEvent(ctx context.Context, event Event, retryFailures bool) ([]Delivery, error) {
	if _, declared := r.registry.events[eventKey{id: event.ID, version: event.Version}]; !declared {
		return nil, fmt.Errorf("%w: event %q v%d is not declared", ErrInvalidEvent, event.ID, event.Version)
	}
	ids := r.registry.Subscribers(event.ID, event.Version)
	if len(ids) == 0 {
		return nil, nil
	}

	pass, cancel := context.WithCancel(ctx)
	inboxes := make(map[string]queues.Queue, len(ids))
	defer func() {
		cancel()
		for _, inbox := range inboxes {
			<-inbox.Done()
		}
	}()
	results := make(chan controllerResult, r.capacity)
	var scheduled sync.Map
	progress := make(map[string]*controllerProgress, len(ids))
	for _, id := range ids {
		progress[id] = &controllerProgress{delivery: Delivery{EventSeq: event.Seq, Plugin: id}}
		inbox, err := r.queueManager.Open(pass, "router."+id, queues.Options{Capacity: 1}, func(ticket, _ string) error {
			if _, ok := scheduled.LoadAndDelete([2]string{id, ticket}); !ok {
				return nil
			}
			outcome := r.runSteps(pass, ClaimedRun{Event: event, Plugin: id})
			select {
			case results <- controllerResult{id: id, outcome: outcome}:
			case <-pass.Done():
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("open controller queue %q: %w", id, err)
		}
		inboxes[id] = inbox
	}

	remaining, running := len(ids), 0
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		now := r.now()
		var nextRetry time.Time
		skipped := false
		for _, id := range ids {
			current := progress[id]
			if current.running || current.settled {
				continue
			}
			waiting, blocked := r.dependencyState(event.ID, id, progress)
			if blocked {
				reason := "dependency did not complete"
				current.delivery.State = "skipped"
				current.delivery.LastError = &reason
				current.delivery.UpdatedAt = now.UnixMilli()
				current.settled = true
				remaining--
				skipped = true
				continue
			}
			if waiting || running == r.capacity {
				continue
			}
			if now.Before(current.retryAt) {
				if nextRetry.IsZero() || current.retryAt.Before(nextRetry) {
					nextRetry = current.retryAt
				}
				continue
			}
			ticket := uuid.NewString()
			key := [2]string{id, ticket}
			scheduled.Store(key, struct{}{})
			if err := inboxes[id].Enqueue(pass, ticket, event.UID, 0); err != nil {
				scheduled.Delete(key)
				return nil, fmt.Errorf("dispatch controller %q: %w", id, err)
			}
			current.running = true
			running++
		}
		if remaining == 0 {
			break
		}
		if skipped {
			continue
		}

		var retry <-chan time.Time
		if !nextRetry.IsZero() {
			timer.Reset(max(0, nextRetry.Sub(r.now())))
			retry = timer.C
		} else if running == 0 {
			return nil, fmt.Errorf("controller dependencies stalled for event %q", event.ID)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-retry:
		case result := <-results:
			current := progress[result.id]
			if !current.running {
				continue
			}
			current.running = false
			running--
			now := r.now()
			decision, err := r.registry.DecideControllerOutcome(ClaimedRun{
				Event: event, Plugin: result.id, Failures: current.delivery.Attempts,
			}, result.outcome)
			if err != nil {
				return nil, err
			}
			current.delivery.Attempts = decision.Failures
			if decision.RetryAfter > 0 && retryFailures {
				current.retryAt = now.Add(decision.RetryAfter)
				continue
			}
			current.delivery.State = decision.State
			current.delivery.UpdatedAt = now.UnixMilli()
			if result.outcome.Kind == Fail {
				current.delivery.State = "failed"
				current.delivery.LastError = &result.outcome.Error
			}
			current.settled = true
			remaining--
		}
	}

	deliveries := make([]Delivery, 0, len(ids))
	for _, id := range ids {
		deliveries = append(deliveries, progress[id].delivery)
	}
	return deliveries, nil
}

func (r *Runtime) dependencyState(event, id string, progress map[string]*controllerProgress) (waiting, blocked bool) {
	for _, dependency := range r.registry.Dependencies(id, event) {
		current := progress[dependency]
		if current == nil {
			return false, true
		}
		if !current.settled {
			waiting = true
		} else if current.delivery.State != "completed" {
			return false, true
		}
	}
	return waiting, false
}
