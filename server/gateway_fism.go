package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/storage"
	"github.com/rs/zerolog/log"
)

// GatewayFISM owns the gateway reconciliation sources.
type GatewayFISM struct {
	store   *storage.Store
	runtime *fism.Runtime
}

type gatewayFISMJournal struct {
	*storage.Store
}

func (j *gatewayFISMJournal) EmitRouterEvent(ctx context.Context, registry *fism.Registry, request fism.EmitRequest) (fism.EmitResult, error) {
	return j.Store.AcceptObservation(ctx, registry, request)
}

func NewGatewayFISM(store *storage.Store, retentionDays, botRetentionDays uint64) (*GatewayFISM, error) {
	defs := append(storage.RouterTaskEventDefinitions(), storage.RouterPresenceEventDefinitions()...)
	defs = append(defs, controllers.ClockDefinition(), controllers.RetentionStartupDefinition(), controllers.InboundDefinition(), controllers.ClientHeartbeatDefinition())
	defs = append(defs, controllers.ActivityEventDefinitions()...)
	defs = append(defs, controllers.NotificationEventDefinitions()...)
	forwarded := controllers.RemoteForwardedEvents()
	builtins := []fism.Controller{
		controllers.RemoteController(forwarded), controllers.InboundController(store),
		controllers.PresenceController(store), controllers.AssignmentController(store),
		controllers.NotifierController(store), controllers.ReminderController(store),
		controllers.ReviewController(store), controllers.ReclaimController(store),
		controllers.RetentionController(store, retentionDays, botRetentionDays),
		controllers.NativeDeliveryController(store),
		controllers.ProjectInboxController(store),
	}
	runtime, err := fism.NewRuntime(&gatewayFISMJournal{Store: store}, store.EventBus(), defs, builtins, fism.RunConfig{})
	if err != nil {
		return nil, err
	}
	return &GatewayFISM{store: store, runtime: runtime}, nil
}

func (r *GatewayFISM) Registry() *fism.Registry {
	return r.runtime.Registry()
}

// legacyReviewSource uses the same owned durable outbox loop as the task
// source, while preserving obligations written before the controller cutover.
type legacyReviewSource struct{ *GatewayFISM }

func (r *GatewayFISM) reconcileClock(ctx context.Context, at time.Time) error {
	event := fism.Event{
		UID: uuid.NewString(), ID: "clock.tick", Version: 1,
		Source: "clock", EmittedAt: at.UnixMilli(),
	}
	return r.runtime.ReconcileSignal(ctx, event)
}

func (r *GatewayFISM) DrainRouterHeartbeatOutbox(ctx context.Context, _ *fism.Registry, limit int) (int, error) {
	return r.reconcileObservations(ctx, limit, r.store.PendingObservations)
}

func (r *GatewayFISM) reconcileObservations(ctx context.Context, limit int, read func(context.Context, int) ([]storage.AcceptedObservation, error)) (int, error) {
	reports, err := read(ctx, limit)
	if err != nil {
		return 0, err
	}
	for i, report := range reports {
		if err := r.runtime.ReconcileSignal(ctx, report.Event); err != nil {
			return i, fmt.Errorf("reconcile observation %s: %w", report.Event.UID, err)
		}
		if err := r.settleObservation(ctx, report); err != nil {
			return i, fmt.Errorf("settle observation %s: %w", report.Event.UID, err)
		}
	}
	return len(reports), nil
}

func (r *GatewayFISM) reconcileNotifications(ctx context.Context, _ *fism.Registry, limit int) (int, error) {
	// Try every accepted notification once per sweep. A pending recipient
	// can stay in the source while later notifications settle.
	through, err := r.store.ObservationWatermark(ctx)
	if err != nil {
		return 0, err
	}
	var after int64
	for after < through {
		batch, err := r.store.PendingNotifications(ctx, after, limit)
		if err != nil {
			return 0, err
		}
		for _, notice := range batch {
			if notice.ID > through {
				return 0, nil
			}
			after = notice.ID
			if err := r.runtime.ReconcileSignal(ctx, notice.Event); err != nil {
				if ctx.Err() != nil {
					return 0, ctx.Err()
				}
				log.Debug().Ctx(ctx).Err(err).Str("event_uid", notice.Event.UID).Msg("notification remains pending")
				continue
			}
			if err := r.settleObservation(ctx, notice); err != nil {
				return 0, err
			}
		}
		if len(batch) < limit {
			return 0, nil
		}
	}
	return 0, nil
}

func (r *GatewayFISM) settleObservation(ctx context.Context, observation storage.AcceptedObservation) error {
	return retrySettlement(ctx, observation.Event.UID, func() error {
		return r.store.SettleObservation(ctx, observation.ID, observation.Event.UID)
	})
}

func (r *GatewayFISM) DrainRouterTaskOutbox(ctx context.Context, registry *fism.Registry, limit int) (int, error) {
	changes, err := r.store.PendingTaskChanges(ctx, limit)
	if err != nil {
		return 0, err
	}
	for i, change := range changes {
		event, err := r.store.PublishTaskChange(ctx, registry, change)
		if err != nil {
			return i, err
		}
		if err := r.reconcileAndSettle(ctx, event, func(outcomes []fism.Delivery) error {
			return r.store.SettleTaskChange(ctx, change.ID, event, outcomes)
		}); err != nil {
			return i, err
		}
	}
	return len(changes), nil
}

func (r *GatewayFISM) DrainRouterPresenceOutbox(ctx context.Context, registry *fism.Registry, limit int) (int, error) {
	changes, err := r.store.PublishPresenceChanges(ctx, registry, limit)
	if err != nil {
		return 0, err
	}
	for i, change := range changes {
		if err := r.reconcileAndSettle(ctx, change.Event, func(outcomes []fism.Delivery) error {
			return r.store.SettlePresenceChange(ctx, change, outcomes)
		}); err != nil {
			return i, err
		}
	}
	return len(changes), nil
}

func (r *GatewayFISM) reconcileAndSettle(ctx context.Context, event fism.Event, settle func([]fism.Delivery) error) error {
	outcomes, err := r.runtime.ReconcileEvent(ctx, event)
	if err != nil {
		return err
	}
	return retrySettlement(ctx, event.UID, func() error { return settle(outcomes) })
}

func retrySettlement(ctx context.Context, eventUID string, settle func() error) error {
	for failures := int64(0); ; failures++ {
		err := settle()
		if err == nil {
			return nil
		}
		log.Warn().Ctx(ctx).Err(err).Str("event_uid", eventUID).Msg("retry reconciliation settlement")
		timer := time.NewTimer(fism.RetryDelay(failures + 1))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s legacyReviewSource) DrainRouterTaskOutbox(ctx context.Context, registry *fism.Registry, limit int) (int, error) {
	changes, count, err := s.store.PublishLegacyReviewChanges(ctx, registry, limit)
	if err != nil {
		return 0, err
	}
	for i, change := range changes {
		if err := s.reconcileAndSettle(ctx, change.Event, func(outcomes []fism.Delivery) error {
			return s.store.SettleLegacyReviewChange(ctx, change, outcomes)
		}); err != nil {
			return i, err
		}
	}
	return count, nil

}

func (r *GatewayFISM) Run(parent context.Context, ready chan<- struct{}) error {
	release, err := r.store.AcquireReconciliation()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	type result struct {
		name string
		err  error
	}
	done := make(chan result, 9)
	start := func(name string, run func(context.Context) error) {
		go func() { done <- result{name, run(ctx)} }()
	}
	if err := r.store.RecoverLegacyInputs(ctx, r.runtime.Registry()); err != nil {
		return err
	}
	registry, bus := r.runtime.Registry(), r.store.EventBus()
	start("tasks", func(ctx context.Context) error { return controllers.RunTaskSource(ctx, r, registry, bus) })
	start("legacy reviews", func(ctx context.Context) error {
		return controllers.RunTaskSource(ctx, legacyReviewSource{r}, registry, bus)
	})
	start("presence", func(ctx context.Context) error { return controllers.RunPresenceSource(ctx, r, registry, bus) })
	start("heartbeats", func(ctx context.Context) error { return controllers.RunHeartbeatSource(ctx, r, registry, bus) })
	start("inbound", func(ctx context.Context) error {
		return controllers.RunInboundReconciliation(ctx, registry, bus, func(ctx context.Context, _ *fism.Registry, limit int) (int, error) {
			return r.reconcileObservations(ctx, limit, r.store.PendingInboundMessages)
		})
	})
	start("notifications", func(ctx context.Context) error {
		return controllers.RunNotificationSource(ctx, registry, bus, r.reconcileNotifications)
	})
	start("clock", func(ctx context.Context) error {
		return controllers.RunClockSource(ctx, r.reconcileClock, bus, controllers.ClockConfig{})
	})
	start("ticks", func(ctx context.Context) error { RunEventTicks(ctx, bus, time.Second); return nil })
	start("startup retention", func(ctx context.Context) error {
		_, err := r.runtime.ReconcileEvent(ctx, fism.Event{
			UID: uuid.NewString(), ID: controllers.RetentionStartupID, Version: 1,
			Source: "retention", EmittedAt: time.Now().UTC().UnixMilli(), Payload: []byte(`{}`),
		})
		if err != nil {
			return err
		}
		<-ctx.Done()
		return nil
	})
	close(ready)
	var failures []error
	for range 9 {
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
