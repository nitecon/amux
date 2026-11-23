package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

func channelRouterRegistry(t *testing.T) *router.Registry {
	t.Helper()
	r, err := router.NewRegistry([]router.EventDefinition{{ID: "test.changed", Version: 1, Validate: router.Object}}, []router.PluginDefinition{{ID: "fixture", Subscriptions: []router.Subscription{{Event: "test.changed", MinVersion: 1, MaxVersion: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRouterChannelsAnnounceCommittedJournal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := channelRouterRegistry(t)
	one, two := s.EventBus().Subscribe(4), s.EventBus().Subscribe(4)
	defer one.Unsubscribe()
	defer two.Unsubscribe()
	result, err := s.EmitRouterEvent(ctx, r, router.EmitRequest{ID: "test.changed", Version: 1, Source: "server:test", Payload: []byte(`{"value":1}`)})
	if err != nil || !result.Stored {
		t.Fatalf("emit %+v: %v", result, err)
	}
	for _, sub := range []*eventbus.Subscription{one, two} {
		select {
		case e := <-sub.C:
			if e.Topic != eventbus.RouterChanged {
				t.Fatalf("topic %s", e.Topic)
			}
		default:
			t.Fatal("independent subscriber missed commit")
		}
		rows, err := s.RouterEvents(ctx, 0, 100)
		if err != nil || len(rows) != 1 || rows[0].UID != result.Event.UID {
			t.Fatalf("announced before journal readable: %+v %v", rows, err)
		}
	}
}

func TestRouterEmitSavepointKeepsCallerTransactionAtomic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := channelRouterRegistry(t)
	sub := s.EventBus().Subscribe(4)
	defer sub.Unsubscribe()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('router-test','caller'); CREATE TEMP TRIGGER reject_router_delivery BEFORE INSERT ON router_deliveries BEGIN SELECT RAISE(ABORT,'fixture rejection'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := emitRouterEventIn(ctx, tx, r, router.EmitRequest{ID: "test.changed", Version: 1, Source: "server:test", Payload: []byte(`{}`)}); err == nil {
		t.Fatal("accepted rejected delivery")
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM router_events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial event persisted: %d %v", count, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := s.db.QueryRow("SELECT value FROM settings WHERE key='router-test'").Scan(&value); err != nil || value != "caller" {
		t.Fatalf("caller transaction lost: %q %v", value, err)
	}
	select {
	case <-sub.C:
		t.Fatal("failed event announced")
	default:
	}
}

func TestOpenUpgradesLegacyRouterWithinFullRustSchema(t *testing.T) {
	ctx := context.Background()
	schema, err := os.ReadFile("testdata/task_status_legacy.sql")
	if err != nil {
		t.Fatal(err)
	}
	if matches := currentRouterDeliveryStatePattern.FindAllIndex(schema, -1); len(matches) != 1 {
		t.Fatalf("delivery constraint matches %d", len(matches))
	}
	legacy := currentRouterDeliveryStatePattern.ReplaceAllString(string(schema), oldRouterDeliveryStateCheck)
	path := filepath.Join(t.TempDir(), "legacy-router.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, legacy); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO router_events(seq,uid,id,version,payload,source,emitted_at) VALUES(42,'11111111-1111-4111-8111-111111111111','test.event',1,'{}','test',1);
 INSERT INTO router_deliveries(event_seq,plugin,state,attempts,next_attempt_at,last_error,updated_at) VALUES(42,'fixture','retrying',2,2000,'offline',1000);`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	deliveries, err := s.RouterDeliveries(ctx, 42)
	if err != nil || len(deliveries) != 1 || deliveries[0].Attempts != 2 || deliveries[0].State != "retrying" || deliveries[0].LastError == nil || *deliveries[0].LastError != "offline" {
		t.Fatalf("progress lost: %+v %v", deliveries, err)
	}
	if _, err := s.db.Exec("UPDATE router_deliveries SET state='failed' WHERE event_seq=42"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DELETE FROM router_events WHERE seq=42"); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_deliveries WHERE event_seq=42", int64(0))
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".task-status-backup-*", "database.sqlite"))
	if len(backups) != 1 {
		t.Fatalf("expected backup before rebuild: %v", backups)
	}
}

func TestRouterSkippedEmitsStillValidateDeclarationAndSchema(t *testing.T) {
	s := testStore(t)
	r, err := router.NewRegistry([]router.EventDefinition{{ID: "test.changed", Version: 1, Validate: router.Object}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []router.EmitRequest{
		{ID: "undeclared.event", Version: 1, Source: "test", Payload: []byte(`{}`)},
		{ID: "test.changed", Version: 1, Source: "test", Payload: []byte(`[]`)},
	} {
		if _, err := s.EmitRouterEvent(context.Background(), r, request); err == nil {
			t.Fatalf("unsubscribed invalid event accepted: %+v", request)
		}
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events", int64(0))
}

func TestRouterEmitInRollsBackWithCaller(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	result, err := emitRouterEventIn(ctx, tx, channelRouterRegistry(t), router.EmitRequest{ID: "test.changed", Version: 1, Source: "test", Payload: []byte(`{}`)})
	if err != nil || !result.Stored {
		t.Fatalf("emit %+v: %v", result, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_deliveries", int64(0))
}
