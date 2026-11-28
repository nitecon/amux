package storage

import (
	"context"
	"testing"

	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
)

func TestRecoverLegacyInputsPreservesPublicationsAndTerminalOutcomes(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	defs := []fism.EventDefinition{controllers.InboundDefinition(), controllers.ClockDefinition()}
	registry, err := fism.NewRegistry(defs, []fism.ControllerDefinition{
		{ID: "old", Subscriptions: []fism.Subscription{{Event: controllers.InboundMessageReceivedID, MinVersion: 1, MaxVersion: 1}, {Event: "clock.tick", MinVersion: 1, MaxVersion: 1}}},
		{ID: "finished", Subscriptions: []fism.Subscription{{Event: controllers.InboundMessageReceivedID, MinVersion: 1, MaxVersion: 1}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := s.EmitRouterEvent(ctx, registry, fism.EmitRequest{ID: controllers.InboundMessageReceivedID, Version: 1, Source: "channel:discord",
		Payload: []byte(`{"channel_name":"discord","room_id":"fixture","message":{"id":"1","content":"saved","author_kind":"human"}}`)})
	if err != nil || !message.Stored {
		t.Fatalf("legacy inbound acceptance: %+v %v", message, err)
	}
	tick, err := s.EmitRouterEvent(ctx, registry, fism.EmitRequest{ID: "clock.tick", Version: 1, Source: "clock", Payload: []byte("{}")})
	if err != nil || !tick.Stored {
		t.Fatalf("legacy clock acceptance: %+v %v", tick, err)
	}
	if _, err := s.db.Exec("UPDATE router_deliveries SET state=CASE plugin WHEN 'finished' THEN 'completed' ELSE 'running' END"); err != nil {
		t.Fatal(err)
	}
	current, err := fism.NewRegistry(defs, []fism.ControllerDefinition{
		{ID: "new", Subscriptions: []fism.Subscription{{Event: controllers.InboundMessageReceivedID, MinVersion: 1, MaxVersion: 1}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_cutover BEFORE INSERT ON amux_heartbeat_outbox
		BEGIN SELECT RAISE(ABORT,'fixture recovery write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverLegacyInputs(ctx, current); err == nil {
		t.Fatal("expected recovery failure")
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_deliveries", int64(3))
	if _, err := s.db.Exec("DROP TRIGGER reject_cutover"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.RecoverLegacyInputs(ctx, current); err != nil {
			t.Fatal(err)
		}
	}
	accepted, err := s.PendingInboundMessages(ctx, 10)
	if err != nil || len(accepted) != 1 || accepted[0].Event.UID != message.Event.UID || accepted[0].Event.Seq != message.Event.Seq {
		t.Fatalf("recovered inputs=%+v err=%v", accepted, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_heartbeat_outbox", int64(1))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events", int64(2))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_deliveries WHERE state='completed' AND plugin='finished'", int64(1))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_deliveries", int64(1))
}
