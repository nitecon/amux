package controllers

import (
	"bytes"
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/nitecon/amux/fism"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type retentionStoreFunc func(context.Context, fism.Event, uint64, uint64) (bool, int64, error)

func (f retentionStoreFunc) ReconcileRouterRetention(ctx context.Context, event fism.Event, retentionDays, botRetentionDays uint64) (bool, int64, error) {
	return f(ctx, event, retentionDays, botRetentionDays)
}

func TestRetentionStartupDefinitionAndPluginMetadata(t *testing.T) {
	definition := RetentionStartupDefinition()
	if RetentionStartupID != "retention.startup" {
		t.Fatalf("startup ID=%q, want retention.startup", RetentionStartupID)
	}
	if definition.ID != RetentionStartupID || definition.Version != 1 || definition.RemoteEmit {
		t.Fatalf("unexpected retention startup definition: %+v", definition)
	}
	if err := definition.Validate([]byte(`{"startup":true}`)); err != nil {
		t.Fatalf("object payload rejected: %v", err)
	}
	if err := definition.Validate([]byte(`[]`)); err == nil {
		t.Fatal("non-object payload accepted")
	}

	plugin := RetentionController(nil, 1, 2)
	if plugin.Definition.ID != retentionPluginID || plugin.Definition.FIFO || plugin.Definition.MaxAttempts != 0 {
		t.Fatalf("unexpected retention policy: %+v", plugin.Definition)
	}
	want := []fism.Subscription{{Event: RetentionStartupID, MinVersion: 1, MaxVersion: 1}, {Event: "clock.tick", MinVersion: 1, MaxVersion: 1}}
	if !reflect.DeepEqual(plugin.Definition.Subscriptions, want) {
		t.Fatalf("subscriptions=%+v, want %+v", plugin.Definition.Subscriptions, want)
	}
	if len(plugin.PreExec) != 0 || len(plugin.Exec) != 1 {
		t.Fatalf("retention should execute without pre-exec gates: pre=%d exec=%d", len(plugin.PreExec), len(plugin.Exec))
	}
	registry, err := fism.NewRegistry([]fism.EventDefinition{definition, ClockDefinition()}, []fism.ControllerDefinition{plugin.Definition})
	if err != nil {
		t.Fatalf("retention metadata is not registrable: %v", err)
	}
	if registry.FIFO(retentionPluginID) || registry.MaxAttempts(retentionPluginID) != 0 {
		t.Fatalf("retention policy is switchable: FIFO=%t maxAttempts=%d", registry.FIFO(retentionPluginID), registry.MaxAttempts(retentionPluginID))
	}
}

func TestRetentionPluginForwardsEventConfigAndLogsOnlyDeletedCount(t *testing.T) {
	cause := fism.Event{Seq: 17, UID: "retention-cause", ID: "clock.tick", Version: 1, Payload: []byte(`{"secret":"do-not-log"}`), Source: "runtime", EmittedAt: 1234}
	var gotContext context.Context
	var gotEvent fism.Event
	var gotRetentionDays, gotBotRetentionDays uint64
	var logs bytes.Buffer
	originalLogger := log.Logger
	log.Logger = zerolog.New(&logs)
	t.Cleanup(func() { log.Logger = originalLogger })
	plugin := RetentionController(retentionStoreFunc(func(ctx context.Context, event fism.Event, retentionDays, botRetentionDays uint64) (bool, int64, error) {
		gotContext, gotEvent = ctx, event
		gotRetentionDays, gotBotRetentionDays = retentionDays, botRetentionDays
		return true, 9, nil
	}), math.MaxUint64, math.MaxUint64-1)
	ctx := context.WithValue(context.Background(), struct{}{}, "forwarded")
	outcome := plugin.Exec[0](ctx, cause, nil)
	if outcome.Kind != fism.Continue {
		t.Fatalf("outcome=%+v, want Continue", outcome)
	}
	if gotContext != ctx || !reflect.DeepEqual(gotEvent, cause) || gotRetentionDays != math.MaxUint64 || gotBotRetentionDays != math.MaxUint64-1 {
		t.Fatalf("forwarded context/event/config mismatch: context=%v event=%+v days=%d botDays=%d", gotContext == ctx, gotEvent, gotRetentionDays, gotBotRetentionDays)
	}
	if output := logs.String(); !strings.Contains(output, `"count":9`) || strings.Contains(output, "do-not-log") || strings.Contains(output, cause.UID) {
		t.Fatalf("retention log should contain only deletion count, got %q", output)
	}

	logs.Reset()
	plugin = RetentionController(retentionStoreFunc(func(context.Context, fism.Event, uint64, uint64) (bool, int64, error) {
		return false, 99, nil
	}), 0, 0)
	if outcome := plugin.Exec[0](ctx, cause, nil); outcome.Kind != fism.Continue || logs.Len() != 0 {
		t.Fatalf("skipped retention should continue without logging: outcome=%+v logs=%q", outcome, logs.String())
	}
}

func TestRetentionPluginNilStoreErrorAndCanceledContext(t *testing.T) {
	cause := fism.Event{ID: RetentionStartupID, Version: 1}
	if outcome := RetentionController(nil, 1, 1).Exec[0](context.Background(), cause, nil); outcome.Kind != fism.Fail || outcome.Error != "retention plugin store is nil" {
		t.Fatalf("nil store outcome=%+v", outcome)
	}

	wantErr := errors.New("retention failed")
	plugin := RetentionController(retentionStoreFunc(func(context.Context, fism.Event, uint64, uint64) (bool, int64, error) {
		return false, 0, wantErr
	}), 1, 1)
	if outcome := plugin.Exec[0](context.Background(), cause, nil); outcome.Kind != fism.Fail || outcome.Error != wantErr.Error() {
		t.Fatalf("store error outcome=%+v", outcome)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plugin = RetentionController(retentionStoreFunc(func(ctx context.Context, _ fism.Event, _, _ uint64) (bool, int64, error) {
		if !errors.Is(ctx.Err(), context.Canceled) {
			return false, 0, errors.New("canceled context was not forwarded")
		}
		return false, 0, ctx.Err()
	}), 1, 1)
	if outcome := plugin.Exec[0](ctx, cause, nil); outcome.Kind != fism.Fail || outcome.Error != context.Canceled.Error() {
		t.Fatalf("canceled context outcome=%+v", outcome)
	}
}
