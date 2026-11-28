package fism

import (
	"math"
	"testing"
	"time"
)

func TestRetryDelayDoublesAndCaps(t *testing.T) {
	for _, tc := range []struct {
		failures int64
		want     time.Duration
	}{{-1, 0}, {0, 0}, {1, time.Second}, {2, 2 * time.Second}, {3, 4 * time.Second}, {4, 8 * time.Second}, {5, 16 * time.Second}, {6, 30 * time.Second}, {7, 30 * time.Second}, {math.MaxInt64, 30 * time.Second}} {
		if got := RetryDelay(tc.failures); got != tc.want {
			t.Fatalf("RetryDelay(%d)=%s want %s", tc.failures, got, tc.want)
		}
	}
}

func TestRegistryCopiesExecutionPolicies(t *testing.T) {
	plugins := []ControllerDefinition{{ID: "work", FIFO: true, MaxAttempts: 3, Subscriptions: []Subscription{{Event: "test.work", MinVersion: 1, MaxVersion: 1}}}}
	r, err := NewRegistry([]EventDefinition{{ID: "test.work", Version: 1, Validate: Object}}, plugins)
	if err != nil {
		t.Fatal(err)
	}
	plugins[0].FIFO = false
	plugins[0].MaxAttempts = 1
	if !r.FIFO("work") || r.MaxAttempts("work") != 3 {
		t.Fatal("caller mutation changed registered policy")
	}
	ids := r.ControllerIDs()
	ids[0] = "changed"
	if got := r.ControllerIDs(); len(got) != 1 || got[0] != "work" {
		t.Fatalf("plugin IDs aliased %v", got)
	}
}
