package storage

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPresenceSummaryCountsCurrentMachinesAgentsAndOS(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "summary.db"))
	now := time.UnixMilli(1_801_000_000_000)
	first := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	first.Busy = true
	firstHost := "agent-host"
	first.Hostname = &firstHost
	second := presenceAgent("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	second.Origin.OS = "windows"
	idleSince := now.Add(-31 * time.Second).UnixMilli()
	second.LastActivityAt = &idleSince
	secondHost := "second-host"
	second.Hostname = &secondHost
	third := presenceAgent("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", "ffffffff-ffff-4fff-8fff-ffffffffffff")
	thirdHost := "agent-host-must-lose"
	third.Hostname = &thirdHost
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "native:key", Sequence: 1, OS: "linux", Hostname: summaryString("Box-A"), Agents: []PresenceAgent{first, second, third}}, now); err != nil || !accepted {
		t.Fatalf("report accepted=%v err=%v", accepted, err)
	}
	// Historical target display metadata is a hostname preference, not a lease.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,seen_at,hostname,advertised) VALUES
		(?,?,?,'{}','',1,'Target-X',0),
		('presence','other-instance',?,'{}','',2,'Same-Project',0),
		('old-project','other-instance',?,'{}','',999999,'Newest-But-Lower-Rank',0),
		('old-project','old-instance',?,'{}','',0,'Historical-Host',0)`, "presence", first.Origin.InstanceID, first.Origin.SessionID, first.Origin.SessionID, first.Origin.SessionID, second.Origin.SessionID); err != nil {
		t.Fatal(err)
	}
	// Another source aliasing the same machine differs only by hostname case.
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "native:peer", Sequence: 1, OS: "macos", Hostname: summaryString("BOX-A")}, now); err != nil || !accepted {
		t.Fatalf("empty report accepted=%v err=%v", accepted, err)
	}
	got, err := s.PresenceSummary(ctx, now)
	want := PresenceSummary{Machines: 3, OS: []string{"linux", "macos", "windows"}, Agents: 3, Active: 1, Idle: 1}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("summary=%+v err=%v want=%+v", got, err, want)
	}
	got, err = s.PresenceSummaryForHost(ctx, now, " TARGET-X ")
	want.ThisMachine = &MachineAgentCounts{Agents: 1, Active: 1}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("summary for local host=%+v err=%v want=%+v", got, err, want)
	}
}

func TestPresenceSummarySourceFallbackPreservesCase(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "summary-source-case.db"))
	now := time.UnixMilli(1_801_050_000_000)
	for _, source := range []string{"Native:slot", "native:slot"} {
		if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: source, Sequence: 1}, now); err != nil || !accepted {
			t.Fatalf("report %q accepted=%v err=%v", source, accepted, err)
		}
	}
	got, err := s.PresenceSummary(ctx, now)
	want := PresenceSummary{Machines: 2, OS: []string{}, Agents: 0, Active: 0}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("summary=%+v err=%v want=%+v", got, err, want)
	}
}

func TestPresenceSummaryTTLUsesMonotonicClock(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "summary-ttl.db"))
	now := time.UnixMilli(1_801_100_000_000)
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "native:ttl", Sequence: 1, OS: "windows", Hostname: summaryString("Box-TTL")}, now); err != nil || !accepted {
		t.Fatalf("report accepted=%v err=%v", accepted, err)
	}
	if err := s.ReconcilePresence(ctx, now.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{now, now.Add(90 * time.Second)} {
		got, err := s.PresenceSummary(ctx, at)
		want := PresenceSummary{Machines: 0, OS: []string{}, Agents: 0, Active: 0}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("summary at %v = %+v, %v; want %+v", at, got, err, want)
		}
	}
}

func TestPresenceSummaryEnvelopeValidationAndRejectedReportRollback(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "summary-rollback.db"))
	now := time.UnixMilli(1_801_200_000_000)
	for _, report := range []PresenceReport{
		{Source: "native:bad-os", Sequence: 1, OS: "plan9"},
		{Source: "native:bad-host", Sequence: 1, Hostname: summaryString("bad\nhost")},
	} {
		if accepted, err := s.ReportPresence(ctx, report, now); accepted || !errors.Is(err, ErrPresenceInput) {
			t.Fatalf("invalid envelope accepted=%v err=%v", accepted, err)
		}
	}
	badAgent := presenceAgent("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", "ffffffff-ffff-4fff-8fff-ffffffffffff")
	badAgent.ProjectIdent = "missing"
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "native:rollback", Sequence: 2, OS: "linux", Hostname: summaryString("NoCommit"), Agents: []PresenceAgent{badAgent}}, now); accepted || !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("invalid project accepted=%v err=%v", accepted, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_sources WHERE source='native:rollback'", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_heartbeat_outbox", int64(0))
	got, err := s.PresenceSummary(ctx, now)
	want := PresenceSummary{Machines: 0, OS: []string{}, Agents: 0, Active: 0}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("summary after rollback=%+v err=%v", got, err)
	}
}

func summaryString(value string) *string { return &value }
