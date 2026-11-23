package storage

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
)

func TestPresenceSummarySurvivesUnsubscribedHeartbeatDrainAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "machine-inventory.db")
	s := presenceStore(t, path)
	now := time.UnixMilli(1_800_000_000_000)
	report := PresenceReport{Source: "native:empty", Sequence: 1, OS: "linux", Hostname: ptr("Box-A")}
	if accepted, err := s.ReportPresence(ctx, report, now); err != nil || !accepted {
		t.Fatalf("empty inventory: %v %v", accepted, err)
	}
	check := func(store *Store, at time.Time, machines int, platforms []string) {
		t.Helper()
		summary, err := store.PresenceSummary(ctx, at)
		if err != nil || summary.Machines != machines || summary.Agents != 0 || summary.Active != 0 || !reflect.DeepEqual(summary.OS, platforms) {
			t.Fatalf("machine inventory: %+v error=%v want machines=%d os=%v", summary, err, machines, platforms)
		}
	}
	check(s, now, 1, []string{"linux"})
	registry, err := router.NewRegistry([]router.EventDefinition{plugins.ClientHeartbeatDefinition()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if drained, err := s.DrainRouterHeartbeatOutbox(ctx, registry, 200); err != nil || drained != 1 {
		t.Fatalf("drain without subscribers: %d %v", drained, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_heartbeat_outbox", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events", int64(0))
	check(s, now, 1, []string{"linux"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check(s, now, 1, []string{"linux"})
	report.OS, report.Hostname = "windows", ptr("Box-B")
	if accepted, err := s.ReportPresence(ctx, report, now.Add(time.Millisecond)); err != nil || accepted {
		t.Fatalf("replay changed source inventory: %v %v", accepted, err)
	}
	check(s, now, 1, []string{"linux"})
	report.Sequence++
	latest := now.Add(time.Millisecond)
	if accepted, err := s.ReportPresence(ctx, report, latest); err != nil || !accepted {
		t.Fatalf("new inventory: %v %v", accepted, err)
	}
	check(s, latest, 1, []string{"windows"})
	if err := s.ReconcilePresence(ctx, latest.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	check(s, latest.Add(90*time.Second), 0, []string{})
	check(s, now, 0, []string{})
}

func TestPresenceSummaryUpgradePreservesAcceptedCursor(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pre-summary.db")
	s := presenceStore(t, path)
	now := time.UnixMilli(1_800_000_000_000)
	report := PresenceReport{Source: "native:upgrade", Sequence: 7}
	if accepted, err := s.ReportPresence(ctx, report, now); err != nil || !accepted {
		t.Fatalf("seed old inventory: %v %v", accepted, err)
	}
	var uid string
	if err := s.db.QueryRowContext(ctx, "SELECT heartbeat_uid FROM amux_presence_sources WHERE source=?", report.Source).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE amux_presence_sources DROP COLUMN os;
		ALTER TABLE amux_presence_sources DROP COLUMN hostname;
		DELETE FROM amux_go_migrations WHERE version=26`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertEqual(t, s.db, "SELECT sequence FROM amux_presence_sources WHERE source='native:upgrade'", int64(7))
	assertEqual(t, s.db, "SELECT heartbeat_uid FROM amux_presence_sources WHERE source='native:upgrade'", uid)
	assertEqual(t, s.db, "SELECT heartbeat_seen_at FROM amux_presence_sources WHERE source='native:upgrade'", now.UnixMilli())
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_heartbeat_outbox", int64(1))
	report.OS, report.Hostname = "windows", ptr("New-Metadata")
	if accepted, err := s.ReportPresence(ctx, report, now); err != nil || accepted {
		t.Fatalf("upgrade lost source replay fence: %v %v", accepted, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_sources WHERE os IS NULL AND hostname IS NULL", int64(1))
	report.Sequence++
	if accepted, err := s.ReportPresence(ctx, report, now.Add(time.Millisecond)); err != nil || !accepted {
		t.Fatalf("new inventory after upgrade: %v %v", accepted, err)
	}
	summary, err := s.PresenceSummary(ctx, now.Add(time.Millisecond))
	if err != nil || summary.Machines != 1 || !reflect.DeepEqual(summary.OS, []string{"windows"}) {
		t.Fatalf("upgraded inventory: %+v %v", summary, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM pragma_foreign_key_check", int64(0))
}
