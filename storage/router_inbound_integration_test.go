package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
)

func TestInboundRuntimeFIFOAcrossProjectsSurvivesFailureAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "inbound-runtime.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, ident := range []string{"first-project", "second-project"} {
		if err := s.InsertProject(ctx, Project{Ident: ident, ChannelName: "fixture", RoomID: ident, Kind: "adhoc"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_first_inbound_cursor BEFORE UPDATE OF last_msg_id ON projects
		WHEN NEW.last_msg_id='first'
		BEGIN SELECT RAISE(ABORT,'fixture first cursor failed'); END`); err != nil {
		t.Fatal(err)
	}
	newRuntime := func(store *Store) *router.Runtime {
		t.Helper()
		runtime, err := router.NewRuntime(store, store.EventBus(), []router.EventDefinition{plugins.InboundDefinition()}, []router.Plugin{
			plugins.InboundPlugin(store),
			{Definition: router.PluginDefinition{ID: "independent", Subscriptions: []router.Subscription{{Event: "channel.message_received", MinVersion: 1, MaxVersion: 1}}}},
		}, router.RunConfig{Capacity: 4})
		if err != nil {
			t.Fatal(err)
		}
		return runtime
	}
	runtime := newRuntime(s)
	emit := func(store *Store, registry *router.Registry, room, id string) router.Event {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"channel_name": "fixture", "room_id": room,
			"message": map[string]string{"id": id, "content": "Input " + id, "author_kind": "human"}})
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.EmitRouterEvent(ctx, registry, router.EmitRequest{ID: "channel.message_received", Version: 1, Source: "channel:fixture", Payload: payload})
		if err != nil || !result.Stored {
			t.Fatalf("accept durable input: %+v %v", result, err)
		}
		return result.Event
	}
	first := emit(s, runtime.Registry(), "first-project", "first")
	second := emit(s, runtime.Registry(), "second-project", "second")
	stop := runRouterFixture(t, runtime)
	waitRouterDeliveryState(t, s, first.Seq, "inbound", "retrying")
	waitRouterDeliveryState(t, s, second.Seq, "independent", "completed")
	deliveries, err := s.RouterDeliveries(ctx, second.Seq)
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range deliveries {
		if delivery.Plugin == "inbound" && delivery.State != "pending" {
			t.Fatalf("FIFO failure exposed later project input: %+v", delivery)
		}
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM messages", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM inbound_cursors", int64(0))
	stop()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.db.Exec(`DROP TRIGGER fail_first_inbound_cursor;
		UPDATE router_deliveries SET next_attempt_at=0 WHERE plugin='inbound'`); err != nil {
		t.Fatal(err)
	}
	runtime = newRuntime(reopened)
	stop = runRouterFixture(t, runtime)
	defer stop()
	waitRouterDeliveryState(t, reopened, first.Seq, "inbound", "completed")
	waitRouterDeliveryState(t, reopened, second.Seq, "inbound", "completed")
	assertEqual(t, reopened.db, "SELECT COUNT(*) FROM messages", int64(2))
	assertEqual(t, reopened.db, "SELECT external_message_id FROM messages ORDER BY id LIMIT 1", "first")
	assertEqual(t, reopened.db, "SELECT last_msg_id FROM projects WHERE ident='first-project'", "first")
	assertEqual(t, reopened.db, "SELECT last_msg_id FROM projects WHERE ident='second-project'", "second")
	assertEqual(t, reopened.db, "SELECT event_seq FROM inbound_cursors WHERE project='first-project'", first.Seq)
	assertEqual(t, reopened.db, "SELECT event_seq FROM inbound_cursors WHERE project='second-project'", second.Seq)
	redelivery := emit(reopened, runtime.Registry(), "first-project", "first")
	waitRouterDeliveryState(t, reopened, redelivery.Seq, "inbound", "completed")
	assertEqual(t, reopened.db, "SELECT COUNT(*) FROM messages", int64(2))
	assertEqual(t, reopened.db, "SELECT event_seq FROM inbound_cursors WHERE project='first-project'", first.Seq)
	assertEqual(t, reopened.db, "SELECT COUNT(*) FROM messages WHERE source='user' AND author_kind='human' AND deliver_to_agents=0 AND confirmed_at IS NULL", int64(2))
	assertEqual(t, reopened.db, "SELECT COUNT(*) FROM pragma_foreign_key_check", int64(0))
}
