package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProjectInventoryExcludesArchivedGlobalAndExpiredAgents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"zeta", "alpha", "archived", "__global__"} {
		if err := s.InsertProject(ctx, fixtureProject(id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetProjectArchived(ctx, "archived", true); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	activity := now.Add(-2 * time.Minute).UnixMilli()
	owner := func() AgentOrigin {
		return AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	}
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "expired", Sequence: 1, Agents: []PresenceAgent{{ProjectIdent: "zeta", Origin: owner(), LastActivityAt: &activity}}}, now.Add(-91*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "live", Sequence: 1, Agents: []PresenceAgent{{ProjectIdent: "zeta", Origin: owner(), Busy: true}, {ProjectIdent: "zeta", Origin: owner()}}}, now); err != nil {
		t.Fatal(err)
	}
	projects, err := s.ProjectOverviews(ctx, now)
	if err != nil || len(projects) != 2 || projects[0].Ident != "alpha" || projects[1].Ident != "zeta" || projects[0].Agents != 0 || projects[1].Agents != 2 {
		t.Fatalf("inventory %+v %v", projects, err)
	}
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "live", Sequence: 2}, now); err != nil {
		t.Fatal(err)
	}
	projects, err = s.ProjectOverviews(ctx, now)
	if err != nil || projects[1].Agents != 0 {
		t.Fatalf("gone agent counted: %+v %v", projects, err)
	}
}
