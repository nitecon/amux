package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDesignReviewContributionAndRunOutputRollbackAndRetryTogether(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if err := store.InsertProject(ctx, fixtureProject("review-atomic")); err != nil {
		t.Fatal(err)
	}
	actor, err := store.ResolveArtifactActor(ctx, ArtifactActorIdentity{
		ActorType: "agent", AgentSystem: stringPointer("codex"), AgentID: stringPointer("reviewer"), DisplayName: "Reviewer",
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CreateArtifact(ctx, ArtifactInsert{
		ProjectIdent: "review-atomic", Kind: "design_review", Title: "Review", CreatedByActorID: actor.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, versionState := "source", "under_review"
	version, err := store.CreateArtifactVersion(ctx, "review-atomic", artifact.ArtifactID, ArtifactVersionInsert{
		BodyFormat: "markdown", Body: &body, CreatedByActorID: actor.ActorID, VersionState: versionState,
	})
	if err != nil {
		t.Fatal(err)
	}
	phase, runPhase := "pass_1", "pass_1"
	workflow, err := store.StartWorkflowRun(ctx, WorkflowRunInsert{
		ArtifactID: artifact.ArtifactID, WorkflowKind: "design_review_round", Phase: &runPhase,
		CoordinatorActorID: actor.ActorID, SourceArtifactVersionID: &version.Record.ArtifactVersionID,
		IsResumable: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	idempotencyKey := "atomic-review-contribution"
	input := ArtifactContributionInsert{
		TargetKind: "artifact_version", TargetID: version.Record.ArtifactVersionID,
		ContributionKind: "review", Phase: &phase, Role: "reviewer", ActorID: actor.ActorID,
		ReadSet:    json.RawMessage(`{"versions":["` + version.Record.ArtifactVersionID + `"]}`),
		BodyFormat: "markdown", Body: "review body", IdempotencyKey: &idempotencyKey,
	}
	trigger := `CREATE TRIGGER fail_review_run_output BEFORE UPDATE OF generated_contribution_ids ON workflow_runs
WHEN OLD.workflow_run_id='` + workflow.Record.WorkflowRunID + `'
BEGIN SELECT RAISE(ABORT, 'synthetic output append failure'); END`
	if _, err := store.db.ExecContext(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddDesignReviewContributionWithWorkflow(ctx, "review-atomic", artifact.ArtifactID, workflow.Record.WorkflowRunID, input); err == nil || !strings.Contains(err.Error(), "synthetic output append failure") {
		t.Fatalf("expected output append failure, got %v", err)
	}
	contributions, err := store.ArtifactContributions(ctx, "review-atomic", artifact.ArtifactID)
	if err != nil || len(contributions) != 0 {
		t.Fatalf("contribution survived transaction rollback: items=%+v err=%v", contributions, err)
	}
	run, err := store.WorkflowRun(ctx, workflow.Record.WorkflowRunID)
	if err != nil || run == nil || len(run.GeneratedContributionIDs) != 0 {
		t.Fatalf("workflow output changed despite rollback: run=%+v err=%v", run, err)
	}
	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER fail_review_run_output"); err != nil {
		t.Fatal(err)
	}

	created, replayed, err := store.AddDesignReviewContributionWithWorkflow(ctx, "review-atomic", artifact.ArtifactID, workflow.Record.WorkflowRunID, input)
	if err != nil || created == nil || replayed {
		t.Fatalf("retry after rollback failed: contribution=%+v replayed=%v err=%v", created, replayed, err)
	}
	run, err = store.WorkflowRun(ctx, workflow.Record.WorkflowRunID)
	if err != nil || run == nil || len(run.GeneratedContributionIDs) != 1 || run.GeneratedContributionIDs[0] != created.ContributionID {
		t.Fatalf("successful retry did not persist matching output: run=%+v err=%v", run, err)
	}
	again, replayed, err := store.AddDesignReviewContributionWithWorkflow(ctx, "review-atomic", artifact.ArtifactID, workflow.Record.WorkflowRunID, input)
	if err != nil || again == nil || !replayed || again.ContributionID != created.ContributionID {
		t.Fatalf("same-key retry changed contribution: contribution=%+v replayed=%v err=%v", again, replayed, err)
	}
	run, err = store.WorkflowRun(ctx, workflow.Record.WorkflowRunID)
	if err != nil || run == nil || len(run.GeneratedContributionIDs) != 1 || run.GeneratedContributionIDs[0] != created.ContributionID {
		t.Fatalf("same-key replay duplicated workflow output: run=%+v err=%v", run, err)
	}
}

func TestDesignReviewSynthesisRollsBackAllOutputsWhenLateStateWriteFails(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if err := store.InsertProject(ctx, fixtureProject("synthesis-atomic")); err != nil {
		t.Fatal(err)
	}
	actor, err := store.ResolveArtifactActor(ctx, ArtifactActorIdentity{
		ActorType: "agent", AgentSystem: stringPointer("codex"), AgentID: stringPointer("synthesizer"), DisplayName: "Synthesizer",
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CreateArtifact(ctx, ArtifactInsert{
		ProjectIdent: "synthesis-atomic", Kind: "design_review", Title: "Review", CreatedByActorID: actor.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceBody := "source"
	source, err := store.CreateArtifactVersion(ctx, "synthesis-atomic", artifact.ArtifactID, ArtifactVersionInsert{
		BodyFormat: "markdown", Body: &sourceBody, CreatedByActorID: actor.ActorID, VersionState: "under_review",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartWorkflowRun(ctx, WorkflowRunInsert{
		ArtifactID: artifact.ArtifactID, WorkflowKind: "design_review_round", CoordinatorActorID: actor.ActorID,
		SourceArtifactVersionID: &source.Record.ArtifactVersionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	passOne, _, err := store.AddDesignReviewContributionWithWorkflow(ctx, "synthesis-atomic", artifact.ArtifactID, run.Record.WorkflowRunID, ArtifactContributionInsert{
		TargetKind: "artifact_version", TargetID: source.Record.ArtifactVersionID, ContributionKind: "review", Phase: stringPointer("pass_1"),
		Role: "reviewer", ActorID: actor.ActorID, BodyFormat: "markdown", Body: "review", IdempotencyKey: stringPointer("seed-review"),
	})
	if err != nil || passOne == nil {
		t.Fatalf("seed review contribution: item=%+v err=%v", passOne, err)
	}
	readSet, err := json.Marshal(map[string][]string{"contributions": {passOne.ContributionID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_synthesis_state BEFORE UPDATE OF review_state ON artifacts
WHEN NEW.review_state='needs_user_decision'
BEGIN SELECT RAISE(ABORT, 'synthetic synthesis state failure'); END`); err != nil {
		t.Fatal(err)
	}
	input := DesignReviewSynthesisInsert{
		ReviewedVersionID: source.Record.ArtifactVersionID, ActorID: actor.ActorID, WorkflowRunID: run.Record.WorkflowRunID,
		IdempotencyKey: "synthetic-synthesis", ReadSet: readSet, BodyFormat: "markdown", Body: "synthesis",
		CreateVersion: true,
	}
	if _, err := store.CreateDesignReviewSynthesis(ctx, "synthesis-atomic", artifact.ArtifactID, input); err == nil || !strings.Contains(err.Error(), "synthetic synthesis state failure") {
		t.Fatalf("expected late synthesis state failure, got %v", err)
	}
	contributions, err := store.ArtifactContributions(ctx, "synthesis-atomic", artifact.ArtifactID)
	if err != nil || len(contributions) != 1 || contributions[0].ContributionID != passOne.ContributionID {
		t.Fatalf("synthesis contribution survived rollback: %+v err=%v", contributions, err)
	}
	versions, err := store.ArtifactVersions(ctx, "synthesis-atomic", artifact.ArtifactID)
	if err != nil || len(versions) != 1 || versions[0].ArtifactVersionID != source.Record.ArtifactVersionID {
		t.Fatalf("synthesis version survived rollback: %+v err=%v", versions, err)
	}
	current, err := store.Artifact(ctx, "synthesis-atomic", artifact.ArtifactID)
	if err != nil || current == nil || current.ReviewState != "none" || current.CurrentVersionID == nil || *current.CurrentVersionID != source.Record.ArtifactVersionID {
		t.Fatalf("artifact changed despite rollback: %+v err=%v", current, err)
	}
	storedRun, err := store.WorkflowRun(ctx, run.Record.WorkflowRunID)
	if err != nil || storedRun == nil || len(storedRun.GeneratedContributionIDs) != 1 || storedRun.GeneratedContributionIDs[0] != passOne.ContributionID || len(storedRun.GeneratedVersionIDs) != 0 {
		t.Fatalf("workflow outputs survived rollback: %+v err=%v", storedRun, err)
	}
	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER fail_synthesis_state"); err != nil {
		t.Fatal(err)
	}
	write, err := store.CreateDesignReviewSynthesis(ctx, "synthesis-atomic", artifact.ArtifactID, input)
	if err != nil || write == nil || write.Contribution == nil || write.Version == nil || write.Artifact == nil || write.Artifact.ReviewState != "needs_user_decision" {
		t.Fatalf("retry after rollback failed: write=%+v err=%v", write, err)
	}
	storedRun, err = store.WorkflowRun(ctx, run.Record.WorkflowRunID)
	if err != nil || storedRun == nil || len(storedRun.GeneratedContributionIDs) != 2 || storedRun.GeneratedContributionIDs[1] != write.Contribution.ContributionID || len(storedRun.GeneratedVersionIDs) != 1 || storedRun.GeneratedVersionIDs[0] != write.Version.ArtifactVersionID {
		t.Fatalf("successful retry did not persist both synthesis outputs: %+v err=%v", storedRun, err)
	}
}
