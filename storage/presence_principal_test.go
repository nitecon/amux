package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
)

func TestPresencePrincipalBindingSequenceAndTrustedReports(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "principal.db"))
	now := time.UnixMilli(1_810_000_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	agent.AutoAssign = true
	agent.Goal = stringPtr("preserve full native profile")
	legacy := PresenceReport{Source: "shared-source", Sequence: 1, Agents: []PresenceAgent{agent}}
	if accepted, err := s.ReportPresence(ctx, legacy, now); err != nil || !accepted {
		t.Fatalf("trusted report: accepted=%v err=%v", accepted, err)
	}
	if principal, err := nativeSourcePrincipal(ctx, s.db, legacy.Source, 1); err != nil || principal != "" {
		t.Fatalf("legacy source gained principal: %q %v", principal, err)
	}
	if accepted, err := s.ReportPresenceWithPrincipal(ctx, PresenceReport{Source: legacy.Source, Sequence: 2, Agents: []PresenceAgent{agent}}, now.Add(time.Second), "api-key"); err != nil || !accepted {
		t.Fatalf("authenticated report: accepted=%v err=%v", accepted, err)
	}
	if principal, err := nativeSourcePrincipal(ctx, s.db, legacy.Source, 1); err != nil || principal != "" {
		t.Fatalf("stale sequence proof=%q err=%v", principal, err)
	}
	if principal, err := nativeSourcePrincipal(ctx, s.db, legacy.Source, 2); err != nil || principal != "api-key" {
		t.Fatalf("current source proof=%q err=%v", principal, err)
	}
	if _, err := s.ReportPresenceWithPrincipal(ctx, PresenceReport{Source: legacy.Source, Sequence: 2}, now.Add(2*time.Second), "other-key"); !errors.Is(err, ErrPresenceConflict) {
		t.Fatalf("wrong-principal replay error=%v, want conflict", err)
	}
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: legacy.Source, Sequence: 3, Agents: []PresenceAgent{agent}}, now.Add(3*time.Second)); err != nil || !accepted {
		t.Fatalf("trusted follow-up: accepted=%v err=%v", accepted, err)
	}
	if principal, err := nativeSourcePrincipal(ctx, s.db, legacy.Source, 3); err != nil || principal != "" {
		t.Fatalf("trusted sequence inherited principal proof=%q err=%v", principal, err)
	}
	if principal, err := nativeSourcePrincipal(ctx, s.db, legacy.Source, 2); err != nil || principal != "" {
		t.Fatalf("superseded sequence retained principal proof=%q err=%v", principal, err)
	}
	presence := onlyPresence(t, s)
	if !presence.AutoAssign || presence.Goal == nil || *presence.Goal != "preserve full native profile" {
		t.Fatalf("principal update lost existing native profile: %+v", presence)
	}
}

func TestPresencePrincipalRejectsCrossPrincipalAfterSourceGone(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "gone-principal.db"))
	now := time.UnixMilli(1_810_100_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if accepted, err := s.ReportPresenceWithPrincipal(ctx, PresenceReport{Source: "immutable-source", Sequence: 1, Agents: []PresenceAgent{agent}}, now, "api-key"); err != nil || !accepted {
		t.Fatalf("initial report: accepted=%v err=%v", accepted, err)
	}
	if accepted, err := s.ReportPresenceWithPrincipal(ctx, PresenceReport{Source: "immutable-source", Sequence: 2}, now.Add(2*time.Minute), "api-key"); err != nil || !accepted {
		t.Fatalf("gone snapshot: accepted=%v err=%v", accepted, err)
	}
	if _, err := s.ReportPresenceWithPrincipal(ctx, PresenceReport{Source: "immutable-source", Sequence: 3}, now.Add(3*time.Minute), "different-principal"); !errors.Is(err, ErrPresenceConflict) {
		t.Fatalf("cross-principal reuse of gone source error=%v, want conflict", err)
	}
}

func TestPresencePrincipalRollbackDoesNotBindSource(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "rollback-principal.db"))
	now := time.UnixMilli(1_810_200_000_000)
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_principal_source BEFORE INSERT ON amux_presence_sources
		WHEN NEW.source='rollback-source' BEGIN SELECT RAISE(ABORT,'injected source failure'); END`); err != nil {
		t.Fatal(err)
	}
	if accepted, err := s.ReportPresenceWithPrincipal(ctx, PresenceReport{Source: "rollback-source", Sequence: 1}, now, "api-key"); err == nil || accepted {
		t.Fatalf("injected failure accepted=%v err=%v", accepted, err)
	}
	if principal, err := nativeSourcePrincipal(ctx, s.db, "rollback-source", 1); err != nil || principal != "" {
		t.Fatalf("failed transaction left principal=%q err=%v", principal, err)
	}
	var sources int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_presence_sources WHERE source='rollback-source'").Scan(&sources); err != nil || sources != 0 {
		t.Fatalf("failed transaction source rows=%d err=%v", sources, err)
	}
}

func TestNativeSourcePrincipalMigrationKeepsLegacyAndRustFieldsFailClosed(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "principal-migration.db"))
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "legacy-source", Sequence: 4}, time.Now()); err != nil || !accepted {
		t.Fatalf("legacy source insert: accepted=%v err=%v", accepted, err)
	}
	var principal sql.NullString
	if err := s.db.QueryRowContext(ctx, "SELECT principal FROM amux_presence_sources WHERE source='legacy-source'").Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if principal.Valid {
		t.Fatalf("trusted legacy source was migrated into principal %q", principal.String)
	}
	hasRustPrincipal, err := sqliteTableHasColumn(ctx, s.db, "assignment_offers", "reporter_principal")
	if err != nil {
		t.Fatal(err)
	}
	if hasRustPrincipal {
		t.Fatal("migration changed frozen Rust assignment_offers schema")
	}
	hasNativePrincipal, err := sqliteTableHasColumn(ctx, s.db, "amux_assignment_offer_claims", "reporter_principal")
	if err != nil || !hasNativePrincipal {
		t.Fatalf("native offer claim principal field present=%v err=%v", hasNativePrincipal, err)
	}
}

func TestNativeSourcePrincipalDoesNotAuthorizeRawOrMismatchedMembership(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "membership-principal.db"))
	now := time.UnixMilli(1_810_300_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if accepted, err := s.ReportPresenceWithPrincipal(ctx, PresenceReport{Source: "bound-source", Sequence: 7, Agents: []PresenceAgent{agent}}, now, "api-key"); err != nil || !accepted {
		t.Fatalf("authenticated report: accepted=%v err=%v", accepted, err)
	}
	// The helper proves only source ownership. A raw target record and a target
	// whose source/sequence differs remain ineligible for native callbacks.
	if _, err := s.db.ExecContext(ctx, `UPDATE amux_presence_agents SET report_kind='event'
		WHERE project_ident=? AND instance_id=? AND session_id=?`, agent.ProjectIdent, agent.Origin.InstanceID, agent.Origin.SessionID); err != nil {
		t.Fatal(err)
	}
	principal, err := nativeSourcePrincipal(ctx, s.db, "bound-source", 7)
	if err != nil || principal != "api-key" {
		t.Fatalf("source proof=%q err=%v", principal, err)
	}
	var nativeMembership int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM amux_presence_agents WHERE project_ident=? AND instance_id=? AND session_id=?
		AND source=? AND sequence=? AND report_kind='native' AND state!='gone'`, agent.ProjectIdent, agent.Origin.InstanceID, agent.Origin.SessionID, "bound-source", 7).Scan(&nativeMembership)
	if err != nil || nativeMembership != 0 {
		t.Fatalf("event observation incorrectly qualifies as native membership: count=%d err=%v", nativeMembership, err)
	}
	if got, err := nativeSourcePrincipal(ctx, s.db, "bound-source", 6); err != nil || got != "" {
		t.Fatalf("mismatched source sequence proof=%q err=%v", got, err)
	}
}

func TestAssignmentOfferCapturesCurrentNativeReporterPrincipal(t *testing.T) {
	s, agent, first, _ := assignmentPluginFixture(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-31 * time.Second)
	activity := old.UnixMilli()
	agent.LastActivityAt = &activity
	if accepted, err := s.ReportPresenceWithPrincipal(ctx, PresenceReport{Source: "worker:assignment", Sequence: 2, Agents: []PresenceAgent{agent}}, old.Add(time.Second), "api-key"); err != nil || !accepted {
		t.Fatalf("authenticated assignment reporter: accepted=%v err=%v", accepted, err)
	}
	definitions := append(append(nativePresenceDefinitions(), RouterTaskEventDefinitions()...), controllers.NotificationEventDefinitions()...)
	runtime, err := fism.NewRuntime(s, s.EventBus(), definitions, []fism.Controller{
		controllers.PresenceController(s), controllers.AssignmentController(s), assignmentNotifyObserver(),
	}, fism.RunConfig{Capacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	clock, err := s.EmitRouterEvent(ctx, runtime.Registry(), fism.EmitRequest{ID: "clock.tick", Version: 1, Source: "clock", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	stop := runRouterFixture(t, runtime)
	defer stop()
	waitRouterDeliveryState(t, s, clock.Event.Seq, "assignment", "completed")
	assertEqual(t, s.db, "SELECT reporter_principal FROM amux_assignment_offer_claims WHERE notify_uid=(SELECT notify_uid FROM assignment_offers WHERE task_id='"+first.ID+"')", "api-key")
}

func stringPtr(value string) *string { return &value }
