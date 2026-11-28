package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nitecon/amux/identity/agent"
)

func TestTargetRegistrationLeaseIsExplicitAndIndependentOfPresence(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "target-registration.db"))
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='adhoc' WHERE ident='presence'"); err != nil {
		t.Fatal(err)
	}
	start := time.UnixMilli(1_800_200_000_000)
	request := TargetRegistration{
		Origin: agent.Origin{
			SessionID:  "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
			Provider:   "codex", OS: "linux",
		},
		Generation: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
	}
	registered, err := s.RegisterTarget(ctx, "presence", request, start)
	if err != nil {
		t.Fatal(err)
	}
	if registered.SeenAt != start.UnixMilli() || registered.ExpiresAt != start.Add(120*time.Second).UnixMilli() {
		t.Fatalf("registration lease = %+v", registered)
	}

	activity := start.Add(30 * time.Second).UnixMilli()
	agent := presenceAgent(request.Origin.SessionID, request.Origin.InstanceID)
	agent.LastActivityAt = &activity
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:target", Sequence: 1, Agents: []PresenceAgent{agent}}, start.Add(60*time.Second)); err != nil || !accepted {
		t.Fatalf("presence report accepted=%v, err=%v", accepted, err)
	}
	targets, err := s.Targets(ctx, "presence", start.Add(60*time.Second))
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets=%+v, err=%v", targets, err)
	}
	if targets[0].SeenAt != registered.SeenAt || targets[0].ExpiresAt != registered.ExpiresAt {
		t.Fatalf("event activity renewed registration: before=%+v after=%+v", registered, targets[0])
	}
	if targets, err := s.Targets(ctx, "presence", start.Add(120*time.Second)); err != nil || len(targets) != 1 {
		t.Fatalf("target at exact lease boundary=%+v, err=%v", targets, err)
	}
	if targets, err := s.Targets(ctx, "presence", start.Add(120*time.Second+time.Millisecond)); err != nil || len(targets) != 0 {
		t.Fatalf("expired targets=%+v, err=%v", targets, err)
	}
}

func TestRegisterTargetRequiresProjectBinding(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "target-binding.db"))
	remote := "github.com/example/repo"
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET canonical_remote=? WHERE ident='presence'", remote); err != nil {
		t.Fatal(err)
	}
	request := TargetRegistration{
		Origin: agent.Origin{
			SessionID:  "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
			Provider:   "codex", OS: "linux",
		},
		Generation: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
	}
	if _, err := s.RegisterTarget(ctx, "presence", request, time.UnixMilli(1_800_300_000_000)); !errors.Is(err, ErrTargetBinding) {
		t.Fatalf("missing canonical remote error=%v, want ErrTargetBinding", err)
	}
	request.CanonicalRemote = &remote
	if _, err := s.RegisterTarget(ctx, "presence", request, time.UnixMilli(1_800_300_000_000)); err != nil {
		t.Fatalf("matching canonical remote: %v", err)
	}
	malformed := "not a repository remote"
	request.CanonicalRemote = &malformed
	if _, err := s.RegisterTarget(ctx, "presence", request, time.UnixMilli(1_800_300_000_001)); !errors.Is(err, ErrTargetBinding) {
		t.Fatalf("malformed canonical remote error=%v, want ErrTargetBinding", err)
	}
	namespaceOnly := "github.com/example"
	request.CanonicalRemote = &namespaceOnly
	if _, err := s.RegisterTarget(ctx, "presence", request, time.UnixMilli(1_800_300_000_002)); !errors.Is(err, ErrTargetBinding) {
		t.Fatalf("remote without repository error=%v, want ErrTargetBinding", err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_target_presence").Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed registrations changed directory count=%d, err=%v", count, err)
	}
	request.Generation = "invalid"
	if _, err := s.RegisterTarget(ctx, "presence", request, time.UnixMilli(1_800_300_000_003)); !errors.Is(err, ErrTargetInput) {
		t.Fatalf("invalid generation error=%v, want ErrTargetInput", err)
	}
}

func TestTargetRegistrationMigrationPreservesLegacyMetadata(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy-targets.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `CREATE TABLE projects(ident TEXT PRIMARY KEY);
CREATE TABLE amux_go_migrations(version INTEGER PRIMARY KEY);
CREATE TABLE task_target_presence (
 project_ident TEXT NOT NULL, instance_id TEXT NOT NULL, session_id TEXT NOT NULL,
 origin_json TEXT NOT NULL, generation TEXT NOT NULL, seen_at INTEGER NOT NULL DEFAULT 0,
 canonical_remote TEXT NOT NULL DEFAULT '', display_name TEXT, hostname TEXT, advertised INTEGER NOT NULL DEFAULT 1,
 source TEXT, agent_json TEXT, report_seen_at INTEGER, output_at INTEGER, draft INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL DEFAULT 'unknown', identity_seq INTEGER NOT NULL DEFAULT 0, activity_seq INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(project_ident,instance_id,session_id));
INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,seen_at,source,agent_json,report_seen_at,output_at,draft,state,identity_seq,activity_seq,display_name,hostname)
VALUES('p','bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','{}','cccccccc-cccc-4ccc-8ccc-cccccccccccc',10,'source','{"agent":true}',8,7,1,'busy',3,4,'Agent','host');`)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateTargetRegistration(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var source, agentJSON, state, displayName, hostname string
	var seen, reportSeen, output, identitySeq, activitySeq int64
	var draft int
	err = db.QueryRowContext(ctx, `SELECT source,agent_json,state,display_name,hostname,seen_at,report_seen_at,output_at,draft,identity_seq,activity_seq
FROM task_target_presence WHERE project_ident='p'`).Scan(&source, &agentJSON, &state, &displayName, &hostname, &seen, &reportSeen, &output, &draft, &identitySeq, &activitySeq)
	if err != nil {
		t.Fatal(err)
	}
	if source != "source" || agentJSON != `{"agent":true}` || state != "busy" || displayName != "Agent" || hostname != "host" || seen != 10 || reportSeen != 8 || output != 7 || draft != 1 || identitySeq != 3 || activitySeq != 4 {
		t.Fatalf("migration changed legacy presence metadata")
	}
}

func TestTargetRegistrationMigrationAddsColumnsToMinimalDirectory(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "minimal-targets.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `CREATE TABLE projects(ident TEXT PRIMARY KEY);
CREATE TABLE amux_go_migrations(version INTEGER PRIMARY KEY);
CREATE TABLE task_target_presence(
 project_ident TEXT NOT NULL, instance_id TEXT NOT NULL, session_id TEXT NOT NULL,
 origin_json TEXT NOT NULL, generation TEXT NOT NULL, seen_at INTEGER NOT NULL DEFAULT 0,
 hostname TEXT, PRIMARY KEY(project_ident,instance_id,session_id));
INSERT INTO task_target_presence VALUES('legacy','instance','session','{}','generation',17,'legacy-host');`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateTargetRegistration(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	readTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns, err := retentionTableColumns(ctx, readTx, "task_target_presence")
	_ = readTx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"canonical_remote", "display_name", "advertised", "source", "agent_json", "report_seen_at", "output_at", "draft", "state", "identity_seq", "activity_seq"} {
		if !columns[column] {
			t.Errorf("migration did not add %s", column)
		}
	}
	var seen int64
	var hostname string
	if err := db.QueryRowContext(ctx, "SELECT seen_at,hostname FROM task_target_presence WHERE project_ident='legacy'").Scan(&seen, &hostname); err != nil || seen != 17 || hostname != "legacy-host" {
		t.Fatalf("minimal legacy row changed: seen_at=%d hostname=%q err=%v", seen, hostname, err)
	}
}

func TestRegisterTargetRollsBackFailedInsert(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "target-rollback.db"))
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='adhoc' WHERE ident='presence'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_target_registration BEFORE INSERT ON task_target_presence
BEGIN SELECT RAISE(ABORT,'registration rejected'); END;`); err != nil {
		t.Fatal(err)
	}
	request := TargetRegistration{
		Origin:     agent.Origin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", OS: "linux"},
		Generation: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
	}
	if _, err := s.RegisterTarget(ctx, "presence", request, time.UnixMilli(1_800_400_000_000)); err == nil {
		t.Fatal("trigger-rejected registration succeeded")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_target_presence").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed registration left %d rows, err=%v", count, err)
	}
}
