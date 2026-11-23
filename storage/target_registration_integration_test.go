package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTargetRegistrationUpgradePreservesFullRustDirectory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rust-directory.db")
	schema, err := os.ReadFile("testdata/task_status_legacy.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, string(schema)); err != nil {
		t.Fatal(err)
	}
	seedRustTaskProfile(t, ctx, db)
	original := snapshotRustSchema(t, db)
	if _, err := db.ExecContext(ctx, `INSERT INTO task_target_presence
		(project_ident,instance_id,session_id,origin_json,generation,seen_at,canonical_remote,
		display_name,hostname,advertised,source,agent_json,report_seen_at,output_at,draft,state,identity_seq,activity_seq)
		VALUES('rust-project','legacy-instance','legacy-session','{"provider":"codex"}','legacy-generation',123,'github.com/nitecon/amux',
		'Legacy agent','retained-host',1,'retained-source','{"project":"rust-project","session_id":"legacy-session"}',124,125,1,'busy',126,127)`); err != nil {
		t.Fatal(err)
	}
	var before string
	const snapshot = `SELECT json_array(project_ident,instance_id,session_id,origin_json,generation,seen_at,canonical_remote,
		display_name,hostname,advertised,source,agent_json,report_seen_at,output_at,draft,state,identity_seq,activity_seq)
		FROM task_target_presence WHERE session_id='legacy-session'`
	if err := db.QueryRowContext(ctx, snapshot).Scan(&before); err != nil {
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
	assertRustSchemaPreserved(t, s.db, original)
	var after string
	if err := s.db.QueryRowContext(ctx, snapshot).Scan(&after); err != nil || before != after {
		t.Fatalf("retained directory changed: before=%s after=%s error=%v", before, after, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM pragma_foreign_key_check", int64(0))
}

// Registration is permission to route manually; native reports are observations.
// Neither lease may accidentally extend or erase the other.
func TestTargetRegistrationAndNativePresenceHaveIndependentLeases(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "directory.db"))
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='adhoc',canonical_remote=NULL WHERE ident='presence'"); err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(1_800_000_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	agent.Busy = true
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:directory", Sequence: 1, Agents: []PresenceAgent{agent}}, now); err != nil || !ok {
		t.Fatalf("report: %v %v", ok, err)
	}
	if targets, err := s.Targets(ctx, "presence", now); err != nil || len(targets) != 0 {
		t.Fatalf("observation granted routing: %+v %v", targets, err)
	}
	request := TargetRegistration{Origin: agent.Origin, Generation: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Hostname: ptr("worker-host")}
	if _, err := s.RegisterTarget(ctx, "presence", request, now); err != nil {
		t.Fatal(err)
	}
	if item := onlyPresence(t, s); item.State != "busy" || item.Source != "worker:directory" {
		t.Fatalf("advertisement replaced native observation: %+v", item)
	}
	// A later advertisement can change routing metadata, never event ownership.
	request.Generation = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	if _, err := s.RegisterTarget(ctx, "presence", request, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:foreign", Sequence: 1, Agents: []PresenceAgent{agent}}, now.Add(2*time.Second)); !errors.Is(err, ErrPresenceConflict) {
		t.Fatalf("advertisement released native source ownership: %v", err)
	}
	if err := s.ReconcilePresence(ctx, now.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if item := onlyPresence(t, s); item.State != "gone" {
		t.Fatalf("registration extended native liveness: %+v", item)
	}
	if targets, err := s.Targets(ctx, "presence", now.Add(90*time.Second)); err != nil || len(targets) != 1 {
		t.Fatalf("native expiry erased registration: %+v %v", targets, err)
	}
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:directory", Sequence: 2, Agents: []PresenceAgent{agent}}, now.Add(121*time.Second)); err != nil || !ok {
		t.Fatalf("fresh observation: %v %v", ok, err)
	}
	if targets, err := s.Targets(ctx, "presence", now.Add(122*time.Second)); err != nil || len(targets) != 0 {
		t.Fatalf("observation renewed manual lease: %+v %v", targets, err)
	}
	if item := onlyPresence(t, s); item.State != "busy" {
		t.Fatalf("registration expiry erased observation: %+v", item)
	}
}

func TestTargetRegistrationRebindingRevokesRoutingButKeepsDisplayHistory(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "rebind.db"))
	remote := "github.com/example/original"
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET canonical_remote=? WHERE ident='presence'", remote); err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(1_800_000_000_000)
	origin := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb").Origin
	request := TargetRegistration{Origin: origin, Generation: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", CanonicalRemote: &remote, Hostname: ptr("historical-host")}
	if _, err := s.RegisterTarget(ctx, "presence", request, now); err != nil {
		t.Fatal(err)
	}
	if targets, err := s.Targets(ctx, "presence", now); err != nil || len(targets) != 1 {
		t.Fatalf("matching target not routable: %+v %v", targets, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET canonical_remote='github.com/example/replacement' WHERE ident='presence'"); err != nil {
		t.Fatal(err)
	}
	if targets, err := s.Targets(ctx, "presence", now.Add(time.Second)); err != nil || len(targets) != 0 {
		t.Fatalf("rebound target retained routing authority: %+v %v", targets, err)
	}
	// An obsolete directory entry is filtered before its legacy JSON is decoded.
	if _, err := s.db.ExecContext(ctx, "UPDATE task_target_presence SET origin_json='obsolete invalid JSON'"); err != nil {
		t.Fatal(err)
	}
	if targets, err := s.Targets(ctx, "presence", now.Add(time.Second)); err != nil || len(targets) != 0 {
		t.Fatalf("obsolete origin poisoned current directory: %+v %v", targets, err)
	}
	assertEqual(t, s.db, "SELECT hostname FROM task_target_presence WHERE session_id='aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'", "historical-host")
	if _, err := s.RegisterTarget(ctx, "presence", request, now.Add(2*time.Second)); !errors.Is(err, ErrTargetBinding) {
		t.Fatalf("obsolete binding renewed registration: %v", err)
	}
	assertEqual(t, s.db, "SELECT seen_at FROM task_target_presence WHERE session_id='aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'", now.UnixMilli())
}
