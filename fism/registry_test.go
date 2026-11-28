package fism

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func validObject(json.RawMessage) error { return nil }

func definitions(id string, versions ...uint32) []EventDefinition {
	events := make([]EventDefinition, 0, len(versions))
	for _, version := range versions {
		events = append(events, EventDefinition{ID: id, Version: version, RemoteEmit: true, Validate: Object})
	}
	return events
}

func TestRegistryDependencyCoverageIsPerVersion(t *testing.T) {
	events := definitions("work", 1, 2)
	plugins := []ControllerDefinition{
		{ID: "base", Subscriptions: []Subscription{{Event: "work", MinVersion: 1, MaxVersion: 2}}},
		{ID: "dependent", Subscriptions: []Subscription{{Event: "work", MinVersion: 1, MaxVersion: 2, DependsOn: []string{"base"}}}},
	}
	if _, err := NewRegistry(events, plugins); err != nil {
		t.Fatalf("fully covered dependency rejected: %v", err)
	}

	plugins[0].Subscriptions[0].MinVersion = 2
	if _, err := NewRegistry(events, plugins); err == nil {
		t.Fatal("dependency lacking coverage for v1 was accepted")
	}
}

func TestRegistryRejectsVersionWithoutDeclaredMatch(t *testing.T) {
	_, err := NewRegistry(definitions("work", 1, 3), []ControllerDefinition{{
		ID:            "consumer",
		Subscriptions: []Subscription{{Event: "work", MinVersion: 2, MaxVersion: 2}},
	}})
	if err == nil {
		t.Fatal("subscription matching no declared version was accepted")
	}
}

func TestRegistryRejectsDependencyCycle(t *testing.T) {
	plugins := []ControllerDefinition{
		{ID: "a", Subscriptions: []Subscription{{Event: "work", MinVersion: 1, MaxVersion: 1, DependsOn: []string{"b"}}}},
		{ID: "b", Subscriptions: []Subscription{{Event: "work", MinVersion: 1, MaxVersion: 1, DependsOn: []string{"a"}}}},
	}
	if _, err := NewRegistry(definitions("work", 1), plugins); err == nil {
		t.Fatal("dependency cycle was accepted")
	}
}

func TestRegistryCopiesCallerMetadataAndReturnedSlices(t *testing.T) {
	deps := []string{"base"}
	event := EventDefinition{ID: "work", Version: 1, RemoteEmit: true, Validate: Object}
	events := []EventDefinition{event}
	plugins := []ControllerDefinition{
		{ID: "base", Subscriptions: []Subscription{{Event: "work", MinVersion: 1, MaxVersion: 1}}},
		{ID: "consumer", Subscriptions: []Subscription{{Event: "work", MinVersion: 1, MaxVersion: 1, DependsOn: deps}}},
	}
	registry, err := NewRegistry(events, plugins)
	if err != nil {
		t.Fatal(err)
	}
	deps[0] = "missing"
	plugins[0].ID = "changed"
	plugins[1].Subscriptions[0].Event = "changed"
	events[0].ID = "changed"

	got := registry.Subscribers("work", 1)
	if want := []string{"base", "consumer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Subscribers() = %v, want %v", got, want)
	}
	got[0] = "changed"
	if got = registry.Subscribers("work", 1); !reflect.DeepEqual(got, []string{"base", "consumer"}) {
		t.Fatalf("Subscribers() exposed internal storage: %v", got)
	}
	if got = registry.Dependencies("consumer", "work"); !reflect.DeepEqual(got, []string{"base"}) {
		t.Fatalf("Dependencies() = %v, want [base]", got)
	}
	got[0] = "changed"
	if got = registry.Dependencies("consumer", "work"); !reflect.DeepEqual(got, []string{"base"}) {
		t.Fatalf("Dependencies() exposed internal storage: %v", got)
	}
}

func TestRegistryRejectsDuplicateDeclarations(t *testing.T) {
	tests := []struct {
		name    string
		events  []EventDefinition
		plugins []ControllerDefinition
	}{
		{name: "event version", events: append(definitions("work", 1), definitions("work", 1)...)},
		{name: "plugin", events: definitions("work", 1), plugins: []ControllerDefinition{{ID: "p"}, {ID: "p"}}},
		{name: "subscription", events: definitions("work", 1), plugins: []ControllerDefinition{{ID: "p", Subscriptions: []Subscription{{Event: "work", MinVersion: 1, MaxVersion: 1}, {Event: "work", MinVersion: 1, MaxVersion: 1}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewRegistry(tt.events, tt.plugins); err == nil {
				t.Fatal("duplicate declaration was accepted")
			}
		})
	}
}

func TestRegistryRejectsInvalidSubscriptionRange(t *testing.T) {
	_, err := NewRegistry(definitions("work", 1), []ControllerDefinition{{
		ID: "p", Subscriptions: []Subscription{{Event: "work", MinVersion: 2, MaxVersion: 1}},
	}})
	if err == nil {
		t.Fatal("inverted version range was accepted")
	}
}

func TestRegistryLookupsAndValidation(t *testing.T) {
	registry, err := NewRegistry(definitions("work", 1, 2), []ControllerDefinition{{
		ID: "consumer", Subscriptions: []Subscription{{Event: "work", MinVersion: 1, MaxVersion: 2}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Validate("work", 1, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("valid object rejected: %v", err)
	}
	if err := registry.Validate("missing", 1, json.RawMessage(`{}`)); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("unknown event error = %v, want ErrInvalidEvent", err)
	}
	if err := registry.Validate("work", 3, json.RawMessage(`{}`)); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("unknown version error = %v, want ErrInvalidEvent", err)
	}
	if registry.AcceptsRemote("work", 1) != true || registry.AcceptsRemote("work", 3) {
		t.Fatal("AcceptsRemote returned wrong result")
	}
	if !registry.Subscribes("consumer", "work", 2) || registry.Subscribes("consumer", "work", 3) {
		t.Fatal("Subscribes returned wrong result")
	}
	if got := registry.Subscribers("work", 3); len(got) != 0 {
		t.Fatalf("undeclared version has subscribers: %v", got)
	}
}

func TestObjectRequiresExactlyOneJSONObject(t *testing.T) {
	for _, payload := range []string{`null`, `[]`, `1`, `"text"`, ``, `{ } { }`, `{} trailing`} {
		if err := Object(json.RawMessage(payload)); err == nil {
			t.Errorf("Object(%q) unexpectedly succeeded", payload)
		}
	}
	if err := Object(json.RawMessage(`{"nested":[null]}`)); err != nil {
		t.Fatalf("valid object rejected: %v", err)
	}
}

func TestRetryingBuiltinAndUIDConflictSentinels(t *testing.T) {
	registry, err := NewRegistry(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if registry.AcceptsRemote("router.retrying", 1) || registry.AcceptsRemote("router.retrying", 2) {
		t.Fatal("built-in router.retrying declaration is incorrect")
	}
	if !errors.Is(ErrUIDConflict, ErrUIDConflict) {
		t.Fatal("ErrUIDConflict sentinel is unavailable")
	}
}

func TestContractTypesHaveSnakeCaseJSONTags(t *testing.T) {
	tests := []struct {
		value any
		keys  []string
	}{
		{Event{Seq: 1, UID: "u", ID: "e", Version: 1, Payload: json.RawMessage(`{}`), Source: "s", EmittedAt: 2}, []string{`"seq"`, `"uid"`, `"id"`, `"version"`, `"payload"`, `"source"`, `"emitted_at"`, `"correlation"`}},
		{EmitRequest{UID: "u", ID: "e", Version: 1, Payload: json.RawMessage(`{}`), Source: "s"}, []string{`"uid"`, `"id"`, `"version"`, `"payload"`, `"source"`, `"correlation"`}},
		{EmitResult{Stored: true}, []string{`"event"`, `"stored"`, `"replayed"`}},
		{Delivery{EventSeq: 1, Plugin: "p", State: "pending", Attempts: 0, NextAttemptAt: 1, UpdatedAt: 1}, []string{`"event_seq"`, `"plugin"`, `"state"`, `"attempts"`, `"next_attempt_at"`, `"last_error"`, `"updated_at"`}},
	}
	for _, tt := range tests {
		data, err := json.Marshal(tt.value)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range tt.keys {
			if !bytes.Contains(data, []byte(key+":")) {
				t.Errorf("%T JSON missing key %s in %s", tt.value, key, data)
			}
		}
	}
}
