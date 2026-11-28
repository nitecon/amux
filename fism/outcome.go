package fism

import (
	"fmt"
	"time"
)

// ControllerDecision describes how the state machine handles a processor result.
// RetryAfter is relative to the current pass's clock.
type ControllerDecision struct {
	State      string
	Failures   int64
	RetryAfter time.Duration
	Notify     bool
}

func (r *Registry) DecideControllerOutcome(run ClaimedRun, outcome Outcome) (ControllerDecision, error) {
	decision := ControllerDecision{Failures: run.Failures}
	switch outcome.Kind {
	case Continue:
		decision.State = "completed"
	case Stop:
		decision.State = "stopped"
	case Fail:
		decision.Failures++
		maxAttempts := r.MaxAttempts(run.Plugin)
		if maxAttempts != 0 && decision.Failures >= int64(maxAttempts) {
			decision.State = "failed"
			break
		}
		decision.State = "retrying"
		decision.RetryAfter = RetryDelay(decision.Failures)
		decision.Notify = run.Event.ID != retryingEventID &&
			decision.RetryAfter == RetryDelay(6) &&
			RetryDelay(decision.Failures-1) < decision.RetryAfter
	default:
		return ControllerDecision{}, fmt.Errorf("unknown router outcome kind %d", outcome.Kind)
	}
	return decision, nil
}
