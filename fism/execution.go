package fism

import (
	"context"
	"errors"
	"time"

	"github.com/nitecon/amux/queues"
)

// OutcomeKind controls the remainder of a plugin run.
type OutcomeKind uint8

const (
	Continue OutcomeKind = iota
	Stop
	Fail
)

type Outcome struct {
	Kind  OutcomeKind
	Error string
}

// Step must honor ctx cancellation. Runs may replay after a crash, so effects
// must be idempotent by event UID.
type Step func(context.Context, Event, *StepContext) Outcome

type Controller struct {
	Definition ControllerDefinition
	PreExec    []Step
	Exec       []Step
	PostExec   []Step
}

type ClaimedRun struct {
	Event  Event
	Plugin string
	// Failures snapshots the delivery failure count when claimed, fencing late finishes.
	Failures int64
}
type ClaimBatch struct {
	Runs          []ClaimedRun
	NextAttemptAt *int64
	Skipped       bool
}

var ErrRuntimeInactive = errors.New("router runtime is not active")

// Journal owns atomic durable transitions. The host must hold its database
// profile's OS lock; the token fences one runtime lifetime within that host.
type Journal interface {
	EmitRouterEvent(context.Context, *Registry, EmitRequest) (EmitResult, error)
	AcquireRouterRuntime(context.Context, *Registry, int64) (string, error)
	ClaimRouterDeliveries(context.Context, string, *Registry, int64, int) (ClaimBatch, error)
	FinishRouterDelivery(context.Context, string, *Registry, ClaimedRun, Outcome, int64) error
	ReleaseRouterRuntime(context.Context, string) error
}

// RunConfig bounds owned executions. Now is injectable for deterministic tick
// reconciliation tests; nil uses time.Now.
type RunConfig struct {
	Capacity     int
	QueueManager queues.Manager
	Now          func() time.Time
}

// RetryDelay follows the router's one-second exponential retry schedule.
func RetryDelay(failures int64) time.Duration {
	if failures <= 0 {
		return 0
	}
	if failures >= 6 {
		return 30 * time.Second
	}
	return time.Second << uint(failures-1)
}
