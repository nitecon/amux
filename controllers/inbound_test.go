package controllers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nitecon/amux/fism"
)

func TestInboundDefinitionAndPluginContract(t *testing.T) {
	definition := InboundDefinition()
	if definition.ID != InboundMessageReceivedID || definition.Version != 1 || definition.RemoteEmit {
		t.Fatalf("unexpected definition: %+v", definition)
	}
	plugin := InboundController(inboundStoreFunc(func(context.Context, fism.Event) (bool, error) { return false, nil }))
	if plugin.Definition.ID != "inbound" || !plugin.Definition.FIFO || plugin.Definition.MaxAttempts != 0 {
		t.Fatalf("unexpected plugin policy: %+v", plugin.Definition)
	}
	registry, err := fism.NewRegistry([]fism.EventDefinition{definition}, []fism.ControllerDefinition{plugin.Definition})
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Subscribers(InboundMessageReceivedID, 1); len(got) != 1 || got[0] != "inbound" {
		t.Fatalf("subscribers = %v", got)
	}
}

func TestInboundPayloadValidation(t *testing.T) {
	valid := []string{
		`{"channel_name":"","room_id":"","message":{"id":"","content":"","author_kind":""}}`,
		`{"channel_name":"discord","room_id":"r","message":{"id":"m","content":"x","author_kind":"human","extra":true},"extra":1}`,
	}
	decoded, err := DecodeInboundPayload(json.RawMessage(valid[0]))
	if err != nil || decoded.ChannelName != "" || decoded.RoomID != "" || decoded.Message != (InboundMessage{}) {
		t.Fatalf("decode empty-string payload = %+v, %v", decoded, err)
	}
	for _, raw := range valid {
		if err := InboundDefinition().Validate(json.RawMessage(raw)); err != nil {
			t.Errorf("valid payload rejected: %s: %v", raw, err)
		}
	}
	invalid := []string{
		`null`, `[]`, `{}`, `{"channel_name":null,"room_id":"r","message":{"id":"m","content":"x","author_kind":"human"}}`,
		`{"channel_name":"c","room_id":"r","message":{"id":"m","content":null,"author_kind":"human"}}`,
		`{"channel_name":"c","room_id":"r","message":{"id":"m","author_kind":"human"}}`,
		`{"channel_name":"c","room_id":"r","message":{"id":2,"content":"x","author_kind":"human"}}`,
		`{"channel_name":"c","room_id":"r","message":{"id":"m","content":"x","author_kind":"human"}} {}`,
	}
	for _, raw := range invalid {
		if err := InboundDefinition().Validate(json.RawMessage(raw)); err == nil {
			t.Errorf("invalid payload accepted: %s", raw)
		}
	}
}

type inboundStoreFunc func(context.Context, fism.Event) (bool, error)

func (f inboundStoreFunc) IngestRouterInbound(ctx context.Context, event fism.Event) (bool, error) {
	return f(ctx, event)
}

func TestInboundPluginOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop bool
		err  error
		want fism.OutcomeKind
	}{
		{name: "continue", want: fism.Continue},
		{name: "stop", stop: true, want: fism.Stop},
		{name: "retry", err: context.DeadlineExceeded, want: fism.Fail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plugin := InboundController(inboundStoreFunc(func(context.Context, fism.Event) (bool, error) { return tc.stop, tc.err }))
			got := plugin.Exec[0](context.Background(), fism.Event{}, nil)
			if got.Kind != tc.want {
				t.Fatalf("outcome = %+v", got)
			}
		})
	}
}
