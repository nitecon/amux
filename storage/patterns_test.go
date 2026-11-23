package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestPatternGetDoesNotIncludeCommentsAndCommentsAreExplicit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	pattern, err := s.InsertPattern(ctx, PatternInsert{
		Title: "Deploying Eventic Applications", Summary: patternStringPtr("Main deploys dev and tags deploy prod."),
		Body: "# Deploying Eventic Applications\n\nUse main and tags.", Labels: []string{"eventic", "deploy"}, Version: "draft", State: "active", Author: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	comment, err := s.InsertPatternComment(ctx, pattern.ID, "reviewer", "user", "Clarify tag gates.")
	if err != nil || comment == nil {
		t.Fatalf("insert comment: %+v %v", comment, err)
	}
	fetched, err := s.GetPattern(ctx, pattern.Slug)
	if err != nil || fetched == nil || fetched.Body != pattern.Body || len(fetched.Labels) != 2 || fetched.Labels[0] != "eventic" || fetched.Labels[1] != "deploy" {
		t.Fatalf("get pattern: %+v %v", fetched, err)
	}
	comments, found, err := s.ListPatternComments(ctx, pattern.Slug)
	if err != nil || !found || len(comments) != 1 || comments[0].ID != comment.ID || comments[0].Content != "Clarify tag gates." {
		t.Fatalf("explicit comments: %+v found=%v err=%v", comments, found, err)
	}
}

func TestPatternSearchChecksMarkdownBodyAndSummaryCountsComments(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	pattern, err := s.InsertPattern(ctx, PatternInsert{
		Title: "Settings Encryption", Slug: patternStringPtr("settings-encryption"), Summary: patternStringPtr("Encrypt stored settings."),
		Body: "Use envelope encryption for sensitive values.", Labels: []string{"security"}, Version: "latest", State: "active", Author: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertPatternComment(ctx, pattern.ID, "reviewer", "user", "Add key rotation notes."); err != nil {
		t.Fatal(err)
	}
	query := "envelope"
	results, err := s.ListPatterns(ctx, PatternFilters{Query: &query})
	if err != nil || len(results) != 1 || results[0].Slug != "settings-encryption" || results[0].Version != "latest" || results[0].State != "active" || results[0].CommentCount != 1 {
		t.Fatalf("body search summary: %+v %v", results, err)
	}
	emptyVersion := ""
	results, err = s.ListPatterns(ctx, PatternFilters{Version: &emptyVersion})
	if err != nil || len(results) != 1 {
		t.Fatalf("storage should keep treating an empty filter as absent: %+v %v", results, err)
	}
}

func TestPatternLifecycleMetadataIsRequiredAndSearchable(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	old, err := s.InsertPattern(ctx, PatternInsert{
		Title: "Old Systemd Build Units", Slug: patternStringPtr("old-systemd-build-units"), Summary: patternStringPtr("Compile in the service unit."),
		Body: "Do not use for new deployments.", Labels: []string{"linux", "systemd", "services"}, Version: "superseded", State: "active",
		SupersededBy: patternStringPtr("eventic-build-units"), Author: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertPattern(ctx, PatternInsert{Title: "Bad Version", Slug: patternStringPtr("bad-version"), Body: "Invalid lifecycle.", Version: "v1", State: "active", Author: "tester"}); err == nil || err.Error() != "invalid pattern version 'v1': must be draft|latest|superseded" {
		t.Fatalf("storage validation wording changed: %v", err)
	}
	fetched, err := s.GetPattern(ctx, old.Slug)
	if err != nil || fetched == nil || fetched.Version != "superseded" || fetched.State != "active" || fetched.SupersededBy == nil || *fetched.SupersededBy != "eventic-build-units" {
		t.Fatalf("lifecycle row: %+v %v", fetched, err)
	}
	label, version, target := "systemd", "superseded", "eventic-build-units"
	results, err := s.ListPatterns(ctx, PatternFilters{Label: &label, Version: &version})
	if err != nil || len(results) != 1 || results[0].Slug != "old-systemd-build-units" {
		t.Fatalf("lifecycle/label search: %+v %v", results, err)
	}
	results, err = s.ListPatterns(ctx, PatternFilters{SupersededBy: &target})
	if err != nil || len(results) != 1 || results[0].Version != "superseded" {
		t.Fatalf("superseded_by search: %+v %v", results, err)
	}
}

func TestPatternMigrationPreservesLegacyRustRowsAndComments(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "patterns-legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.ExecContext(ctx, `CREATE TABLE patterns (
 id TEXT PRIMARY KEY,title TEXT NOT NULL,slug TEXT NOT NULL UNIQUE,summary TEXT,body TEXT NOT NULL,labels TEXT,
 state TEXT NOT NULL DEFAULT 'active',author TEXT NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE pattern_comments (
 id TEXT PRIMARY KEY,pattern_id TEXT NOT NULL REFERENCES patterns(id) ON DELETE CASCADE,
 author TEXT NOT NULL,author_type TEXT NOT NULL CHECK(author_type IN ('agent','user','system')),
 content TEXT NOT NULL,created_at INTEGER NOT NULL);
INSERT INTO patterns VALUES('legacy-id','Legacy pattern','legacy-pattern','Legacy summary','Legacy markdown','["old"]','superseded-by:replacement-pattern','rust-author',111,222);
INSERT INTO pattern_comments VALUES('legacy-comment','legacy-id','legacy-reviewer','user','Legacy comment',123);`)
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
	t.Cleanup(func() { _ = s.Close() })
	pattern, err := s.GetPattern(ctx, "legacy-pattern")
	if err != nil || pattern == nil || pattern.ID != "legacy-id" || pattern.Title != "Legacy pattern" || pattern.Summary == nil || *pattern.Summary != "Legacy summary" || pattern.Body != "Legacy markdown" || pattern.Version != "draft" || pattern.State != "active" || pattern.SupersededBy == nil || *pattern.SupersededBy != "replacement-pattern" || pattern.CreatedAt != 111 || pattern.UpdatedAt != 222 {
		t.Fatalf("legacy pattern changed during migration: %+v %v", pattern, err)
	}
	comments, found, err := s.ListPatternComments(ctx, pattern.ID)
	if err != nil || !found || len(comments) != 1 || comments[0].ID != "legacy-comment" || comments[0].CreatedAt != 123 {
		t.Fatalf("legacy comment changed during migration: %+v found=%v err=%v", comments, found, err)
	}
	var recorded int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_go_migrations WHERE version=?", patternRepositoryMigration).Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("pattern migration record=%d err=%v", recorded, err)
	}
}

func patternStringPtr(value string) *string { return &value }
