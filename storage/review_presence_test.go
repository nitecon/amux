package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewContextCapabilityMigrationAndDurableReports(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "presence.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, fixtureProject("reviews")); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	activity := now.Add(-time.Minute).UnixMilli()
	report := PresenceReport{Source: "host", Sequence: 1, Agents: []PresenceAgent{{ProjectIdent: "reviews", Origin: *taskActor(), AutoAssign: true, LastActivityAt: &activity}}}
	if _, err := s.ReportPresence(ctx, report, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the pre-capability Go schema and open it through the normal
	// migration path. Existing clients must not gain a reset capability.
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `ALTER TABLE amux_presence_agents DROP COLUMN review_context_clear;
 DELETE FROM amux_go_migrations WHERE version>=12;
 DROP TABLE IF EXISTS assignment_turns;`); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := s.Presence(ctx, "reviews")
	if err != nil || len(agents) != 1 || agents[0].ReviewContextClear {
		t.Fatalf("legacy capability granted: %+v %v", agents, err)
	}
	report.Sequence++
	report.Agents[0].ReviewContextClear = true
	if _, err := s.ReportPresence(ctx, report, now); err != nil {
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
	agents, err = s.Presence(ctx, "reviews")
	if err != nil || len(agents) != 1 || !agents[0].ReviewContextClear {
		t.Fatalf("explicit capability not persisted: %+v %v", agents, err)
	}
}
