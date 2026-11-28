package queues

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestManagerOpensIndependentTopics(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	manager := Manager{}
	started := make(chan struct{})
	first, err := manager.Open(ctx, "wsock.session", Options{Capacity: 1, RejectFull: true}, func(_, _ string) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan string, 1)
	second, err := manager.Open(ctx, "router.plugin", Options{Capacity: 1, RejectFull: true}, func(id, payload string) error {
		delivered <- id + ":" + payload
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Enqueue(ctx, "frame", "body", 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("socket processor did not start")
	}
	if err := first.Enqueue(ctx, "extra", "body", 0); !errors.Is(err, ErrFull) {
		t.Fatalf("saturated topic returned %v", err)
	}
	if err := second.Enqueue(ctx, "run", "event", 0); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-delivered:
		if got != "run:event" {
			t.Fatalf("router topic received %q", got)
		}
	case <-ctx.Done():
		t.Fatal("router topic was blocked")
	}
	cancel()
	<-first.Done()
	<-second.Done()
}
