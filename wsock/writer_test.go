package wsock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nitecon/amux/queues"
)

func TestWriterBoundsBacklogAndIsolatesConnections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	started := make(chan string, 2)
	unblock := make(chan struct{})
	slow, err := NewWriter(ctx, queues.Manager{}, "slow", 1, time.Second, func(ctx context.Context, _ websocket.MessageType, body []byte) error {
		started <- string(body)
		select {
		case <-unblock:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	fastFrames := make(chan string, 1)
	fast, err := NewWriter(ctx, queues.Manager{}, "fast", 1, time.Second, func(_ context.Context, _ websocket.MessageType, body []byte) error {
		fastFrames <- string(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()

	send := func(writer *Writer, value int) {
		t.Helper()
		if err := writer.EnqueueJSON(value); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(frames <-chan string, want string) {
		t.Helper()
		select {
		case got := <-frames:
			if got != want {
				t.Fatalf("frame = %q, want %q", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("frame %q did not arrive", want)
		}
	}
	send(slow, 1)
	expect(started, "1")
	send(slow, 2)
	if err := slow.EnqueueJSON(3); !errors.Is(err, ErrOutboundFull) {
		t.Fatalf("saturated writer returned %v", err)
	}
	send(fast, 4)
	expect(fastFrames, "4")
	close(unblock)
	expect(started, "2")
}
