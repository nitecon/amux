// Package plugins contains built-in event sources that bridge durable state
// and server ticks into the router.
package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

const (
	sourceDrainBatchSize     = 200
	sourceSubscriptionBuffer = 64
)

// TaskDrainer drains persisted router task notifications. Implementations
// retain rows when they return an error.
type TaskDrainer interface {
	DrainRouterTaskOutbox(context.Context, *router.Registry, int) (int, error)
}

// EventEmitter persists an event using the supplied router registry.
type EventEmitter interface {
	EmitRouterEvent(context.Context, *router.Registry, router.EmitRequest) (router.EmitResult, error)
}

// RunTaskSource reconciles the durable task outbox at startup and on task or
// clock notifications. It owns its event subscription and retry timer.
func RunTaskSource(ctx context.Context, drainer TaskDrainer, registry *router.Registry, bus *eventbus.Bus) error {
	if ctx == nil || drainer == nil || registry == nil || bus == nil {
		return errors.New("task source requires context, drainer, registry, and event bus")
	}
	return runReconciliationSource(ctx, registry, bus, taskSourceName, eventbus.TaskChanged, drainer.DrainRouterTaskOutbox)
}

const taskSourceName = "task"

// runReconciliationSource owns a durable outbox subscription and retry timer.
func runReconciliationSource(ctx context.Context, registry *router.Registry, bus *eventbus.Bus, sourceName string, changedTopic eventbus.Topic, drain func(context.Context, *router.Registry, int) (int, error)) error {
	if ctx == nil || drain == nil || registry == nil || bus == nil {
		return errors.New("reconciliation source requires context, drain function, registry, and event bus")
	}
	sub := bus.Subscribe(sourceSubscriptionBuffer)
	defer func() { sub.Unsubscribe() }()

	failures := int64(0)
	var retryDeadline time.Time
	needsDrain := true
	for {
		if ctx.Err() != nil {
			return nil
		}
		if needsDrain {
			needsDrain = false
			for {
				if ctx.Err() != nil {
					return nil
				}
				n, err := drain(ctx, registry, sourceDrainBatchSize)
				if ctx.Err() != nil {
					return nil
				}
				if err != nil {
					failures++
					slog.WarnContext(ctx, "router outbox drain failed", "source", sourceName, "error", err)
					retryDeadline = time.Now().Add(router.RetryDelay(failures))
					break
				}
				failures = 0
				retryDeadline = time.Time{}
				if n < sourceDrainBatchSize {
					break
				}
				// A full batch may leave more durable work. Yield to cancellation
				// between batches while keeping startup/reconciliation immediate.
				select {
				case <-ctx.Done():
					return nil
				default:
				}
			}
		}
		if !retryDeadline.IsZero() && !time.Now().Before(retryDeadline) {
			retryDeadline = time.Time{}
			needsDrain = true
			continue
		}

		var retry <-chan time.Time
		var timer *time.Timer
		if !retryDeadline.IsZero() {
			delay := time.Until(retryDeadline)
			if delay < 0 {
				delay = 0
			}
			timer = time.NewTimer(delay)
			retry = timer.C
		}
		select {
		case <-ctx.Done():
			stopSourceTimer(timer)
			return nil
		case <-retry:
			retryDeadline = time.Time{}
			needsDrain = true
		case event, ok := <-sub.C:
			stopSourceTimer(timer)
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				switch sub.Err() {
				case eventbus.ErrSlowConsumer:
					sub.Unsubscribe()
					sub = bus.Subscribe(sourceSubscriptionBuffer)
					needsDrain = true // Reconcile durable state after losing hints.
				case eventbus.ErrBusClosed:
					return eventbus.ErrBusClosed
				default:
					return errors.New(sourceName + " source event subscription closed")
				}
			} else if event.Topic == changedTopic || event.Topic == eventbus.Tick {
				retryDeadline = time.Time{}
				needsDrain = true
			}
		}
	}
}

// ClockDefinition declares the local clock event accepted by the router.
func ClockDefinition() router.EventDefinition {
	return router.EventDefinition{ID: "clock.tick", Version: 1, RemoteEmit: false, Validate: router.Object}
}

// ClockConfig injects the clock used to timestamp emitted events.
type ClockConfig struct {
	Now func() time.Time
}

// RunClockSource emits one clock event per observed Tick notification. It
// owns only its subscription; the server owns the shared tick schedule.
func RunClockSource(ctx context.Context, emitter EventEmitter, registry *router.Registry, bus *eventbus.Bus, config ClockConfig) error {
	if ctx == nil || emitter == nil || registry == nil || bus == nil {
		return errors.New("clock source requires context, emitter, registry, and event bus")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	sub := bus.Subscribe(sourceSubscriptionBuffer)
	return runClockSource(ctx, emitter, registry, bus, sub, now)
}

// runClockSource consumes a subscription already owned by its caller. Keeping
// this loop separate lets tests establish subscription readiness synchronously.
func runClockSource(ctx context.Context, emitter EventEmitter, registry *router.Registry, bus *eventbus.Bus, sub *eventbus.Subscription, now func() time.Time) error {
	defer func() { sub.Unsubscribe() }()
	for {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-sub.C:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				switch sub.Err() {
				case eventbus.ErrSlowConsumer:
					sub.Unsubscribe()
					sub = bus.Subscribe(sourceSubscriptionBuffer)
					continue // Wait for a subsequent Tick; do not synthesize one.
				case eventbus.ErrBusClosed:
					return eventbus.ErrBusClosed
				default:
					return errors.New("clock source event subscription closed")
				}
			}
			if event.Topic != eventbus.Tick {
				continue
			}

			// Collapse ticks already queued while the source was busy. Other
			// notifications are consumed but never converted into clock events.
			for drained, draining := 0, true; draining && drained < sourceSubscriptionBuffer; drained++ {
				select {
				case <-ctx.Done():
					return nil
				case _, ok := <-sub.C:
					if !ok {
						draining = false
						if ctx.Err() != nil {
							return nil
						}
						if sub.Err() == eventbus.ErrSlowConsumer {
							sub.Unsubscribe()
							sub = bus.Subscribe(sourceSubscriptionBuffer)
						} else if sub.Err() == eventbus.ErrBusClosed {
							return eventbus.ErrBusClosed
						} else {
							return errors.New("clock source event subscription closed")
						}
					}
				default:
					draining = false
				}
			}
			if ctx.Err() != nil {
				return nil
			}
			payload, err := json.Marshal(struct {
				At int64 `json:"at"`
			}{At: now().UnixMilli()})
			if err != nil { // The fixed payload cannot normally fail to encode.
				slog.WarnContext(ctx, "router clock event encoding failed", "source", "clock", "error", err)
				continue
			}
			_, err = emitter.EmitRouterEvent(ctx, registry, router.EmitRequest{
				ID: "clock.tick", Version: 1, Payload: payload, Source: "clock",
			})
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				slog.WarnContext(ctx, "router clock event emit failed", "source", "clock", "error", err)
			}
		}
	}
}

func stopSourceTimer(timer *time.Timer) {
	if timer != nil && !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
