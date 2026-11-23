package storage

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCheckArtifactRestoreReportsForeignPointerWithoutRepair(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := testStore(t)
	artifact, _, _ := artifactChunkFixture(t, s, "restore-pointers")
	foreign, err := s.CreateArtifact(ctx, ArtifactInsert{
		ProjectIdent: artifact.ProjectIdent, Kind: "spec", Title: "Foreign owner", CreatedByActorID: artifact.CreatedByActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := s.CreateArtifactVersion(ctx, foreign.ProjectIdent, foreign.ArtifactID, ArtifactVersionInsert{
		CreatedByActorID: artifact.CreatedByActorID, VersionState: "accepted",
	})
	if err != nil {
		t.Fatal(err)
	}
	versionID := version.Record.ArtifactVersionID
	if _, err := s.db.ExecContext(ctx, `UPDATE artifacts SET current_version_id=? WHERE artifact_id=?`, versionID, artifact.ArtifactID); err != nil {
		t.Fatal(err)
	}
	report, err := s.CheckArtifactRestore(ctx, 0, ArtifactRestoreCheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	findings := report.ArtifactPointerMismatches
	if len(findings) != 1 || findings[0].Tag != "restore:pointer_mismatch" || findings[0].EntityID != artifact.ArtifactID || !strings.Contains(findings[0].Detail, "belongs to artifact "+foreign.ArtifactID) {
		t.Fatalf("pointer findings: %+v", findings)
	}
	after, err := s.Artifact(ctx, artifact.ProjectIdent, artifact.ArtifactID)
	if err != nil || after == nil || after.CurrentVersionID == nil || *after.CurrentVersionID != versionID {
		t.Fatalf("checker changed the foreign pointer: %+v %v", after, err)
	}
}

func TestCheckArtifactRestorePopulatedStoreIsReadOnly(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.InsertProject(ctx, fixtureProject("restore-check")); err != nil {
		t.Fatal(err)
	}
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", DisplayName: "restore fixture"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CreateArtifact(ctx, ArtifactInsert{ProjectIdent: "restore-check", Kind: "spec", Title: "restore fixture", CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO workflow_runs(workflow_run_id,artifact_id,workflow_kind,coordinator_actor_id,state,started_at) VALUES('restore-stuck',?,?,?,'started',?)`, artifact.ArtifactID, "spec_task_generation", actor.ActorID, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.db.QueryContext(ctx, `SELECT workflow_run_id,state,started_at FROM workflow_runs`)
	if err != nil {
		t.Fatal(err)
	}
	var beforeID, beforeState string
	var beforeStarted int64
	if !before.Next() {
		t.Fatal("expected populated workflow run")
	}
	if err := before.Scan(&beforeID, &beforeState, &beforeStarted); err != nil {
		t.Fatal(err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}

	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	report, err := s.CheckArtifactRestore(checkCtx, 48*60*60*1000, ArtifactRestoreCheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.WorkflowRunInconsistencies) != 1 || report.WorkflowRunInconsistencies[0].Tag != "restore:stuck_workflow_run" {
		t.Fatalf("report %+v", report)
	}
	var afterID, afterState string
	var afterStarted int64
	if err := s.db.QueryRowContext(ctx, `SELECT workflow_run_id,state,started_at FROM workflow_runs`).Scan(&afterID, &afterState, &afterStarted); err != nil {
		t.Fatal(err)
	}
	if beforeID != afterID || beforeState != afterState || beforeStarted != afterStarted {
		t.Fatalf("checker modified state: before=(%s,%s,%d), after=(%s,%s,%d)", beforeID, beforeState, beforeStarted, afterID, afterState, afterStarted)
	}
}

func TestCheckArtifactRestoreCleanStore(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	report, err := s.CheckArtifactRestore(ctx, 123456789, ArtifactRestoreCheckOptions{})
	if err != nil || report == nil || !report.IsClean() {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestCheckArtifactRestoreIgnoresMalformedGeneratedIDs(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.InsertProject(ctx, fixtureProject("restore-json")); err != nil {
		t.Fatal(err)
	}
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", DisplayName: "restore fixture"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CreateArtifact(ctx, ArtifactInsert{ProjectIdent: "restore-json", Kind: "spec", Title: "restore fixture", CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO workflow_runs(workflow_run_id,artifact_id,workflow_kind,coordinator_actor_id,state,started_at,generated_version_ids,generated_link_ids) VALUES('restore-json-run',?,?,?,'succeeded',1,'not-json','["missing-link"]')`, artifact.ArtifactID, "spec_task_generation", actor.ActorID); err != nil {
		t.Fatal(err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	report, err := s.CheckArtifactRestore(checkCtx, 48*60*60*1000, ArtifactRestoreCheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.WorkflowRunInconsistencies) != 1 {
		t.Fatalf("workflow findings: %+v", report.WorkflowRunInconsistencies)
	}
	finding := report.WorkflowRunInconsistencies[0]
	if finding.Tag != "restore:dangling" || finding.EntityID != "restore-json-run" || finding.Detail != "generated link missing-link missing" {
		t.Fatalf("finding %+v", finding)
	}
}
