package storage

import (
	"context"
	"testing"
)

func TestAcceptArtifactVersionCommitsStatePointersAndAuditTogether(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artifact, first, second := artifactChunkFixture(t, s, "accept-spec")
	workflow, err := s.StartWorkflowRun(ctx, WorkflowRunInsert{
		ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_acceptance", CoordinatorActorID: artifact.CreatedByActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := "accept-once"
	contribution, err := s.AcceptArtifactVersion(ctx, "ACCEPT-SPEC", artifact.ArtifactID, first.ArtifactVersionID, artifact.CreatedByActorID, &workflow.Record.WorkflowRunID, &key)
	if err != nil {
		t.Fatal(err)
	}
	if contribution == nil || contribution.TargetKind != "artifact_version" || contribution.TargetID != first.ArtifactVersionID || contribution.ContributionKind != "state_transition" || contribution.Phase == nil || *contribution.Phase != "acceptance" || contribution.Role != "coordinator" || contribution.Body != "accepted artifact version" || contribution.WorkflowRunID == nil || *contribution.WorkflowRunID != workflow.Record.WorkflowRunID {
		t.Fatalf("acceptance contribution mismatch: %+v", contribution)
	}
	accepted, err := s.Artifact(ctx, "accept-spec", artifact.ArtifactID)
	if err != nil || accepted == nil || accepted.CurrentVersionID == nil || *accepted.CurrentVersionID != first.ArtifactVersionID || accepted.AcceptedVersionID == nil || *accepted.AcceptedVersionID != first.ArtifactVersionID {
		t.Fatalf("accepted artifact pointers: %+v %v", accepted, err)
	}
	acceptedVersion, err := s.ArtifactVersion(ctx, "accept-spec", artifact.ArtifactID, first.ArtifactVersionID)
	if err != nil || acceptedVersion == nil || acceptedVersion.VersionState != "accepted" {
		t.Fatalf("accepted version state: %+v %v", acceptedVersion, err)
	}
	otherVersion, err := s.ArtifactVersion(ctx, "accept-spec", artifact.ArtifactID, second.ArtifactVersionID)
	if err != nil || otherVersion == nil || otherVersion.VersionState != "draft" {
		t.Fatalf("unselected version changed: %+v %v", otherVersion, err)
	}
	replayed, err := s.AcceptArtifactVersion(ctx, "accept-spec", artifact.ArtifactID, first.ArtifactVersionID, artifact.CreatedByActorID, &workflow.Record.WorkflowRunID, &key)
	if err != nil || replayed == nil || replayed.ContributionID != contribution.ContributionID {
		t.Fatalf("acceptance replay: %+v err=%v", replayed, err)
	}
	newerKey := "accept-newer-once"
	newerContribution, err := s.AcceptArtifactVersion(ctx, "accept-spec", artifact.ArtifactID, second.ArtifactVersionID, artifact.CreatedByActorID, &workflow.Record.WorkflowRunID, &newerKey)
	if err != nil || newerContribution == nil || newerContribution.TargetID != second.ArtifactVersionID {
		t.Fatalf("accept newer version: %+v %v", newerContribution, err)
	}
	newer, err := s.Artifact(ctx, "accept-spec", artifact.ArtifactID)
	if err != nil || newer == nil || newer.CurrentVersionID == nil || *newer.CurrentVersionID != second.ArtifactVersionID || newer.AcceptedVersionID == nil || *newer.AcceptedVersionID != second.ArtifactVersionID {
		t.Fatalf("newer accepted pointers: %+v %v", newer, err)
	}
	staleReplay, err := s.AcceptArtifactVersion(ctx, "accept-spec", artifact.ArtifactID, first.ArtifactVersionID, artifact.CreatedByActorID, &workflow.Record.WorkflowRunID, &key)
	if err != nil || staleReplay == nil || staleReplay.ContributionID != contribution.ContributionID {
		t.Fatalf("stale acceptance replay: %+v %v", staleReplay, err)
	}
	unchanged, err := s.Artifact(ctx, "accept-spec", artifact.ArtifactID)
	if err != nil || unchanged == nil || unchanged.CurrentVersionID == nil || *unchanged.CurrentVersionID != second.ArtifactVersionID || unchanged.AcceptedVersionID == nil || *unchanged.AcceptedVersionID != second.ArtifactVersionID {
		t.Fatalf("stale exact replay regressed newer pointers: %+v %v", unchanged, err)
	}
	items, err := s.ArtifactContributions(ctx, "accept-spec", artifact.ArtifactID)
	seen := map[string]bool{}
	for _, item := range items {
		seen[item.ContributionID] = true
	}
	if err != nil || len(items) != 2 || !seen[contribution.ContributionID] || !seen[newerContribution.ContributionID] {
		t.Fatalf("acceptance replay audit rows: %+v %v", items, err)
	}
	if _, err := s.AcceptArtifactVersion(ctx, "accept-spec", artifact.ArtifactID, second.ArtifactVersionID, artifact.CreatedByActorID, &workflow.Record.WorkflowRunID, &key); err == nil {
		t.Fatal("reused acceptance key for another version")
	}
	unchanged, err = s.Artifact(ctx, "accept-spec", artifact.ArtifactID)
	if err != nil || unchanged == nil || unchanged.CurrentVersionID == nil || *unchanged.CurrentVersionID != second.ArtifactVersionID || unchanged.AcceptedVersionID == nil || *unchanged.AcceptedVersionID != second.ArtifactVersionID {
		t.Fatalf("conflicting replay changed pointers: %+v %v", unchanged, err)
	}
	secondAfter, err := s.ArtifactVersion(ctx, "accept-spec", artifact.ArtifactID, second.ArtifactVersionID)
	if err != nil || secondAfter == nil || secondAfter.VersionState != "accepted" {
		t.Fatalf("conflicting replay changed newer version: %+v %v", secondAfter, err)
	}
}

func TestAcceptArtifactVersionRejectsScopeKindStateAndWorkflow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artifact, version, _ := artifactChunkFixture(t, s, "accept-invalid")
	if _, err := s.AcceptArtifactVersion(ctx, "wrong-project", artifact.ArtifactID, version.ArtifactVersionID, artifact.CreatedByActorID, nil, nil); err == nil {
		t.Fatal("accepted artifact outside requested project")
	}
	other, _, _ := artifactChunkFixture(t, s, "accept-other")
	if _, err := s.AcceptArtifactVersion(ctx, "accept-invalid", other.ArtifactID, version.ArtifactVersionID, artifact.CreatedByActorID, nil, nil); err == nil {
		t.Fatal("accepted version not owned by artifact")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE artifact_versions SET version_state='rejected' WHERE artifact_version_id=?`, version.ArtifactVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptArtifactVersion(ctx, "accept-invalid", artifact.ArtifactID, version.ArtifactVersionID, artifact.CreatedByActorID, nil, nil); err == nil {
		t.Fatal("accepted rejected version")
	}
	secondArtifact, err := s.CreateArtifact(ctx, ArtifactInsert{ProjectIdent: "accept-invalid", Kind: "design_review", Title: "Not a spec", CreatedByActorID: artifact.CreatedByActorID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptArtifactVersion(ctx, "accept-invalid", secondArtifact.ArtifactID, version.ArtifactVersionID, artifact.CreatedByActorID, nil, nil); err == nil {
		t.Fatal("accepted a design_review artifact")
	}
	if _, err := s.AcceptArtifactVersion(ctx, "accept-invalid", artifact.ArtifactID, version.ArtifactVersionID, artifact.CreatedByActorID, artifactStringPtr("missing-run"), nil); err == nil {
		t.Fatal("accepted with a missing workflow run")
	}
}

func TestAcceptArtifactVersionRollsBackWhenAuditContributionFails(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artifact, first, second := artifactChunkFixture(t, s, "accept-rollback")
	if _, err := s.AcceptArtifactVersion(ctx, "accept-rollback", artifact.ArtifactID, first.ArtifactVersionID, "missing-actor", nil, nil); err == nil {
		t.Fatal("acceptance succeeded despite invalid audit actor")
	}
	after, err := s.Artifact(ctx, "accept-rollback", artifact.ArtifactID)
	if err != nil || after == nil || after.CurrentVersionID == nil || *after.CurrentVersionID != second.ArtifactVersionID || after.AcceptedVersionID != nil {
		t.Fatalf("failed acceptance changed pointers: %+v %v", after, err)
	}
	unaccepted, err := s.ArtifactVersion(ctx, "accept-rollback", artifact.ArtifactID, first.ArtifactVersionID)
	if err != nil || unaccepted == nil || unaccepted.VersionState != "draft" {
		t.Fatalf("failed acceptance changed version state: %+v %v", unaccepted, err)
	}
	contributions, err := s.ArtifactContributions(ctx, "accept-rollback", artifact.ArtifactID)
	if err != nil || len(contributions) != 0 {
		t.Fatalf("failed acceptance left audit rows: %+v %v", contributions, err)
	}
}
