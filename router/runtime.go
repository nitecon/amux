package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nitecon/amux/eventbus"
)

const (
	defaultRuntimeCapacity = 64
	maxRuntimeCapacity     = 64
	runtimeBusBuffer       = 64
	runtimeCleanupTimeout  = 5 * time.Second
)

var ErrRuntimeRunning = errors.New("router runtime is already running")

// Runtime owns plugin executions and their durable delivery completion.
type Runtime struct {
	journal  Journal
	bus      *eventbus.Bus
	registry *Registry
	plugins  map[string]Plugin
	capacity int
	now      func() time.Time

	runMu   sync.Mutex
	running bool
}

// StepContext carries the current plugin and event identity for emitted events.
type StepContext struct {
	journal  Journal
	registry *Registry
	plugin   string
	cause    string
}

// Registry returns the immutable event declarations for an atomic domain
// operation that emits events in the same transaction as its state changes.
func (c *StepContext) Registry() *Registry { return c.registry }

// Emit persists an event as this plugin, correlated to the event it is handling.
func (c *StepContext) Emit(ctx context.Context, id string, version uint32, payload json.RawMessage) (EmitResult, error) {
	correlation := c.cause
	return c.journal.EmitRouterEvent(ctx, c.registry, EmitRequest{
		ID: id, Version: version, Payload: append(json.RawMessage(nil), payload...),
		Source: "plugin:" + c.plugin, Correlation: &correlation,
	})
}

// NewRuntime validates the declarations and copies definitions and step lists.
func NewRuntime(journal Journal, bus *eventbus.Bus, events []EventDefinition, plugins []Plugin, config RunConfig) (*Runtime, error) {
	if journal == nil {
		return nil, errors.New("router journal is nil")
	}
	if bus == nil {
		return nil, errors.New("router event bus is nil")
	}
	pluginDefs := make([]PluginDefinition, len(plugins))
	ownedPlugins := make(map[string]Plugin, len(plugins))
	for i, plugin := range plugins {
		def := plugin.Definition
		def.Subscriptions = append([]Subscription(nil), plugin.Definition.Subscriptions...)
		for j := range def.Subscriptions {
			def.Subscriptions[j].DependsOn = append([]string(nil), def.Subscriptions[j].DependsOn...)
		}
		pluginDefs[i] = def
		ownedPlugins[def.ID] = Plugin{
			Definition: def,
			PreExec:    append([]Step(nil), plugin.PreExec...),
			Exec:       append([]Step(nil), plugin.Exec...),
			PostExec:   append([]Step(nil), plugin.PostExec...),
		}
	}
	ownedEvents := append([]EventDefinition(nil), events...)
	registry, err := NewRegistry(ownedEvents, pluginDefs)
	if err != nil {
		return nil, err
	}
	capacity := config.Capacity
	if capacity <= 0 {
		capacity = defaultRuntimeCapacity
	}
	if capacity > maxRuntimeCapacity {
		capacity = maxRuntimeCapacity
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Runtime{journal: journal, bus: bus, registry: registry, plugins: ownedPlugins, capacity: capacity, now: now}, nil
}

func (r *Runtime) Registry() *Registry { return r.registry }

type activeRun struct {
	claimed  ClaimedRun
	finished chan struct{}
	outcome  Outcome
	retries  int64
	nextTry  time.Time
}

// Run admits exactly one runtime loop, recovers durable running deliveries at
// startup, and joins all owned plugin work before releasing its journal token.
func (r *Runtime) Run(ctx context.Context) (retErr error) {
	r.runMu.Lock()
	if r.running {
		r.runMu.Unlock()
		return ErrRuntimeRunning
	}
	r.running = true
	r.runMu.Unlock()
	defer func() {
		r.runMu.Lock()
		r.running = false
		r.runMu.Unlock()
	}()

	// Subscribe before acquisition so changes racing startup remain observable.
	sub := r.bus.Subscribe(runtimeBusBuffer)
	var token string
	acquired := false
	defer func() {
		sub.Unsubscribe()
		if acquired {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), runtimeCleanupTimeout)
			defer cancel()
			retErr = errors.Join(retErr, r.journal.ReleaseRouterRuntime(cleanupCtx, token))
		}
	}()
	if err := sub.Err(); err != nil {
		return err
	}
	var err error
	token, err = r.journal.AcquireRouterRuntime(ctx, r.registry, r.now().UnixMilli())
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return err
	}
	acquired = true

	workCtx, cancelWorkers := context.WithCancel(ctx)
	active := make(map[*activeRun]struct{})
	completed := make(chan *activeRun, r.capacity)
	var admissionMu sync.Mutex
	pendingWrites := 0
	inboxes := make(map[string]chan *activeRun, len(r.plugins))
	var dispatchers sync.WaitGroup
	for _, pluginID := range r.registry.PluginIDs() {
		inbox := make(chan *activeRun, r.capacity)
		inboxes[pluginID] = inbox
		dispatchers.Add(1)
		go r.dispatchPlugin(workCtx, inbox, completed, &admissionMu, &pendingWrites, &dispatchers)
	}
	pending := make([]*activeRun, 0, r.capacity)
	var nextDue *int64
	claimRetryAt := time.Time{}
	for {
		if err := ctx.Err(); err != nil {
			break
		}
		// Drain ready outcomes before admitting more claims. The admission lock
		// also prevents a worker from publishing a completed outcome mid-claim.
		drainCompletions(completed, &pending)
		for i := 0; i < len(pending); {
			run := pending[i]
			if r.now().Before(run.nextTry) {
				i++
				continue
			}
			finishErr := r.journal.FinishRouterDelivery(ctx, token, r.registry, run.claimed, run.outcome, r.now().UnixMilli())
			if finishErr == nil {
				admissionMu.Lock()
				pendingWrites--
				admissionMu.Unlock()
				<-run.finished
				delete(active, run)
				pending = append(pending[:i], pending[i+1:]...)
				continue
			}
			if errors.Is(finishErr, ErrRuntimeInactive) {
				retErr = finishErr
				goto shutdown
			}
			run.retries++
			run.nextTry = r.now().Add(RetryDelay(run.retries))
			i++
		}

		admissionMu.Lock()
		canClaim := pendingWrites == 0 && len(active) < r.capacity && !r.now().Before(claimRetryAt)
		if canClaim {
			batch, claimErr := r.journal.ClaimRouterDeliveries(ctx, token, r.registry, r.now().UnixMilli(), r.capacity-len(active))
			if claimErr != nil {
				if errors.Is(claimErr, ErrRuntimeInactive) {
					admissionMu.Unlock()
					retErr = claimErr
					break
				}
				claimRetryAt = r.now().Add(time.Second)
				admissionMu.Unlock()
			} else {
				claimRetryAt = time.Time{}
				nextDue = batch.NextAttemptAt
				for _, claimed := range batch.Runs {
					if ctx.Err() != nil { // Never start a newly claimed step after shutdown.
						break
					}
					run := &activeRun{claimed: cloneClaimedRun(claimed), finished: make(chan struct{})}
					active[run] = struct{}{}
					if inbox, ok := inboxes[claimed.Plugin]; ok {
						inbox <- run
					} else {
						// Keep the existing unknown-plugin failure path for malformed
						// journal claims instead of blocking on a missing inbox.
						go r.execute(workCtx, run, completed, &admissionMu, &pendingWrites)
					}
				}
				admissionMu.Unlock()
				if batch.Skipped || len(batch.Runs) > 0 {
					continue
				}
			}
		} else {
			admissionMu.Unlock()
		}

		wait := time.Hour
		admissionMu.Lock()
		canReconcile := pendingWrites == 0 && len(active) < r.capacity
		admissionMu.Unlock()
		if canReconcile {
			if !claimRetryAt.IsZero() {
				wait = min(wait, max(time.Duration(0), claimRetryAt.Sub(r.now())))
			}
			if nextDue != nil {
				due := time.UnixMilli(*nextDue)
				wait = min(wait, max(time.Duration(0), due.Sub(r.now())))
			}
		}
		for _, run := range pending {
			wait = min(wait, max(time.Duration(0), run.nextTry.Sub(r.now())))
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			goto shutdown
		case run := <-completed:
			timer.Stop()
			pending = append(pending, run)
		case event, ok := <-sub.C:
			timer.Stop()
			if !ok {
				cause := sub.Err()
				if errors.Is(cause, eventbus.ErrSlowConsumer) {
					sub.Unsubscribe()
					sub = r.bus.Subscribe(runtimeBusBuffer)
					if err := sub.Err(); err != nil {
						retErr = err
						goto shutdown
					}
					continue
				}
				retErr = cause
				if retErr == nil {
					retErr = eventbus.ErrBusClosed
				}
				goto shutdown
			}
			if event.Topic == eventbus.RouterChanged || event.Topic == eventbus.Tick {
				continue
			}
		case <-timer.C:
			// Reconcile durable state for retry deadlines and missed notifications.
		}
	}

shutdown:
	cancelWorkers()
	for _, inbox := range inboxes {
		close(inbox)
	}
	dispatchers.Wait()
	for run := range active {
		<-run.finished
	}
	return retErr
}

func (r *Runtime) dispatchPlugin(ctx context.Context, inbox <-chan *activeRun, completed chan<- *activeRun, admissionMu *sync.Mutex, pendingWrites *int, dispatchers *sync.WaitGroup) {
	defer dispatchers.Done()
	for run := range inbox {
		go r.execute(ctx, run, completed, admissionMu, pendingWrites)
	}
}

func (r *Runtime) execute(ctx context.Context, run *activeRun, completed chan<- *activeRun, admissionMu *sync.Mutex, pendingWrites *int) {
	run.outcome = r.runSteps(ctx, run.claimed)
	admissionMu.Lock()
	(*pendingWrites)++
	completed <- run
	admissionMu.Unlock()
	close(run.finished)
}

func drainCompletions(completed <-chan *activeRun, pending *[]*activeRun) {
	for {
		select {
		case run := <-completed:
			*pending = append(*pending, run)
		default:
			return
		}
	}
}

func (r *Runtime) runSteps(ctx context.Context, claimed ClaimedRun) (outcome Outcome) {
	outcome = Outcome{Kind: Continue}
	plugin, ok := r.plugins[claimed.Plugin]
	if !ok {
		return Outcome{Kind: Fail, Error: fmt.Sprintf("plugin %q is not registered", claimed.Plugin)}
	}
	stepCtx := &StepContext{journal: r.journal, registry: r.registry, plugin: claimed.Plugin, cause: claimed.Event.UID}
	for _, phase := range [][]Step{plugin.PreExec, plugin.Exec, plugin.PostExec} {
		for _, step := range phase {
			if ctx.Err() != nil {
				return Outcome{Kind: Fail, Error: ctx.Err().Error()}
			}
			outcome = invokeStep(step, ctx, cloneEvent(claimed.Event), stepCtx)
			if outcome.Kind != Continue {
				if outcome.Kind > Fail {
					return Outcome{Kind: Fail, Error: fmt.Sprintf("plugin %q returned invalid outcome %d", claimed.Plugin, outcome.Kind)}
				}
				return outcome
			}
		}
	}
	return Outcome{Kind: Continue}
}

func invokeStep(step Step, ctx context.Context, event Event, stepCtx *StepContext) (out Outcome) {
	defer func() {
		if recovered := recover(); recovered != nil {
			out = Outcome{Kind: Fail, Error: fmt.Sprintf("plugin panicked: %v", recovered)}
		}
	}()
	if step == nil {
		return Outcome{Kind: Fail, Error: "plugin step is nil"}
	}
	return step(ctx, event, stepCtx)
}

func cloneClaimedRun(run ClaimedRun) ClaimedRun {
	run.Event = cloneEvent(run.Event)
	return run
}

func cloneEvent(event Event) Event {
	event.Payload = append(json.RawMessage(nil), event.Payload...)
	if event.Correlation != nil {
		correlation := *event.Correlation
		event.Correlation = &correlation
	}
	return event
}
