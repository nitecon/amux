package fism

import (
	"testing"
	"time"
)

func TestControllerOutcomeUsesRetryPolicy(t *testing.T) {
	registry, err := NewRegistry(nil, []ControllerDefinition{{ID: "limited", MaxAttempts: 2}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		run  ClaimedRun
		kind OutcomeKind
		want ControllerDecision
	}{
		{"completion", ClaimedRun{}, Continue, ControllerDecision{State: "completed"}},
		{"stop", ClaimedRun{Failures: 1}, Stop, ControllerDecision{State: "stopped", Failures: 1}},
		{"retry", ClaimedRun{Plugin: "limited"}, Fail, ControllerDecision{State: "retrying", Failures: 1, RetryAfter: time.Second}},
		{"exhausted", ClaimedRun{Plugin: "limited", Failures: 1}, Fail, ControllerDecision{State: "failed", Failures: 2}},
		{"first capped retry", ClaimedRun{Failures: 5}, Fail, ControllerDecision{State: "retrying", Failures: 6, RetryAfter: 30 * time.Second, Notify: true}},
		{"already capped", ClaimedRun{Failures: 6}, Fail, ControllerDecision{State: "retrying", Failures: 7, RetryAfter: 30 * time.Second}},
		{"notification cannot notify again", ClaimedRun{Event: Event{ID: retryingEventID}, Failures: 5}, Fail, ControllerDecision{State: "retrying", Failures: 6, RetryAfter: 30 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := registry.DecideControllerOutcome(tc.run, Outcome{Kind: tc.kind})
			if err != nil || got != tc.want {
				t.Fatalf("decision = %+v, %v, want %+v", got, err, tc.want)
			}
		})
	}
}
