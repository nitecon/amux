package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/identity/agent"
)

func TestProjectOverviewsUseBindingFilteredMonotonicPresenceSnapshot(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "project-overview-presence.db"))
	const oldRemote = "github.com/example/old"
	const newRemote = "github.com/example/new"
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET canonical_remote=? WHERE ident='presence'", oldRemote); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Minute).UnixMilli()
	target := TargetRegistration{
		Origin:     agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"},
		Generation: uuid.NewString(), CanonicalRemote: ptr(oldRemote), Hostname: ptr("Manual-Host"),
	}
	if _, err := s.RegisterTarget(ctx, "presence", target, time.UnixMilli(base)); err != nil {
		t.Fatal(err)
	}
	registry, err := fism.NewRegistry(nativePresenceDefinitions(), []fism.ControllerDefinition{controllers.PresenceController(nil).Definition})
	if err != nil {
		t.Fatal(err)
	}
	rawSession, rawInstance := uuid.NewString(), uuid.NewString()
	heartbeat := emitObservationFixtureEvent(t, s, registry, controllers.ClientHeartbeatID, "raw:overview",
		`{"os":"linux","hostname":"Observed-Host","agents":[{"project":"presence","session_id":"`+rawSession+`","instance_id":"`+rawInstance+`","provider":"codex","os":"linux","canonical_remote":"`+oldRemote+`"}]}`, base)
	if err := s.ReconcilePresenceEvent(ctx, registry, heartbeat); err != nil {
		t.Fatal(err)
	}
	projects, err := s.ProjectOverviews(ctx, time.UnixMilli(base))
	if err != nil || len(projects) != 1 || projects[0].Agents != 1 {
		t.Fatalf("matching binding overview=%+v, err=%v", projects, err)
	}

	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET canonical_remote=? WHERE ident='presence'", newRemote); err != nil {
		t.Fatal(err)
	}
	live, err := s.LivePresence(ctx, time.UnixMilli(base))
	if err != nil || len(live) != 0 {
		t.Fatalf("rebound project retained stale live observation: %+v, err=%v", live, err)
	}
	projects, err = s.ProjectOverviews(ctx, time.UnixMilli(base))
	if err != nil || len(projects) != 1 || projects[0].Agents != 0 {
		t.Fatalf("rebound project overview=%+v, err=%v", projects, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events WHERE source='raw:overview'", int64(1))
	assertEqual(t, s.db, "SELECT hostname FROM task_target_presence WHERE hostname='Manual-Host'", "Manual-Host")

	native := presenceAgent(uuid.NewString(), uuid.NewString())
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "native:overview", Sequence: 1, Agents: []PresenceAgent{native}}, time.UnixMilli(base)); err != nil || !accepted {
		t.Fatalf("native report accepted=%v, err=%v", accepted, err)
	}
	projects, err = s.ProjectOverviews(ctx, time.UnixMilli(base-1))
	if err != nil || len(projects) != 1 || projects[0].Agents != 1 {
		t.Fatalf("unexpired native overview=%+v, err=%v", projects, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE amux_presence_clock SET observed_at=? WHERE id=1", base+presenceGoneAfter); err != nil {
		t.Fatal(err)
	}
	projects, err = s.ProjectOverviews(ctx, time.UnixMilli(base-1))
	if err != nil || len(projects) != 1 || projects[0].Agents != 0 {
		t.Fatalf("monotonically expired native overview=%+v, err=%v", projects, err)
	}
}
