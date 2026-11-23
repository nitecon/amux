package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/router"
)

func TestRouterHeartbeatRejectsSourcesThatCannotEnterJournal(t *testing.T) {
	s := testStore(t)
	for _, source := range []string{" worker", "worker ", "\tworker"} {
		accepted, err := s.ReportPresence(context.Background(), PresenceReport{Source: source, Sequence: 1}, time.Now())
		if accepted || !errors.Is(err, ErrPresenceInput) {
			t.Fatalf("accepted undrainable source %q: %v %v", source, accepted, err)
		}
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_sources", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_heartbeat_outbox", int64(0))
}

func TestRouterHeartbeatUpgradeRetainsSourceCursorAndLegacyReminderReceipts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "heartbeat-v18.db")
	s := presenceStore(t, path)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	at := time.Now().UTC()
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "retained-source", Sequence: 7, Agents: []PresenceAgent{agent}}, at); err != nil || !ok {
		t.Fatalf("seed retained report: %v %v", ok, err)
	}
	// Recreate the pre-heartbeat native schema while retaining the Rust reminder
	// tables and their receipts, as an existing profile can contain both.
	if _, err := s.db.ExecContext(ctx, `DROP TABLE amux_heartbeat_outbox;
		ALTER TABLE amux_presence_sources DROP COLUMN heartbeat_uid;
		ALTER TABLE amux_presence_sources DROP COLUMN heartbeat_seen_at;
		DELETE FROM amux_go_migrations WHERE version>=19;
		INSERT INTO reminder_nudges(task_id,sent_at) VALUES('retained-task',1234);
		INSERT INTO reminder_restarts(source,session_id,instance_id,client_run) VALUES('legacy','session','instance','retained-run')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	assertEqual(t, upgraded.db, "SELECT sequence FROM amux_presence_sources WHERE source='retained-source'", int64(7))
	assertEqual(t, upgraded.db, "SELECT COUNT(*) FROM amux_presence_agents", int64(1))
	assertEqual(t, upgraded.db, "SELECT sent_at FROM reminder_nudges WHERE task_id='retained-task'", int64(1234))
	var run string
	if err := upgraded.db.QueryRowContext(ctx, "SELECT client_run FROM reminder_restarts WHERE source='legacy'").Scan(&run); err != nil || run != "retained-run" {
		t.Fatalf("retained reminder receipt %q: %v", run, err)
	}
	if ok, err := upgraded.ReportPresence(ctx, PresenceReport{Source: "retained-source", Sequence: 7, Agents: []PresenceAgent{agent}}, at); err != nil || ok {
		t.Fatalf("upgrade lost source replay fence: %v %v", ok, err)
	}
	assertEqual(t, upgraded.db, "SELECT COUNT(*) FROM amux_heartbeat_outbox", int64(0))
	if ok, err := upgraded.ReportPresence(ctx, PresenceReport{Source: "retained-source", Sequence: 8, Agents: []PresenceAgent{agent}}, at.Add(time.Millisecond)); err != nil || !ok {
		t.Fatalf("new report after upgrade: %v %v", ok, err)
	}
	assertEqual(t, upgraded.db, "SELECT COUNT(*) FROM amux_heartbeat_outbox", int64(1))
}

func heartbeatRegistry(t *testing.T) *router.Registry {
	t.Helper()
	registry, err := router.NewRegistry(
		[]router.EventDefinition{{ID: "client.heartbeat", Version: 1, Validate: func(payload json.RawMessage) error {
			if !json.Valid(payload) {
				return router.ErrInvalidEvent
			}
			return nil
		}}},
		[]router.PluginDefinition{{ID: "heartbeat-test", Subscriptions: []router.Subscription{{Event: "client.heartbeat", MinVersion: 1, MaxVersion: 1}}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestRouterHeartbeatMigrationCaptureAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "heartbeat.db")
	s := presenceStore(t, path)
	reportAt := time.UnixMilli(1_800_700_000_000)
	run := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	agent := presenceAgent("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	activity := reportAt.UnixMilli() - 123
	agent.LastActivityAt = &activity
	agent.AutoAssign = true
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "native:one", Sequence: 9, ClientRun: &run, Agents: []PresenceAgent{agent}}, reportAt); err != nil || !ok {
		t.Fatalf("report accepted=%v err=%v", ok, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_go_migrations WHERE version=19", int64(1))
	var sourceUID string
	var seen, sequence int64
	if err := s.db.QueryRowContext(ctx, "SELECT heartbeat_uid,heartbeat_seen_at,sequence FROM amux_presence_sources WHERE source='native:one'").Scan(&sourceUID, &seen, &sequence); err != nil {
		t.Fatal(err)
	}
	if !canonicalUUID(sourceUID) || seen != reportAt.UnixMilli() || sequence != 9 {
		t.Fatalf("source heartbeat uid=%q seen=%d sequence=%d", sourceUID, seen, sequence)
	}
	var payload []byte
	if err := s.db.QueryRowContext(ctx, "SELECT payload FROM amux_heartbeat_outbox WHERE uid=?", sourceUID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		ClientRun      string `json:"client_run"`
		ReportSequence int64  `json:"report_sequence"`
		ReportedAt     int64  `json:"reported_at"`
		Agents         []struct {
			Project        string `json:"project"`
			SessionID      string `json:"session_id"`
			InstanceID     string `json:"instance_id"`
			Provider       string `json:"provider"`
			OS             string `json:"os"`
			LastActivityAt int64  `json:"last_activity_at"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.ClientRun != run || envelope.ReportSequence != 9 || envelope.ReportedAt != seen || len(envelope.Agents) != 1 {
		t.Fatalf("heartbeat envelope=%s", payload)
	}
	flat := envelope.Agents[0]
	if flat.Project != "presence" || flat.SessionID != agent.Origin.SessionID || flat.InstanceID != agent.Origin.InstanceID || flat.Provider != "codex" || flat.OS != "linux" || flat.LastActivityAt != activity {
		t.Fatalf("flattened agent=%+v", flat)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = presenceStore(t, path)
	if got := queryHeartbeatOutboxCount(t, s); got != 1 {
		t.Fatalf("reopened outbox rows=%d", got)
	}
}

func TestRouterHeartbeatOldClientReplayAndCaptureRollback(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "heartbeat-rollback.db"))
	start := time.UnixMilli(1_800_710_000_000)
	agent := presenceAgent("dddddddd-dddd-4ddd-8ddd-dddddddddddd", "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee")
	if _, err := s.db.Exec(`CREATE TRIGGER reject_heartbeat BEFORE INSERT ON amux_heartbeat_outbox BEGIN SELECT RAISE(ABORT,'fixture heartbeat failure'); END`); err != nil {
		t.Fatal(err)
	}
	report := PresenceReport{Source: "old-client", Sequence: 1, Agents: []PresenceAgent{agent}}
	if accepted, err := s.ReportPresence(ctx, report, start); err == nil || accepted {
		t.Fatalf("capture failure accepted=%v err=%v", accepted, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_sources", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_agents", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_outbox", int64(0))
	if _, err := s.db.Exec("DROP TRIGGER reject_heartbeat"); err != nil {
		t.Fatal(err)
	}
	if accepted, err := s.ReportPresence(ctx, report, start); err != nil || !accepted {
		t.Fatalf("old client accepted=%v err=%v", accepted, err)
	}
	if accepted, err := s.ReportPresence(ctx, report, start); err != nil || accepted {
		t.Fatalf("replay accepted=%v err=%v", accepted, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_heartbeat_outbox", int64(1))
	var clientRun sql.NullString
	if err := s.db.QueryRowContext(ctx, "SELECT json_extract(payload,'$.client_run') FROM amux_heartbeat_outbox").Scan(&clientRun); err != nil {
		t.Fatal(err)
	}
	if clientRun.Valid {
		t.Fatalf("legacy client got client_run=%q", clientRun.String)
	}
	badRun := "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "invalid", Sequence: 1, ClientRun: &badRun}, start); err == nil {
		t.Fatal("noncanonical client_run accepted")
	}
}

func TestDrainRouterHeartbeatOutboxRollbackAndStableUID(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "heartbeat-drain.db"))
	registry := heartbeatRegistry(t)
	start := time.UnixMilli(1_800_720_000_000)
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:drain", Sequence: 1}, start); err != nil || !ok {
		t.Fatalf("report accepted=%v err=%v", ok, err)
	}
	var expectedUID string
	if err := s.db.QueryRowContext(ctx, "SELECT heartbeat_uid FROM amux_presence_sources WHERE source='worker:drain'").Scan(&expectedUID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_heartbeat_ack BEFORE DELETE ON amux_heartbeat_outbox BEGIN SELECT RAISE(ABORT,'fixture ack failure'); END`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DrainRouterHeartbeatOutbox(ctx, registry, 1); err == nil || n != 0 {
		t.Fatalf("failed drain count=%d err=%v", n, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_heartbeat_outbox", int64(1))
	if _, err := s.db.Exec("DROP TRIGGER reject_heartbeat_ack"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DrainRouterHeartbeatOutbox(ctx, registry, 1); err != nil || n != 1 {
		t.Fatalf("drain count=%d err=%v", n, err)
	}
	events, err := s.RouterEvents(ctx, 0, 10)
	if err != nil || len(events) != 1 || events[0].UID != expectedUID || events[0].Source != "worker:drain" || events[0].Correlation != nil {
		t.Fatalf("heartbeat events=%+v err=%v", events, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_heartbeat_outbox", int64(0))
	if n, err := s.DrainRouterHeartbeatOutbox(ctx, registry, 0); err == nil || n != 0 {
		t.Fatalf("invalid limit count=%d err=%v", n, err)
	}
}

func TestRouterHeartbeatCurrentUIDSequenceFreshnessAndRawNumbers(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "heartbeat-current.db"))
	start := time.UnixMilli(1_800_730_000_000)
	const largeSequence = int64(9_007_199_254_741_113)
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:current", Sequence: largeSequence}, start); err != nil || !ok {
		t.Fatalf("large sequence report accepted=%v err=%v", ok, err)
	}
	var uid string
	if err := s.db.QueryRowContext(ctx, "SELECT heartbeat_uid FROM amux_presence_sources WHERE source='worker:current'").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	event := router.Event{UID: uid, ID: "client.heartbeat", Version: 1, Source: "worker:current", Payload: json.RawMessage(`{"report_sequence":9007199254741113,"reported_at":1800730000000,"agents":[]}`)}
	if current, err := routerHeartbeatCurrent(ctx, s.db, event, start.UnixMilli()+89_999); err != nil || !current {
		t.Fatalf("fresh large-number heartbeat current=%v err=%v", current, err)
	}
	for _, mutate := range []func(*router.Event){
		func(e *router.Event) { e.UID = uuid.NewString() },
		func(e *router.Event) { e.Payload = json.RawMessage(`{"report_sequence":9007199254741112}`) },
		func(e *router.Event) { e.Payload = json.RawMessage(`{"report_sequence":null}`) },
		func(e *router.Event) {
			e.Payload = json.RawMessage(`{"report_sequence":9007199254741113,"reported_at":1800730000000}`)
		},
		func(e *router.Event) { e.Payload = json.RawMessage(`{"report_sequence":9007199254741113.0}`) },
		func(e *router.Event) { e.Payload = json.RawMessage(`{"report_sequence":9007199254741113} {}`) },
	} {
		candidate := event
		mutate(&candidate)
		if current, err := routerHeartbeatCurrent(ctx, s.db, candidate, start.UnixMilli()); err != nil || current {
			t.Fatalf("invalid/reordered heartbeat current=%v err=%v event=%+v", current, err, candidate)
		}
	}
	if current, err := routerHeartbeatCurrent(ctx, s.db, event, start.UnixMilli()+90_000); err != nil || current {
		t.Fatalf("expired heartbeat current=%v err=%v", current, err)
	}
	if _, err := s.db.Exec("UPDATE amux_presence_clock SET observed_at=? WHERE id=1", start.UnixMilli()+90_000); err != nil {
		t.Fatal(err)
	}
	if current, err := routerHeartbeatCurrent(ctx, s.db, event, start.UnixMilli()); err != nil || current {
		t.Fatalf("stored clock did not expire heartbeat current=%v err=%v", current, err)
	}
}

func TestReconcileRouterPresenceIgnoresHeartbeatSnapshot(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "heartbeat-reconcile.db"))
	start := time.UnixMilli(1_800_740_000_000)
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:reconcile", Sequence: 1}, start); err != nil || !ok {
		t.Fatalf("first report accepted=%v err=%v", ok, err)
	}
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:reconcile", Sequence: 2}, start.Add(time.Second)); err != nil || !ok {
		t.Fatalf("second report accepted=%v err=%v", ok, err)
	}
	event := router.Event{UID: uuid.NewString(), ID: "client.heartbeat", Version: 1, Source: "worker:reconcile", Payload: json.RawMessage(`{"report_sequence":1,"reported_at":1800740000000,"agents":[]}`)}
	if err := s.ReconcileRouterPresence(ctx, nil, event); err != nil {
		t.Fatalf("heartbeat no-op failed: %v", err)
	}
	assertEqual(t, s.db, "SELECT sequence FROM amux_presence_sources WHERE source='worker:reconcile'", int64(2))
}

func queryHeartbeatOutboxCount(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM amux_heartbeat_outbox").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
