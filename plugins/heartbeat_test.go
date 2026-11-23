package plugins

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

type heartbeatDrainerFunc func(context.Context, *router.Registry, int) (int, error)

func (f heartbeatDrainerFunc) DrainRouterHeartbeatOutbox(ctx context.Context, r *router.Registry, limit int) (int, error) {
	return f(ctx, r, limit)
}

func TestClientHeartbeatDefinitionAndPayloadContract(t *testing.T) {
	definition := ClientHeartbeatDefinition()
	if definition.ID != ClientHeartbeatID || definition.Version != 1 || !definition.RemoteEmit {
		t.Fatalf("unexpected heartbeat definition: %+v", definition)
	}
	valid := []string{
		`{"agents":[]}`,
		`{"agents":[],"os":"windows"}`,
		`{"agents":[],"os":"linux"}`,
		`{"agents":[{"session_id":"s","instance_id":"i","provider":"p","os":"linux","project":"x"}]}`,
		`{"agents":[],"client_run":"123e4567-e89b-12d3-a456-426614174000","os":"macos","hostname":"host","report_sequence":8,"reported_at":12}`,
		`{"agents":[{"session_id":"s","instance_id":"i","provider":"p","os":"windows","project":"x","name":null,"generation":"g","native_extra":true}]}`,
	}
	for _, payload := range valid {
		if err := definition.Validate([]byte(payload)); err != nil {
			t.Errorf("valid payload rejected: %s: %v", payload, err)
		}
	}
	invalid := []string{
		`[]`, `{}`, `{"agents":null}`, `{"agents":[{}]}`,
		`{"agents":[{"session_id":1,"instance_id":"i","provider":"p","os":"linux","project":"x"}]}`,
		`{"agents":[{"session_id":null,"instance_id":"i","provider":"p","os":"linux","project":"x"}]}`,
		`{"agents":[{"session_id":"s","instance_id":null,"provider":"p","os":"linux","project":"x"}]}`,
		`{"agents":[{"session_id":"s","instance_id":"i","provider":null,"os":"linux","project":"x"}]}`,
		`{"agents":[{"session_id":"s","instance_id":"i","provider":"p","os":null,"project":"x"}]}`,
		`{"agents":[{"session_id":"s","instance_id":"i","provider":"p","os":"linux","project":null}]}`,
		`{"agents":[{"session_id":"s","instance_id":"i","provider":"p","os":"linux","project":"x","goal":false}]}`,
		`{"agents":[],"client_run":"nope"}`, `{"agents":[],"client_run":1}`,
		`{"agents":[],"os":"darwin"}`, `{"agents":[],"os":42}`, `{"agents":[],"hostname":1}`,
		`{"agents":[],"hostname":"` + strings.Repeat("h", 256) + `"}`,
	}
	for _, payload := range invalid {
		if err := definition.Validate([]byte(payload)); err == nil {
			t.Errorf("invalid payload accepted: %s", payload)
		}
	}
}

func TestRunHeartbeatSourceStartupWakeAndCancellation(t *testing.T) {
	bus := eventbus.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := sourceRegistry(t)
	calls := make(chan *router.Registry, 8)
	done := make(chan error, 1)
	go func() {
		done <- RunHeartbeatSource(ctx, heartbeatDrainerFunc(func(_ context.Context, got *router.Registry, limit int) (int, error) {
			if limit != sourceDrainBatchSize {
				t.Errorf("drain limit=%d, want %d", limit, sourceDrainBatchSize)
			}
			calls <- got
			return 0, nil
		}), registry, bus)
	}()
	select {
	case got := <-calls:
		if got != registry {
			t.Fatal("startup drain used a different registry")
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat source did not drain on startup")
	}
	if err := bus.Publish(eventbus.Event{Topic: eventbus.Tick}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("clock tick did not wake heartbeat drain")
	}
	if err := bus.Publish(eventbus.Event{Topic: eventbus.PresenceChanged}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("presence change did not wake heartbeat drain")
	}
	if err := bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-calls:
		t.Fatal("router change unexpectedly woke heartbeat drain")
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("source returned on cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat source did not stop on cancellation")
	}
}

func TestRunHeartbeatSourceValidatesArgumentsAndPropagatesBusClose(t *testing.T) {
	if err := RunHeartbeatSource(nil, nil, nil, nil); err == nil {
		t.Fatal("nil arguments accepted")
	}
	bus := eventbus.New()
	done := make(chan error, 1)
	go func() {
		done <- RunHeartbeatSource(context.Background(), heartbeatDrainerFunc(func(context.Context, *router.Registry, int) (int, error) {
			return 0, nil
		}), sourceRegistry(t), bus)
	}()
	bus.Close()
	select {
	case err := <-done:
		if !errors.Is(err, eventbus.ErrBusClosed) {
			t.Fatalf("source error=%v, want ErrBusClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat source did not stop after bus close")
	}
}

func TestPresenceAndReminderSubscribeToHeartbeatWithPresenceDependency(t *testing.T) {
	presence := PresencePlugin(nil)
	if len(presence.Definition.Subscriptions) != 4 {
		t.Fatalf("presence subscriptions=%+v", presence.Definition.Subscriptions)
	}
	if got := presence.Definition.Subscriptions[1]; got.Event != ClientHeartbeatID || got.MinVersion != 1 || got.MaxVersion != 1 || len(got.DependsOn) != 0 {
		t.Fatalf("presence heartbeat subscription=%+v", got)
	}
	reminder := ReminderPlugin(nil)
	if reminder.Definition.ID != reminderPluginID || len(reminder.Definition.Subscriptions) != 2 {
		t.Fatalf("unexpected reminder definition: %+v", reminder.Definition)
	}
	for _, sub := range reminder.Definition.Subscriptions {
		if sub.MinVersion != 1 || sub.MaxVersion != 1 || !reflect.DeepEqual(sub.DependsOn, []string{presencePluginID}) {
			t.Errorf("unexpected reminder subscription: %+v", sub)
		}
	}
	_, err := router.NewRegistry(presenceTestDefinitions(), []router.PluginDefinition{presence.Definition, reminder.Definition})
	if err != nil {
		t.Fatalf("presence and reminder definitions failed registry validation: %v", err)
	}
	_, err = router.NewRegistry(presenceTestDefinitions(), []router.PluginDefinition{reminder.Definition})
	if err == nil {
		t.Fatal("reminder registry accepted missing presence dependencies")
	}
}
