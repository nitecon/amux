package queues

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryQueueKeepsFailedHeadWithinCapacity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	attempts := make(chan string, 4)
	first := true
	q, err := NewMemoryQueue(2, func(id, _ string) error {
		attempts <- id
		if first {
			first = false
			return errors.New("temporarily offline")
		}
		return nil
	}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if err := q.Enqueue(ctx, id, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-attempts:
			if got != want {
				t.Fatalf("processed %q before %q", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("never processed %q", want)
		}
	}
	expect("one")
	wait, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	if err := q.Enqueue(wait, "three", "", 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failed head did not count toward capacity: %v", err)
	}
	q.ConnectResponse()
	expect("one")
	expect("two")
}

func TestMemoryQueueExpiresFailedMessageWithoutReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	attempts := make(chan string, 2)
	q, err := NewMemoryQueue(1, func(id, _ string) error {
		attempts <- id
		return errors.New("still offline")
	}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, "expires", "", 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-attempts:
	case <-ctx.Done():
		t.Fatal("head was never attempted")
	}
	if err := q.Enqueue(ctx, "next", "", 0); err != nil {
		t.Fatalf("expired head did not release capacity: %v", err)
	}
	select {
	case id := <-attempts:
		if id != "next" {
			t.Fatalf("processed expired message %q", id)
		}
	case <-ctx.Done():
		t.Fatal("next message was never attempted")
	}
	cancel()
	if err := q.Enqueue(context.Background(), "stopped", "", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown enqueue: %v", err)
	}
}
