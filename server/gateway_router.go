package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
	"github.com/nitecon/amux/storage"
)

// The server always acquires the complete registry. Acquiring a partial one
// would discard retained obligations for plugins absent from that registry.
type GatewayRouter struct {
	store    *storage.Store
	runtime  *router.Runtime
	acquired <-chan struct{}
}

type readyRouterJournal struct {
	*storage.Store
	ready chan struct{}
}

func (j *readyRouterJournal) AcquireRouterRuntime(ctx context.Context, registry *router.Registry, now int64) (string, error) {
	token, err := j.Store.AcquireRouterRuntime(ctx, registry, now)
	if err == nil {
		close(j.ready)
	}
	return token, err
}

func NewGatewayRouter(store *storage.Store, retentionDays, botRetentionDays uint64) (*GatewayRouter, error) {
	defs := append(storage.RouterTaskEventDefinitions(), storage.RouterPresenceEventDefinitions()...)
	defs = append(defs, plugins.ClockDefinition(), plugins.RetentionStartupDefinition(), plugins.InboundDefinition(), plugins.ClientHeartbeatDefinition())
	defs = append(defs, plugins.ActivityEventDefinitions()...)
	defs = append(defs, plugins.NotificationEventDefinitions()...)
	forwarded := plugins.RemoteForwardedEvents()
	builtins := []router.Plugin{
		plugins.RemotePlugin(forwarded), plugins.InboundPlugin(store),
		plugins.PresencePlugin(store), plugins.AssignmentPlugin(store),
		plugins.NotifierPlugin(store), plugins.ReminderPlugin(store),
		plugins.ReviewPlugin(store), plugins.ReclaimPlugin(store),
		plugins.RetentionPlugin(store, retentionDays, botRetentionDays),
		plugins.NativeDeliveryPlugin(store),
		plugins.ProjectInboxPlugin(store),
	}
	ready := make(chan struct{})
	runtime, err := router.NewRuntime(&readyRouterJournal{Store: store, ready: ready}, store.EventBus(), defs, builtins, router.RunConfig{})
	if err != nil {
		return nil, err
	}
	return &GatewayRouter{store: store, runtime: runtime, acquired: ready}, nil
}

func (r *GatewayRouter) Registry() *router.Registry {
	return r.runtime.Registry()
}

// legacyReviewSource uses the same owned durable outbox loop as the task
// source, while preserving obligations written before the controller cutover.
type legacyReviewSource struct{ *storage.Store }

func (s legacyReviewSource) DrainRouterTaskOutbox(ctx context.Context, registry *router.Registry, limit int) (int, error) {
	return s.DrainLegacyReviewOutbox(ctx, registry, limit)
}

func (r *GatewayRouter) Run(parent context.Context, ready chan<- struct{}) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	type result struct {
		name string
		err  error
	}
	done := make(chan result, 7)
	start := func(name string, run func(context.Context) error) {
		go func() { done <- result{name, run(ctx)} }()
	}
	start("router", r.runtime.Run)
	select {
	case <-r.acquired:
	case result := <-done:
		if result.err == nil && parent.Err() != nil {
			return nil
		}
		if result.err == nil {
			return fmt.Errorf("router stopped before acquiring its journal")
		}
		return fmt.Errorf("start router: %w", result.err)
	case <-ctx.Done():
		cancel()
		return (<-done).err
	}
	registry, bus := r.runtime.Registry(), r.store.EventBus()
	start("tasks", func(ctx context.Context) error { return plugins.RunTaskSource(ctx, r.store, registry, bus) })
	start("legacy reviews", func(ctx context.Context) error {
		return plugins.RunTaskSource(ctx, legacyReviewSource{r.store}, registry, bus)
	})
	start("presence", func(ctx context.Context) error { return plugins.RunPresenceSource(ctx, r.store, registry, bus) })
	start("heartbeats", func(ctx context.Context) error { return plugins.RunHeartbeatSource(ctx, r.store, registry, bus) })
	start("clock", func(ctx context.Context) error {
		return plugins.RunClockSource(ctx, r.store, registry, bus, plugins.ClockConfig{})
	})
	start("ticks", func(ctx context.Context) error { RunEventTicks(ctx, bus, time.Second); return nil })
	_, startupErr := r.store.EmitRouterEvent(ctx, registry, router.EmitRequest{ID: plugins.RetentionStartupID, Version: 1, Source: "retention", Payload: []byte(`{}`)})
	var failures []error
	if startupErr != nil {
		if ctx.Err() == nil {
			failures = append(failures, startupErr)
		}
		cancel()
	} else {
		close(ready)
	}
	for range 7 {
		result := <-done
		if result.err != nil && !(ctx.Err() != nil && errors.Is(result.err, ctx.Err())) {
			failures = append(failures, fmt.Errorf("%s: %w", result.name, result.err))
		}
		if ctx.Err() == nil {
			if result.err == nil {
				failures = append(failures, fmt.Errorf("%s stopped unexpectedly", result.name))
			}
			cancel()
		}
	}
	return errors.Join(failures...)
}

// RunEventTicks owns the server's reconciliation clock. time.Ticker drops
// missed intervals instead of replaying a burst after a slow consumer.
func RunEventTicks(ctx context.Context, bus *eventbus.Bus, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := bus.Publish(eventbus.Event{Topic: eventbus.Tick}); err != nil {
				return
			}
		}
	}
}
