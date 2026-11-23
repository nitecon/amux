package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

func TestEnsureSecurityInstallationReusesRustProfileIdentity(t *testing.T) {
	ctx := context.Background()
	profile := t.TempDir()
	databasePath := filepath.Join(profile, "security.sqlite")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE installation(id INTEGER PRIMARY KEY CHECK(id=1),cluster_id TEXT NOT NULL);
		INSERT INTO installation(id,cluster_id) VALUES(1,'rust-profile-cluster');`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := EnsureSecurityInstallation(ctx, profile)
	if err != nil || got != "rust-profile-cluster" {
		t.Fatalf("cluster identity=%q err=%v", got, err)
	}
}

func TestEnsureSecurityInstallationPersistsFreshIdentityAcrossRestart(t *testing.T) {
	ctx := context.Background()
	profile := t.TempDir()
	first, err := EnsureSecurityInstallation(ctx, profile)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := uuid.Parse(first)
	if err != nil || parsed.Version() != 7 {
		t.Fatalf("fresh cluster identity=%q err=%v, want UUIDv7", first, err)
	}
	second, err := EnsureSecurityInstallation(ctx, profile)
	if err != nil || second != first {
		t.Fatalf("restarted cluster identity=%q err=%v, want %q", second, err, first)
	}
	if _, err := os.Stat(filepath.Join(profile, "security.sqlite")); err != nil {
		t.Fatalf("missing security profile database: %v", err)
	}
}
