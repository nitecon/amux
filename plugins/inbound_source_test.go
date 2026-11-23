package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nitecon/amux/channels"
	"github.com/nitecon/amux/router"
)

type inboundEmitterFunc func(context.Context, *router.Registry, router.EmitRequest) (router.EmitResult, error)

func (f inboundEmitterFunc) EmitRouterEvent(ctx context.Context, registry *router.Registry, request router.EmitRequest) (router.EmitResult, error) {
	return f(ctx, registry, request)
}

func TestRunInboundSourceBackpressuresAndRetriesSameUIDInOrder(t *testing.T) {
	registry, err := router.NewRegistry([]router.EventDefinition{InboundDefinition()}, []router.PluginDefinition{{
		ID: "inbound", Subscriptions: []router.Subscription{{Event: InboundMessageReceivedID, MinVersion: 1, MaxVersion: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	input := make(chan channels.InboundEvent, 256)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type captured struct {
		request router.EmitRequest
		payload InboundPayload
	}
	var mu sync.Mutex
	var calls []captured
	released := false
	emitter := inboundEmitterFunc(func(_ context.Context, _ *router.Registry, request router.EmitRequest) (router.EmitResult, error) {
		payload, err := DecodeInboundPayload(request.Payload)
		if err != nil {
			return router.EmitResult{}, err
		}
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, captured{request: request, payload: payload})
		if !released {
			return router.EmitResult{}, errors.New("fixture bus/storage unavailable")
		}
		return router.EmitResult{Stored: true}, nil
	})
	consumerDone := make(chan error, 1)
	go func() {
		consumerDone <- RunInboundSource(ctx, emitter, registry, input)
		close(consumerDone)
	}()

	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		defer close(input)
		for i := range 300 {
			event := channels.InboundEvent{ChannelName: "discord", RoomID: "room", Message: channels.InboundMessage{
				ID: fmt.Sprintf("message-%03d", i), Content: fmt.Sprintf("body-%03d", i), Sender: "not-persisted", AuthorKind: "human",
			}}
			select {
			case <-ctx.Done():
				return
			case input <- event:
			}
		}
	}()
	defer func() {
		cancel()
		<-producerDone
		<-consumerDone
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		seen := len(calls)
		mu.Unlock()
		if seen >= 2 && len(input) == cap(input) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("producer did not fill the bounded queue; calls=%d queue=%d/%d", seen, len(input), cap(input))
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-producerDone:
		t.Fatal("producer bypassed full bounded queue while durable emission was failing")
	default:
	}
	mu.Lock()
	failureCount := len(calls)
	if failureCount == 0 {
		mu.Unlock()
		t.Fatal("expected a failed initial emit before releasing the fixture")
	}
	released = true
	mu.Unlock()
	select {
	case <-producerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("producer did not finish after durable emission recovered")
	}
	select {
	case err := <-consumerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("inbound source did not drain accepted provider backlog")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != failureCount+300 {
		t.Fatalf("emission attempts=%d, want %d failed attempts plus 300 successful events", len(calls), failureCount)
	}
	firstUID := calls[failureCount].request.UID
	if firstUID == "" {
		t.Fatal("first successful logical event has empty UID")
	}
	for i := 0; i < failureCount; i++ {
		if calls[i].request.UID != firstUID {
			t.Fatalf("first logical event changed UID across retry: failed[%d]=%q success=%q", i, calls[i].request.UID, firstUID)
		}
	}
	uids := make(map[string]struct{}, 300)
	for i := 0; i < 300; i++ {
		got := calls[failureCount+i]
		if got.request.UID == "" {
			t.Fatalf("event[%d] has empty UID", i)
		}
		if _, exists := uids[got.request.UID]; exists {
			t.Fatalf("distinct provider inputs reused UID %q", got.request.UID)
		}
		uids[got.request.UID] = struct{}{}
		if got.payload.Message.ID != fmt.Sprintf("message-%03d", i) || got.request.Source != "channel:discord" {
			t.Fatalf("event[%d]=%+v source=%q", i, got.payload, got.request.Source)
		}
		if got.payload.Message.AuthorKind != "human" || got.payload.Message.Content != fmt.Sprintf("body-%03d", i) {
			t.Fatalf("event[%d] payload=%+v", i, got.payload)
		}
		var raw map[string]any
		if err := json.Unmarshal(got.request.Payload, &raw); err != nil {
			t.Fatal(err)
		}
		message := raw["message"].(map[string]any)
		if _, exists := message["sender"]; exists {
			t.Fatalf("provider sender leaked into durable payload: %s", got.request.Payload)
		}
	}
}

func TestRunInboundSourceCancellationInterruptsRetry(t *testing.T) {
	registry, err := router.NewRegistry([]router.EventDefinition{InboundDefinition()}, []router.PluginDefinition{{
		ID: "inbound", Subscriptions: []router.Subscription{{Event: InboundMessageReceivedID, MinVersion: 1, MaxVersion: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	input := make(chan channels.InboundEvent, 1)
	input <- channels.InboundEvent{ChannelName: "discord", RoomID: "room", Message: channels.InboundMessage{ID: "m"}}
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	emitter := inboundEmitterFunc(func(context.Context, *router.Registry, router.EmitRequest) (router.EmitResult, error) {
		close(entered)
		return router.EmitResult{}, errors.New("temporary failure")
	})
	done := make(chan error, 1)
	go func() { done <- RunInboundSource(ctx, emitter, registry, input) }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("source did not cancel its retry delay")
	}
}
