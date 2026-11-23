package storage

import (
	"context"
	"strings"
	"testing"
)

func TestUpdateDesignReviewStateRejectsInvalidEnumsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if err := store.InsertProject(ctx, fixtureProject("review-state")); err != nil {
		t.Fatal(err)
	}
	actor, err := store.ResolveArtifactActor(ctx, ArtifactActorIdentity{
		ActorType: "agent", AgentSystem: stringPointer("codex"), AgentID: stringPointer("state-agent"), DisplayName: "state-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CreateArtifact(ctx, ArtifactInsert{
		ProjectIdent: "review-state", Kind: "design_review", Title: "State review", CreatedByActorID: actor.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartWorkflowRun(ctx, WorkflowRunInsert{
		ArtifactID: artifact.ArtifactID, WorkflowKind: "design_review_round", CoordinatorActorID: actor.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherArtifact, err := store.CreateArtifact(ctx, ArtifactInsert{
		ProjectIdent: "review-state", Kind: "design_review", Title: "Other review", CreatedByActorID: actor.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherRun, err := store.StartWorkflowRun(ctx, WorkflowRunInsert{
		ArtifactID: otherArtifact.ArtifactID, WorkflowKind: "design_review_round", CoordinatorActorID: actor.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	idempotencyKey := "enum-rejection"
	invalid := "not-a-review-state"
	if _, err := store.UpdateDesignReviewState(ctx, "review-state", artifact.ArtifactID, nil, &invalid, actor.ActorID, "invalid", nil, idempotencyKey); err == nil || !strings.Contains(err.Error(), "invalid artifact review state") {
		t.Fatalf("invalid review state error=%v", err)
	}
	lifecycle := "not-a-lifecycle-state"
	if _, err := store.UpdateDesignReviewState(ctx, "review-state", artifact.ArtifactID, &lifecycle, nil, actor.ActorID, "invalid", nil, idempotencyKey); err == nil || !strings.Contains(err.Error(), "invalid artifact lifecycle state") {
		t.Fatalf("invalid lifecycle state error=%v", err)
	}
	validState := "accepted"
	otherRunID := otherRun.Record.WorkflowRunID
	if _, err := store.UpdateDesignReviewState(ctx, "review-state", artifact.ArtifactID, nil, &validState, actor.ActorID, "wrong round", &otherRunID, "wrong-round"); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("cross-artifact workflow run error=%v", err)
	}
	noContributions, err := store.ArtifactContributions(ctx, "review-state", artifact.ArtifactID)
	if err != nil || len(noContributions) != 0 {
		t.Fatalf("rejected writes left contributions=%+v err=%v", noContributions, err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_design_review_state
		BEFORE UPDATE OF review_state ON artifacts
		WHEN NEW.review_state='needs_user_decision'
		BEGIN SELECT RAISE(ABORT,'synthetic state update failure'); END`); err != nil {
		t.Fatal(err)
	}
	workflowRunID := run.Record.WorkflowRunID
	reviewState := "needs_user_decision"
	rollbackKey := "state-rollback"
	if _, err := store.UpdateDesignReviewState(ctx, "review-state", artifact.ArtifactID, nil, &reviewState, actor.ActorID, "Decision", &workflowRunID, rollbackKey); err == nil || !strings.Contains(err.Error(), "synthetic state update failure") {
		t.Fatalf("expected injected state update failure, got %v", err)
	}
	contributions, err := store.ArtifactContributions(ctx, "review-state", artifact.ArtifactID)
	if err != nil || len(contributions) != 0 {
		t.Fatalf("failed state transition left contributions=%+v err=%v", contributions, err)
	}
	current, err := store.Artifact(ctx, "review-state", artifact.ArtifactID)
	if err != nil || current.ReviewState != "none" {
		t.Fatalf("failed state transition changed artifact=%+v err=%v", current, err)
	}
	currentRun, err := store.WorkflowRun(ctx, workflowRunID)
	if err != nil || len(currentRun.GeneratedContributionIDs) != 0 {
		t.Fatalf("failed state transition changed workflow=%+v err=%v", currentRun, err)
	}
}
