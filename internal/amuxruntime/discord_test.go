package amuxruntime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nitecon/amux/channels"
	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

type registeredDiscordRoom struct {
	id      string
	afterID *string
}

type discordRegistrarFunc func(string, *string) error

func (f discordRegistrarFunc) RegisterRoom(id string, afterID *string) error {
	return f(id, afterID)
}

func TestRegisterDiscordRoomsIncludesGlobalAndStoredCursor(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "rooms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	last := "message-12"
	projects := []storage.Project{
		{Ident: "normal", ChannelName: "discord", RoomID: "room-1", LastMsgID: &last},
		{Ident: "__global__", ChannelName: "discord", RoomID: "room-global"},
		{Ident: "blank", ChannelName: "discord"},
		{Ident: "other", ChannelName: "matrix", RoomID: "room-other"},
	}
	for _, project := range projects {
		if err := store.InsertProject(ctx, project); err != nil {
			t.Fatal(err)
		}
	}
	var got []registeredDiscordRoom
	registrar := discordRegistrarFunc(func(id string, afterID *string) error {
		var cursor *string
		if afterID != nil {
			value := *afterID
			cursor = &value
		}
		got = append(got, registeredDiscordRoom{id: id, afterID: cursor})
		return nil
	})
	if err := registerDiscordRooms(ctx, store, registrar); err != nil {
		t.Fatal(err)
	}
	want := []registeredDiscordRoom{{id: "room-1", afterID: &last}, {id: "room-global"}}
	sort.Slice(got, func(i, j int) bool { return got[i].id < got[j].id })
	sort.Slice(want, func(i, j int) bool { return want[i].id < want[j].id })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registered rooms=%+v, want %+v", got, want)
	}
}

func TestRegisterDiscordRoomsSkipsInvalidRoomAndContinues(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "invalid-room.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, project := range []storage.Project{
		{Ident: "bad-room", ChannelName: "discord", RoomID: "invalid"},
		{Ident: "good-room", ChannelName: "discord", RoomID: "123"},
	} {
		if err := store.InsertProject(ctx, project); err != nil {
			t.Fatal(err)
		}
	}
	var registered []string
	registrar := discordRegistrarFunc(func(id string, _ *string) error {
		if id == "invalid" {
			return errors.New("room id is not a snowflake")
		}
		registered = append(registered, id)
		return nil
	})
	if err := registerDiscordRooms(ctx, store, registrar); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(registered, []string{"123"}) {
		t.Fatalf("registered rooms=%v, want later valid room despite invalid legacy room", registered)
	}
}

type fixtureDiscordInbound struct {
	events []channels.InboundEvent
	sent   atomic.Int64
	runErr error
}

func (*fixtureDiscordInbound) RegisterRoom(string, *string) error { return nil }

func (p *fixtureDiscordInbound) Run(ctx context.Context, sink chan<- channels.InboundEvent) error {
	for _, event := range p.events {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case sink <- event:
			p.sent.Add(1)
		}
	}
	if p.runErr != nil {
		return p.runErr
	}
	<-ctx.Done()
	return ctx.Err()
}

type fixtureRouterEmitter func(context.Context, *router.Registry, router.EmitRequest) (router.EmitResult, error)

func (f fixtureRouterEmitter) EmitRouterEvent(ctx context.Context, registry *router.Registry, request router.EmitRequest) (router.EmitResult, error) {
	return f(ctx, registry, request)
}

func TestDiscordIngressStartsConsumerBeforeLargeSynchronousBackfill(t *testing.T) {
	registry, err := router.NewRegistry([]router.EventDefinition{plugins.InboundDefinition()}, []router.PluginDefinition{{
		ID: "inbound", Subscriptions: []router.Subscription{{Event: plugins.InboundMessageReceivedID, MinVersion: 1, MaxVersion: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	provider := &fixtureDiscordInbound{}
	for i := range 300 {
		provider.events = append(provider.events, channels.InboundEvent{ChannelName: "discord", RoomID: "123", Message: channels.InboundMessage{
			ID: fmt.Sprintf("message-%03d", i), Content: fmt.Sprintf("body-%03d", i), AuthorKind: "human",
		}})
	}
	var mu sync.Mutex
	released := false
	var failedUIDs []string
	var accepted []router.EmitRequest
	emitter := fixtureRouterEmitter(func(_ context.Context, _ *router.Registry, request router.EmitRequest) (router.EmitResult, error) {
		mu.Lock()
		defer mu.Unlock()
		if !released {
			failedUIDs = append(failedUIDs, request.UID)
			return router.EmitResult{}, errors.New("fixture journal unavailable")
		}
		accepted = append(accepted, request)
		return router.EmitResult{Stored: true}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	ingress := startDiscordIngress(ctx, provider, emitter, registry)
	deadline := time.Now().Add(3 * time.Second)
	for provider.sent.Load() < discordInboundQueueSize+1 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("synchronous provider backfill deadlocked before consumer startup")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	if len(failedUIDs) == 0 {
		mu.Unlock()
		cancel()
		t.Fatal("provider backfill did not reach the failing emitter")
	}
	released = true
	mu.Unlock()
	for {
		mu.Lock()
		count := len(accepted)
		mu.Unlock()
		if count == len(provider.events) && provider.sent.Load() == int64(len(provider.events)) {
			break
		}
		if time.Now().After(deadline.Add(10 * time.Second)) {
			cancel()
			t.Fatalf("backfill drain stalled: accepted=%d sent=%d", count, provider.sent.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-ingress.providerDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("provider shutdown error=%v", err)
	}
	if err := <-ingress.sourceDone; err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(failedUIDs) == 0 || failedUIDs[0] != accepted[0].UID {
		t.Fatalf("first event UID changed across emit retry: failed=%v accepted=%q", failedUIDs, accepted[0].UID)
	}
	for _, uid := range failedUIDs {
		if uid != failedUIDs[0] {
			t.Fatalf("first event UID changed across emit retries: %v", failedUIDs)
		}
	}
	for i, request := range accepted {
		if request.Source != "channel:discord" || request.ID != plugins.InboundMessageReceivedID {
			t.Fatalf("accepted[%d] request=%+v", i, request)
		}
		payload, err := plugins.DecodeInboundPayload(request.Payload)
		if err != nil || payload.Message.ID != fmt.Sprintf("message-%03d", i) {
			t.Fatalf("accepted[%d] payload=%+v err=%v", i, payload, err)
		}
	}
}

func TestDiscordProviderFailureDrainsIngressWithoutStoppingRuntime(t *testing.T) {
	registry, err := router.NewRegistry([]router.EventDefinition{plugins.InboundDefinition()}, []router.PluginDefinition{{
		ID: "inbound", Subscriptions: []router.Subscription{{Event: plugins.InboundMessageReceivedID, MinVersion: 1, MaxVersion: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "provider-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler, err := server.NewHandler(store, "fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("fixture Discord gateway failure")
	provider := &fixtureDiscordInbound{runErr: failure, events: []channels.InboundEvent{{
		ChannelName: "discord", RoomID: "123", Message: channels.InboundMessage{ID: "queued", Content: "body", AuthorKind: "human"},
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx, listener, handler, time.Second) }()
	var accepted atomic.Int64
	emitter := fixtureRouterEmitter(func(context.Context, *router.Registry, router.EmitRequest) (router.EmitResult, error) {
		accepted.Add(1)
		return router.EmitResult{Stored: true}, nil
	})
	ingress := startDiscordIngress(ctx, provider, emitter, registry)
	if err := <-ingress.providerDone; !errors.Is(err, failure) {
		t.Fatalf("provider error=%v, want fixture failure", err)
	}
	if err := <-ingress.sourceDone; err != nil {
		t.Fatal(err)
	}
	if accepted.Load() != 1 {
		t.Fatalf("durably accepted buffered events=%d, want 1", accepted.Load())
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("provider failure stopped shared server runtime: %v", err)
	}
	request, err := http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+"/v1/projects", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer fixture-key")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("native API unavailable after Discord failure: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("native API status after Discord failure=%d, want 200", response.StatusCode)
	}
	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("shutdown native API: %v", err)
	}
}
