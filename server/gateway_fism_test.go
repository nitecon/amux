package server

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func TestGatewayFISMReconcilesAcceptedObservationsAfterRestart(t *testing.T) {
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if store != nil {
			store.Close()
		}
	}()
	reportedAt := time.Now().Add(-time.Second)
	if accepted, err := store.ReportPresence(ctx, storage.PresenceReport{Source: "native:fixture", Sequence: 1}, reportedAt); err != nil || !accepted {
		t.Fatalf("record native heartbeat: accepted=%t err=%v", accepted, err)
	}
	reports, err := store.PendingObservations(ctx, 10)
	if err != nil || len(reports) != 1 {
		t.Fatalf("stored observations=%d err=%v", len(reports), err)
	}
	first, err := NewGatewayFISM(store, 7, 7)
	if err != nil {
		t.Fatal(err)
	}
	instance, session := uuid.NewString(), uuid.NewString()
	heartbeat, err := store.AcceptObservation(ctx, first.Registry(), fism.EmitRequest{
		ID: controllers.ClientHeartbeatID, Version: 1, Source: "client:fixture",
		Payload: []byte(fmt.Sprintf(`{"agents":[{"project":"remote","instance_id":%q,"session_id":%q,"provider":"codex","os":"linux"}]}`, instance, session)),
	})
	if err != nil || !heartbeat.Stored {
		t.Fatalf("accept remote heartbeat: %+v %v", heartbeat, err)
	}
	activity, err := store.AcceptObservation(ctx, first.Registry(), fism.EmitRequest{
		ID: controllers.ClientActivityID, Version: 1, Source: "client:fixture",
		Payload: []byte(fmt.Sprintf(`{"instance_id":%q,"session_id":%q,"draft":true}`, instance, session)),
	})
	if err != nil || !activity.Stored {
		t.Fatalf("accept remote activity: %+v %v", activity, err)
	}
	if err := store.InsertProject(ctx, storage.Project{Ident: "inbound", ChannelName: "discord", RoomID: "fixture", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	inbound, err := store.EmitRouterEvent(ctx, first.Registry(), fism.EmitRequest{
		ID: controllers.InboundMessageReceivedID, Version: 1, Source: "channel:discord",
		Payload: []byte(`{"channel_name":"discord","room_id":"fixture","message":{"id":"1","content":"saved","author_kind":"human"}}`),
	})
	if err != nil || !inbound.Stored {
		t.Fatalf("accept inbound message: %+v %v", inbound, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := NewGatewayFISM(store, 7, 7)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	reader.SetMaxOpenConns(1)
	if _, err := reader.ExecContext(ctx, "PRAGMA busy_timeout=3000"); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ExecContext(ctx, `CREATE TRIGGER reject_inbound BEFORE INSERT ON messages
  WHEN NEW.project_ident='inbound' BEGIN SELECT RAISE(ABORT,'inbound fixture unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- gateway.Run(ctx, ready) }()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Errorf("gateway shutdown: %v", err)
		}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("gateway did not start")
	}

	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var observedAt int64
		reports, err := store.PendingObservations(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if err := reader.QueryRowContext(ctx, "SELECT observed_at FROM amux_presence_clock WHERE id=1").Scan(&observedAt); err == nil && observedAt > reportedAt.UnixMilli() && len(reports) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("clock and heartbeat controllers did not settle")
		case <-tick.C:
			if err := store.EventBus().Publish(eventbus.Event{Topic: eventbus.Tick}); err != nil {
				t.Fatal(err)
			}
		}
	}
	var persisted int
	if err := reader.QueryRowContext(ctx, "SELECT COUNT(*) FROM router_events WHERE id='clock.tick' OR (id='client.heartbeat' AND source='native:fixture')").Scan(&persisted); err != nil || persisted != 0 {
		t.Fatalf("persisted reconciliation events=%d err=%v", persisted, err)
	}
	var seq int64
	var draft bool
	if err := reader.QueryRowContext(ctx, "SELECT activity_seq,draft FROM amux_presence_agents WHERE source='client:fixture'").Scan(&seq, &draft); err != nil || seq != activity.Event.Seq || !draft {
		t.Fatalf("remote activity was not applied after heartbeat: seq=%d draft=%t err=%v", seq, draft, err)
	}
	pending, err := store.PendingInboundMessages(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Event.UID != inbound.Event.UID {
		t.Fatalf("unavailable inbound write did not retain its input: %+v %v", pending, err)
	}
	if _, err := reader.ExecContext(ctx, "DROP TRIGGER reject_inbound"); err != nil {
		t.Fatal(err)
	}
	if err := store.EventBus().Publish(eventbus.Event{Topic: eventbus.ObservationAccepted}); err != nil {
		t.Fatal(err)
	}
	for {
		var head sql.NullString
		err := reader.QueryRowContext(ctx, "SELECT last_msg_id FROM projects WHERE ident='inbound'").Scan(&head)
		if err != nil {
			t.Fatal(err)
		}
		if head.Valid && head.String == "1" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("inbound did not settle after write recovery")
		case <-tick.C:
		}
	}
	for _, event := range []fism.Event{heartbeat.Event, activity.Event, inbound.Event} {
		deliveries, err := store.RouterDeliveries(ctx, event.Seq)
		if err != nil || len(deliveries) != 0 {
			t.Fatalf("remote observation created scheduler rows: %+v %v", deliveries, err)
		}
	}

}

func TestGatewayFISMStartsReconciliationFromSavedSnapshots(t *testing.T) {
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if store != nil {
			store.Close()
		}
	}()
	if err := store.InsertProject(ctx, storage.Project{Ident: "tasks", ChannelName: "tasks", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Saved intent", Reporter: "test"}, nil); err != nil {
		t.Fatal(err)
	}
	changes, err := store.PendingTaskChanges(ctx, 10)
	if err != nil || len(changes) != 1 {
		t.Fatalf("accepted task snapshots=%d err=%v", len(changes), err)
	}
	first, err := NewGatewayFISM(store, 7, 7)
	if err != nil {
		t.Fatal(err)
	}
	event, err := store.PublishTaskChange(ctx, first.Registry(), changes[0])
	if err != nil || event.Seq <= 0 {
		t.Fatalf("publish task snapshot: %+v %v", event, err)
	}
	outcomes, err := store.RouterDeliveries(ctx, event.Seq)
	if err != nil || len(outcomes) != 0 {
		t.Fatalf("publish wrote controller progress: %+v %v", outcomes, err)
	}
	if ok, err := store.ReportPresence(ctx, storage.PresenceReport{
		Source: "native:fixture", Sequence: 1,
		Agents: []storage.PresenceAgent{{
			ProjectIdent: "tasks",
			Origin:       agent.Origin{InstanceID: uuid.NewString(), SessionID: uuid.NewString(), Provider: "codex", OS: "linux"},
		}},
	}, time.Now()); err != nil || !ok {
		t.Fatalf("accept presence snapshot: %t %v", ok, err)
	}
	presenceChanges, err := store.PublishPresenceChanges(ctx, first.Registry(), 10)
	if err != nil || len(presenceChanges) == 0 {
		t.Fatalf("publish presence snapshots: %+v %v", presenceChanges, err)
	}
	for _, change := range presenceChanges {
		outcomes, err := store.RouterDeliveries(ctx, change.Event.Seq)
		if change.Event.Seq <= 0 || err != nil || len(outcomes) != 0 {
			t.Fatalf("presence publication wrote controller progress: %+v %+v %v", change, outcomes, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewGatewayFISM(store, 7, 7)
	if err != nil {
		t.Fatal(err)
	}
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() { done <- second.Run(ctx, ready) }()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Errorf("gateway shutdown: %v", err)
		}
	}()

	reader, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		left, err := store.PendingTaskChanges(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		var pendingPresence int
		if err := reader.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_presence_outbox").Scan(&pendingPresence); err != nil {
			t.Fatal(err)
		}
		if len(left) == 0 && pendingPresence == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("startup did not settle the saved task and presence snapshots")
		case <-tick.C:
		}
	}
	outcomes, err = store.RouterDeliveries(ctx, event.Seq)
	if err != nil || len(outcomes) != len(second.Registry().Subscribers(event.ID, event.Version)) {
		t.Fatalf("complete task outcomes=%+v err=%v", outcomes, err)
	}
	for _, outcome := range outcomes {
		if outcome.State != "completed" {
			t.Fatalf("controller persisted intermediate outcome: %+v", outcome)
		}
	}
	for _, change := range presenceChanges {
		outcomes, err := store.RouterDeliveries(ctx, change.Event.Seq)
		if err != nil || len(outcomes) != len(second.Registry().Subscribers(change.Event.ID, change.Event.Version)) {
			t.Fatalf("complete presence outcomes=%+v err=%v", outcomes, err)
		}
		for _, outcome := range outcomes {
			if outcome.State != "completed" {
				t.Fatalf("controller persisted intermediate presence outcome: %+v", outcome)
			}
		}
	}
	events, err := store.RouterEvents(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range events {
		for _, change := range presenceChanges {
			if current.ID == change.Event.ID && current.Seq != change.Event.Seq {
				t.Fatalf("startup duplicated presence broadcast: %+v", current)
			}
		}
		if current.ID == "task.created" && current.Seq != event.Seq {
			t.Fatalf("startup duplicated the task broadcast: %+v", current)
		}
	}
}

func TestNotificationSweepContinuesPastPendingRecipient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "notifications.db")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var firstUID string
	runtime, err := fism.NewRuntime(store, store.EventBus(), controllers.NotificationEventDefinitions(), []fism.Controller{{
		Definition: fism.ControllerDefinition{ID: "notification-fixture", Subscriptions: []fism.Subscription{
			{Event: controllers.NotifyEventID, MinVersion: 1, MaxVersion: 1},
		}},
		Exec: []fism.Step{func(_ context.Context, event fism.Event, _ *fism.StepContext) fism.Outcome {
			if event.UID == firstUID {
				return fism.Outcome{Kind: fism.Fail, Error: "recipient busy"}
			}
			return fism.Outcome{Kind: fism.Continue}
		}},
	}}, fism.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	for i := range 2 {
		result, err := store.PublishBroadcast(ctx, runtime.Registry(), fism.EmitRequest{
			ID: controllers.NotifyEventID, Version: 1, Source: "fixture",
			Payload: []byte(`{"target":"recipient","project":"fixture","mode":"recipient","require_idle":true,"event":{"kind":"task_created","task":{}}}`),
		})
		if err != nil || !result.Stored {
			t.Fatalf("notification fixture publication: %+v %v", result, err)
		}
		event := result.Event
		if i == 0 {
			firstUID = event.UID
		}
		if _, err := writer.ExecContext(ctx, "INSERT INTO amux_heartbeat_outbox(uid,source,payload,created_at) VALUES(?,?,?,?)",
			event.UID, event.Source, string(event.Payload), event.EmittedAt); err != nil {
			t.Fatal(err)
		}
	}
	gateway := GatewayFISM{store: store, runtime: runtime}
	if _, err := gateway.reconcileNotifications(ctx, runtime.Registry(), 1); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingNotifications(ctx, 0, 10)
	if err != nil || len(pending) != 1 || pending[0].Event.UID != firstUID {
		t.Fatalf("later notification did not settle: %+v %v", pending, err)
	}
}

func TestReconciliationRetriesOnlySettlement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "settlement.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	calls := 0
	runtime, err := fism.NewRuntime(store, store.EventBus(), []fism.EventDefinition{
		{ID: "fixture.changed", Version: 1, Validate: fism.Object},
	}, []fism.Controller{{
		Definition: fism.ControllerDefinition{ID: "fixture", Subscriptions: []fism.Subscription{
			{Event: "fixture.changed", MinVersion: 1, MaxVersion: 1},
		}},
		Exec: []fism.Step{func(context.Context, fism.Event, *fism.StepContext) fism.Outcome {
			calls++
			return fism.Outcome{Kind: fism.Continue}
		}},
	}}, fism.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	gateway := GatewayFISM{store: store, runtime: runtime}
	writes := 0
	event := fism.Event{UID: uuid.NewString(), ID: "fixture.changed", Version: 1, Payload: []byte("{}")}
	err = gateway.reconcileAndSettle(ctx, event, func(outcomes []fism.Delivery) error {
		if len(outcomes) != 1 || outcomes[0].State != "completed" {
			t.Fatalf("unexpected outcomes: %+v", outcomes)
		}
		writes++
		if writes == 1 {
			return fmt.Errorf("fixture settlement write failed")
		}
		return nil
	})
	if err != nil || calls != 1 || writes != 2 {
		t.Fatalf("controller calls=%d settlement writes=%d error=%v", calls, writes, err)
	}
}
