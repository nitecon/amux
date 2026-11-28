package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/fism"
)

func routerTestRegistry(t *testing.T, plugins ...string) *fism.Registry {
	t.Helper()
	definitions := make([]fism.ControllerDefinition, 0, len(plugins))
	for _, id := range plugins {
		definitions = append(definitions, fism.ControllerDefinition{ID: id, Subscriptions: []fism.Subscription{{Event: "test.event", MinVersion: 1, MaxVersion: 1}}})
	}
	r, err := fism.NewRegistry([]fism.EventDefinition{{ID: "test.event", Version: 1, RemoteEmit: true, Validate: func(payload json.RawMessage) error {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(payload, &value); err != nil {
			return err
		}
		if value == nil {
			return errors.New("payload must be an object")
		}
		return nil
	}}}, definitions)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func routerTestRequest(payload string) fism.EmitRequest {
	return fism.EmitRequest{ID: "test.event", Version: 1, Payload: json.RawMessage(payload), Source: "test"}
}

func TestRouterEmitPersistsEventAndDeliveries(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	result, err := s.EmitRouterEvent(ctx, routerTestRegistry(t, "alpha", "beta"), routerTestRequest(`{"n":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stored || result.Replayed || result.Event.Seq <= 0 || result.Event.UID == "" || result.Event.EmittedAt <= 0 {
		t.Fatalf("unexpected emit result: %+v", result)
	}
	deliveries, err := s.RouterDeliveries(ctx, result.Event.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 2 || deliveries[0].State != "pending" || deliveries[1].State != "pending" {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	got, err := s.RouterEvents(ctx, 0, 100)
	if err != nil || len(got) != 1 || got[0].Seq != result.Event.Seq {
		t.Fatalf("events = %+v, err = %v", got, err)
	}
	got[0].Payload[0] = 'x'
	again, err := s.RouterEvents(ctx, 0, 100)
	if err != nil || string(again[0].Payload) != `{"n":1}` {
		t.Fatalf("payload is not an independent copy: %q, %v", again[0].Payload, err)
	}
}

func TestRouterEmitNoSubscribersValidatesButStoresNothing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	result, err := s.EmitRouterEvent(ctx, routerTestRegistry(t), routerTestRequest(`{"n":1}`))
	if err != nil || result.Stored || result.Replayed {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	events, err := s.RouterEvents(ctx, 0, 100)
	if err != nil || len(events) != 0 {
		t.Fatalf("events = %+v, err = %v", events, err)
	}
	if _, err := s.EmitRouterEvent(ctx, routerTestRegistry(t), routerTestRequest(`not-json`)); err == nil {
		t.Fatal("invalid event was accepted because there were no subscribers")
	}
}

func TestRouterUIDReplayUsesSemanticJSONAndStoredEvent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	uid := uuid.Must(uuid.NewV7()).String()
	request := routerTestRequest(`{"a":1,"b":9007199254740993}`)
	request.UID = uid
	first, err := s.EmitRouterEvent(ctx, routerTestRegistry(t, "alpha"), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Payload = json.RawMessage(" { \"b\" : 9007199254740993, \"a\": 1 } ")
	replayed, err := s.EmitRouterEvent(ctx, routerTestRegistry(t, "alpha"), request)
	if err != nil || !replayed.Replayed || replayed.Stored || replayed.Event.Seq != first.Event.Seq || replayed.Event.Source != "test" {
		t.Fatalf("replay = %+v, err = %v", replayed, err)
	}
	request.Payload = json.RawMessage(`{"a":1,"b":9007199254740992}`)
	if _, err := s.EmitRouterEvent(ctx, routerTestRegistry(t, "alpha"), request); !errors.Is(err, fism.ErrUIDConflict) {
		t.Fatalf("large integer conflict error = %v", err)
	}
	request.Payload = json.RawMessage(`{"a":1,"b":9007199254740993}`)
	request.Source = "different-source"
	if _, err := s.EmitRouterEvent(ctx, routerTestRegistry(t, "alpha"), request); !errors.Is(err, fism.ErrUIDConflict) {
		t.Fatalf("source conflict error = %v", err)
	}
	request.Source = "test"
	correlation := uuid.Must(uuid.NewV7()).String()
	request.Correlation = &correlation
	if _, err := s.EmitRouterEvent(ctx, routerTestRegistry(t, "alpha"), request); !errors.Is(err, fism.ErrUIDConflict) {
		t.Fatalf("correlation conflict error = %v", err)
	}
	request.Correlation = nil
	registry, err := fism.NewRegistry(
		[]fism.EventDefinition{
			{ID: "test.event", Version: 1, Validate: fism.Object},
			{ID: "test.event", Version: 2, Validate: fism.Object},
			{ID: "other.event", Version: 1, Validate: fism.Object},
		},
		[]fism.ControllerDefinition{{ID: "alpha", Subscriptions: []fism.Subscription{
			{Event: "test.event", MinVersion: 1, MaxVersion: 2},
			{Event: "other.event", MinVersion: 1, MaxVersion: 1},
		}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	request.ID = "other.event"
	if _, err := s.EmitRouterEvent(ctx, registry, request); !errors.Is(err, fism.ErrUIDConflict) {
		t.Fatalf("event ID conflict error = %v", err)
	}
	request.ID = "test.event"
	request.Version = 2
	if _, err := s.EmitRouterEvent(ctx, registry, request); !errors.Is(err, fism.ErrUIDConflict) {
		t.Fatalf("event version conflict error = %v", err)
	}
}

func TestRouterUIDReplayPrecedesNoSubscriberSkip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	request := routerTestRequest(`{"n":1}`)
	request.UID = uuid.Must(uuid.NewV7()).String()
	first, err := s.EmitRouterEvent(ctx, routerTestRegistry(t, "alpha"), request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.EmitRouterEvent(ctx, routerTestRegistry(t), request)
	if err != nil || !replay.Replayed || replay.Event.Seq != first.Event.Seq {
		t.Fatalf("replay = %+v, err = %v", replay, err)
	}
	request.Payload = json.RawMessage(`{"n":2}`)
	if _, err := s.EmitRouterEvent(ctx, routerTestRegistry(t), request); !errors.Is(err, fism.ErrUIDConflict) {
		t.Fatalf("conflict with no subscribers = %v", err)
	}
}

func TestRouterUIDReplaySurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reopen.db")
	firstStore, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	request := routerTestRequest(`{"n":1}`)
	request.UID = uuid.Must(uuid.NewV7()).String()
	first, err := firstStore.EmitRouterEvent(ctx, routerTestRegistry(t, "alpha"), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replay, err := reopened.EmitRouterEvent(ctx, routerTestRegistry(t, "alpha"), request)
	if err != nil || !replay.Replayed || replay.Event.Seq != first.Event.Seq || replay.Event.EmittedAt != first.Event.EmittedAt {
		t.Fatalf("replay after reopen = %+v, err = %v", replay, err)
	}
}

func TestRouterEmitRejectsMalformedRequestBoundaries(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	registry := routerTestRegistry(t, "alpha")
	for _, tc := range []struct {
		name string
		edit func(*fism.EmitRequest)
	}{
		{"malformed json", func(r *fism.EmitRequest) { r.Payload = json.RawMessage(`{"x":`) }},
		{"trailing json", func(r *fism.EmitRequest) { r.Payload = json.RawMessage(`{} {}`) }},
		{"empty source", func(r *fism.EmitRequest) { r.Source = "" }},
		{"whitespace source", func(r *fism.EmitRequest) { r.Source = "  " }},
		{"malformed uid", func(r *fism.EmitRequest) { r.UID = "not-a-uuid" }},
		{"malformed correlation", func(r *fism.EmitRequest) { v := "not-a-uuid"; r.Correlation = &v }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := routerTestRequest(`{"n":1}`)
			tc.edit(&req)
			if _, err := s.EmitRouterEvent(ctx, registry, req); err == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
}

func TestRouterEmitSavepointRollsBackEventAndDeliveries(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_test_delivery BEFORE INSERT ON router_deliveries WHEN NEW.plugin='z-fail' BEGIN SELECT RAISE(ABORT,'injected delivery failure'); END`); err != nil {
		t.Fatal(err)
	}
	request := routerTestRequest(`{"n":1}`)
	request.UID = uuid.Must(uuid.NewV7()).String()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('unrelated','committed')"); err != nil {
		t.Fatal(err)
	}
	if _, err := emitRouterEventIn(ctx, tx, routerTestRegistry(t, "a-ok", "z-fail"), request); err == nil {
		t.Fatal("injected delivery failure was ignored")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("caller transaction did not commit unrelated work: %v", err)
	}
	var setting string
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='unrelated'").Scan(&setting); err != nil || setting != "committed" {
		t.Fatalf("unrelated caller write = %q, %v", setting, err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM router_events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("event count = %d, %v", count, err)
	}
}

func TestRouterEmitUIDConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")
	s1, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	request := routerTestRequest(`{"n":1}`)
	request.UID = uuid.Must(uuid.NewV7()).String()
	registry := routerTestRegistry(t, "alpha")
	stores := []*Store{s1, s2}
	results := make([]fism.EmitResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = stores[i].EmitRouterEvent(ctx, registry, request)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	if results[0].Event.Seq != results[1].Event.Seq || results[0].Replayed == results[1].Replayed {
		t.Fatalf("concurrent results = %+v", results)
	}
}

func TestRouterMigrationFailsClosedOnLegacyFailedCheck(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `CREATE TABLE router_events(seq INTEGER PRIMARY KEY AUTOINCREMENT,uid TEXT NOT NULL UNIQUE,id TEXT NOT NULL,version INTEGER NOT NULL,payload TEXT NOT NULL,source TEXT NOT NULL,emitted_at INTEGER NOT NULL,correlation TEXT);
CREATE TABLE router_deliveries(event_seq INTEGER NOT NULL REFERENCES router_events(seq) ON DELETE CASCADE,plugin TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('pending','running','retrying','completed','stopped','skipped')),attempts INTEGER NOT NULL DEFAULT 0,next_attempt_at INTEGER NOT NULL,last_error TEXT,updated_at INTEGER NOT NULL,PRIMARY KEY(event_seq,plugin));
INSERT INTO router_events(uid,id,version,payload,source,emitted_at) VALUES('kept','test.event',1,'{}','test',1);
INSERT INTO router_deliveries(event_seq,plugin,state,next_attempt_at,updated_at) VALUES(1,'kept','pending',1,1);`)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = migrateRouter(ctx, tx)
	_ = tx.Rollback()
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "failed") {
		t.Fatalf("migration error = %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM router_deliveries").Scan(&count); err != nil || count != 1 {
		t.Fatalf("legacy delivery rows = %d, %v", count, err)
	}
}

func TestRouterEventsPaginationBounds(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	registry := routerTestRegistry(t, "alpha")
	for i := 0; i < 3; i++ {
		if _, err := s.EmitRouterEvent(ctx, registry, routerTestRequest(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	for _, limit := range []int{0, 201} {
		if _, err := s.RouterEvents(ctx, 0, limit); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
	if _, err := s.RouterEvents(ctx, -1, 10); err == nil {
		t.Fatal("accepted negative cursor")
	}
	page, err := s.RouterEvents(ctx, 1, 1)
	if err != nil || len(page) != 1 || page[0].Seq != 2 {
		t.Fatalf("page = %+v, %v", page, err)
	}
}
