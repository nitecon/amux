package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	projectid "github.com/nitecon/amux/identity/project"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "amux.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func fixtureProject(ident string) Project {
	return Project{Ident: ident, ChannelName: "test", RoomID: "room-" + ident, CreatedAt: 123, Kind: "repo"}
}
func requireProject(t *testing.T, s *Store, ident string) Project {
	t.Helper()
	p, err := s.Project(context.Background(), ident)
	if err != nil || p == nil {
		t.Fatalf("project %q: %+v, %v", ident, p, err)
	}
	return *p
}

// Rust db.rs::project_insert_and_lookup_normalize_ident_to_lowercase.
func TestProjectNormalization(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.InsertProject(ctx, fixtureProject("DemoProject")); err != nil {
		t.Fatal(err)
	}
	p := requireProject(t, s, "DEMOPROJECT")
	if p.Ident != "demoproject" {
		t.Fatalf("ident %q", p.Ident)
	}
	list, err := s.Projects(ctx)
	if err != nil || len(list) != 1 || list[0].Ident != "demoproject" {
		t.Fatalf("list %+v, %v", list, err)
	}
	var cursor string
	if err := s.db.QueryRowContext(ctx, "SELECT project_ident FROM cursors").Scan(&cursor); err != nil || cursor != "demoproject" {
		t.Fatalf("cursor %q, %v", cursor, err)
	}
	if err := s.InsertProject(ctx, fixtureProject("DEMOPROJECT")); err != nil {
		t.Fatal(err)
	}
}

// Rust db.rs::repo_mapping_rejects_partial_input_and_clears_when_blank.
func TestRepoMappingValidation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.InsertProject(ctx, fixtureProject("proj")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRepoMapping(ctx, "proj", "github", "nitecon", ""); err == nil || !strings.Contains(err.Error(), "together") {
		t.Fatalf("partial mapping: %v", err)
	}
	mapped, err := s.UpdateRepoMapping(ctx, "proj", "", "nitecon", "proj")
	if err != nil || mapped == nil || mapped.RepoFullName == nil || *mapped.RepoFullName != "nitecon/proj" || *mapped.RepoProvider != "github" || mapped.Kind != "repo" {
		t.Fatalf("mapped %+v, %v", mapped, err)
	}
	cleared, err := s.UpdateRepoMapping(ctx, "proj", "github", "", "")
	if err != nil || cleared == nil || cleared.RepoFullName != nil || cleared.RepoProvider != nil || cleared.Kind != "adhoc" {
		t.Fatalf("cleared %+v, %v", cleared, err)
	}
}

// Rust db.rs::project_repo_mapping_normalizes_and_rejects_casefolded_duplicates.
func TestRepoMappingUniqueness(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"alpha", "beta"} {
		if err := s.InsertProject(ctx, fixtureProject(id)); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.UpdateRepoMapping(ctx, "ALPHA", "GitHub", "NiteCon", "Agent-Gateway")
	if err != nil || p == nil || *p.RepoProvider != "github" || *p.RepoNamespace != "nitecon" || *p.RepoName != "agent-gateway" || *p.RepoFullName != "nitecon/agent-gateway" {
		t.Fatalf("mapping %+v, %v", p, err)
	}
	if _, err := s.UpdateRepoMapping(ctx, "beta", "github", "NITECON", "agent-gateway"); err == nil || !strings.Contains(err.Error(), "already mapped") {
		t.Fatalf("duplicate mapping: %v", err)
	}
	if requireProject(t, s, "beta").RepoFullName != nil {
		t.Fatal("failed mutation persisted")
	}
}

// Rust db.rs::backfill_project_repo_fills_empty_mapping_but_never_overwrites.
func TestRepoBackfill(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.InsertProject(ctx, fixtureProject("proj")); err != nil {
		t.Fatal(err)
	}
	repo, err := projectid.ParseRemote("https://github.com/nitecon/proj.git")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := s.BackfillRepo(ctx, "proj", repo)
	if err != nil || !changed {
		t.Fatalf("backfill %v, %v", changed, err)
	}
	p := requireProject(t, s, "proj")
	if p.RepoFullName == nil || *p.RepoFullName != "nitecon/proj" || *p.CanonicalRemote != "github.com/nitecon/proj" || p.Kind != "repo" {
		t.Fatalf("backfilled %+v", p)
	}
	other, err := projectid.ParseRemote("https://gitlab.com/x/y.git")
	if err != nil {
		t.Fatal(err)
	}
	changed, err = s.BackfillRepo(ctx, "proj", other)
	if err != nil || changed {
		t.Fatalf("overwrite %v, %v", changed, err)
	}
	changed, err = s.BackfillRepo(ctx, "missing", repo)
	if err != nil || changed {
		t.Fatalf("missing %v, %v", changed, err)
	}
}

func TestLegacySchemaUpgradeAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	ctx := context.Background()
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE projects(ident TEXT PRIMARY KEY,channel_name TEXT NOT NULL,room_id TEXT NOT NULL,last_msg_id TEXT,created_at INTEGER NOT NULL);
 INSERT INTO projects VALUES('legacy','none','room','last',42);
 CREATE TABLE unrelated(value TEXT);INSERT INTO unrelated VALUES('preserve me');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		p := requireProject(t, s, "legacy")
		if p.CreatedAt != 42 || p.LastMsgID == nil || *p.LastMsgID != "last" || p.Kind != "repo" {
			t.Fatalf("legacy changed: %+v", p)
		}
		var value string
		if err := s.db.QueryRowContext(ctx, "SELECT value FROM unrelated").Scan(&value); err != nil || value != "preserve me" {
			t.Fatalf("lost unrelated data: %q, %v", value, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// Rust db.rs::project_casing_validation_rejects_legacy_mixed_case_rows.
func TestRejectLegacyMixedCase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('MixedCase','test','room',1)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if s != nil {
		s.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "lowercase") {
		t.Fatalf("mixed case accepted: %v", err)
	}
}

func TestProjectInsertRollsBackCursorFailure(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_cursor BEFORE INSERT ON cursors BEGIN SELECT RAISE(ABORT,'fixture rejects cursor'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, fixtureProject("atomic")); err == nil {
		t.Fatal("failed cursor insertion accepted")
	}
	p, err := s.Project(ctx, "atomic")
	if err != nil || p != nil {
		t.Fatalf("partial project persisted: %+v, %v", p, err)
	}
}

func TestArchiveAndReservedGlobalProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"normal", "__global__"} {
		if err := s.InsertProject(ctx, fixtureProject(id)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.Projects(ctx)
	if err != nil || len(list) != 1 || list[0].Ident != "normal" {
		t.Fatalf("reserved project surfaced: %+v, %v", list, err)
	}
	p, err := s.SetProjectArchived(ctx, "NORMAL", true)
	if err != nil || p == nil || p.ArchivedAt == nil {
		t.Fatalf("archive %+v, %v", p, err)
	}
	p, err = s.SetProjectArchived(ctx, "normal", false)
	if err != nil || p == nil || p.ArchivedAt != nil {
		t.Fatalf("restore %+v, %v", p, err)
	}
}

func TestProjectWritesSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.db")
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, fixtureProject("durable")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRepoMapping(ctx, "durable", "github", "owner", "repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetProjectArchived(ctx, "durable", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version=77"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := requireProject(t, s, "durable")
	if p.RepoFullName == nil || *p.RepoFullName != "owner/repo" || p.ArchivedAt == nil {
		t.Fatalf("lost committed project data: %+v", p)
	}
	var version, foreignKeys int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 77 {
		t.Fatalf("legacy schema version changed: %d, %v", version, err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign keys disabled: %d, %v", foreignKeys, err)
	}
}
