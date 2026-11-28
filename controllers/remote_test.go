package controllers

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/nitecon/amux/fism"
)

func TestRemoteForwardedEventsMatchesCurrentTaskAndPresenceContract(t *testing.T) {
	want := []string{
		"task.created", "task.updated", "task.commented", "task.assigned",
		"task.started", "task.user_review", "task.completed", "task.cancelled",
		"task.unclaimed", "agent.online", "agent.idle", "agent.busy", "agent.gone",
		NotifyEventID, GlobalNotifyEventID, "router.retrying",
	}
	got := RemoteForwardedEvents()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("forwarded events=%v, want %v", got, want)
	}
	got[0] = "mutated"
	if RemoteForwardedEvents()[0] != "task.created" {
		t.Fatal("RemoteForwardedEvents exposed its backing list")
	}
}

func TestRemotePluginSnapshotsDeduplicatedAllowlistAndMetadata(t *testing.T) {
	forwarded := []string{"task.created", "agent.idle", "task.created"}
	plugin := RemoteController(forwarded)
	forwarded[0] = "changed.after.construction"
	forwarded[1] = "also.changed"
	forwarded[2] = "duplicate.changed"

	want := []fism.Subscription{
		{Event: "task.created", MinVersion: 1, MaxVersion: ^uint32(0)},
		{Event: "agent.idle", MinVersion: 1, MaxVersion: ^uint32(0)},
	}
	if plugin.Definition.ID != "remote" || plugin.Definition.FIFO || plugin.Definition.MaxAttempts != 0 {
		t.Fatalf("unexpected remote policy: %+v", plugin.Definition)
	}
	if len(plugin.PreExec) != 0 || len(plugin.PostExec) != 0 || len(plugin.Exec) != 1 {
		t.Fatalf("remote plugin has unexpected steps: pre=%d exec=%d post=%d", len(plugin.PreExec), len(plugin.Exec), len(plugin.PostExec))
	}
	if !reflect.DeepEqual(plugin.Definition.Subscriptions, want) {
		t.Fatalf("subscriptions=%+v, want %+v", plugin.Definition.Subscriptions, want)
	}

	events := make([]fism.EventDefinition, 0, len(want))
	for _, subscription := range want {
		events = append(events, fism.EventDefinition{
			ID: subscription.Event, Version: 1,
			Validate: func(json.RawMessage) error { return nil },
		})
	}
	registry, err := fism.NewRegistry(events, []fism.ControllerDefinition{plugin.Definition})
	if err != nil {
		t.Fatalf("remote plugin registration failed: %v", err)
	}
	if registry.FIFO("remote") {
		t.Fatal("remote plugin unexpectedly uses FIFO delivery")
	}
	if plugin.Definition.ID != "remote" {
		t.Fatalf("plugin ID=%q, want remote", plugin.Definition.ID)
	}
}

func TestRemotePluginStepContinuesRuntime(t *testing.T) {
	plugin := RemoteController([]string{"task.created"})
	ctx := context.WithValue(context.Background(), struct{}{}, "forwarded")
	event := fism.Event{ID: "task.created", Version: 1}
	if outcome := plugin.Exec[0](ctx, event, nil); outcome.Kind != fism.Continue {
		t.Fatalf("remote execution outcome=%+v, want Continue", outcome)
	}
}
