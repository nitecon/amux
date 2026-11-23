package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestProjectLinksPreserveRustRowsScopeValidateAndOrder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rust-project-links.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`PRAGMA foreign_keys=ON;
CREATE TABLE projects(ident TEXT PRIMARY KEY,channel_name TEXT NOT NULL,room_id TEXT NOT NULL,last_msg_id TEXT,created_at INTEGER NOT NULL);
INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('links','links','',1);
CREATE TABLE project_links(id INTEGER PRIMARY KEY AUTOINCREMENT,project_ident TEXT NOT NULL REFERENCES projects(ident) ON DELETE CASCADE,label TEXT NOT NULL,url TEXT NOT NULL,rank INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL);
CREATE INDEX idx_project_links_project ON project_links(project_ident,rank);
INSERT INTO project_links(project_ident,label,url,rank,created_at) VALUES('links','Rust CI','https://ci.example.test',4,1);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, fixtureProject("other-links")); err != nil {
		t.Fatal(err)
	}
	links, err := s.ProjectLinks(ctx, "LINKS")
	if err != nil || len(links) != 1 || links[0].Label != "Rust CI" || links[0].Rank != 4 {
		t.Fatalf("preexisting Rust links not preserved: %+v, %v", links, err)
	}
	first, err := s.AddProjectLink(ctx, "links", " Docs ", " https://docs.example.test ")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AddProjectLink(ctx, "links", "Runbook", "http://runbook.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if first.Rank != 5 || second.Rank != 6 || first.Label != "Docs" || first.URL != "https://docs.example.test" {
		t.Fatalf("add normalization/rank mismatch: %+v %+v", first, second)
	}
	ordered, err := s.ProjectLinks(ctx, "links")
	if err != nil || len(ordered) != 3 || ordered[0].Label != "Rust CI" || ordered[1].ID != first.ID || ordered[2].ID != second.ID {
		t.Fatalf("stable rank order mismatch: %+v, %v", ordered, err)
	}
	if cross, err := s.DeleteProjectLink(ctx, "other-links", first.ID); err != nil || cross {
		t.Fatalf("cross-project delete changed=%v err=%v", cross, err)
	}
	if changed, err := s.DeleteProjectLink(ctx, "links", second.ID); err != nil || !changed {
		t.Fatalf("delete changed=%v err=%v", changed, err)
	}
	if changed, err := s.DeleteProjectLink(ctx, "links", second.ID); err != nil || changed {
		t.Fatalf("repeat delete changed=%v err=%v", changed, err)
	}
	for _, input := range []struct{ label, url string }{{"", "https://ok"}, {"label", "ftp://bad"}} {
		if _, err := s.AddProjectLink(ctx, "links", input.label, input.url); err == nil {
			t.Fatalf("invalid link accepted: %+v", input)
		}
	}
	if _, err := s.AddProjectLink(ctx, "missing", "label", "https://ok"); err != ErrProjectLinkProjectNotFound {
		t.Fatalf("missing project err=%v", err)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT version FROM amux_go_migrations WHERE version=?`, projectLinksMigration).Scan(&version); err != nil || version != projectLinksMigration {
		t.Fatalf("migration version=%d err=%v", version, err)
	}
}
