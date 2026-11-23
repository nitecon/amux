package storage

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func workflowRunTestArtifact(t *testing.T, s *Store, project, title string, actor *ArtifactActor) *Artifact {
	t.Helper()
	if err := s.InsertProject(context.Background(), fixtureProject(project)); err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CreateArtifact(context.Background(), ArtifactInsert{ProjectIdent: project, Kind: "spec", Title: title, CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func TestWorkflowRunStartReplayUsesRustOperationTupleAndSourceScope(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", DisplayName: "Coordinator"})
	if err != nil {
		t.Fatal(err)
	}
	artifact := workflowRunTestArtifact(t, s, "workflow-runs", "Spec", actor)
	otherArtifact, err := s.CreateArtifact(ctx, ArtifactInsert{ProjectIdent: "workflow-runs", Kind: "spec", Title: "Other spec", CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	body := "source"
	versionWrite, err := s.CreateArtifactVersion(ctx, "workflow-runs", artifact.ArtifactID, ArtifactVersionInsert{Body: &body, CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	otherVersion, err := s.CreateArtifactVersion(ctx, "workflow-runs", otherArtifact.ArtifactID, ArtifactVersionInsert{Body: &body, CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	key := "start-key"
	readSet := json.RawMessage(`{"artifact_version":["source"]}`)
	input := WorkflowRunInsert{
		ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_iteration", CoordinatorActorID: actor.ActorID,
		ParticipantActorIDs: []string{"agent-a"}, SourceArtifactVersionID: &versionWrite.Record.ArtifactVersionID,
		ReadSet: readSet, IdempotencyKey: &key, IsResumable: true,
	}
	first, err := s.StartWorkflowRun(ctx, input)
	if err != nil || first == nil || first.Replayed {
		t.Fatalf("initial run: %+v %v", first, err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO workflow_runs(workflow_run_id,artifact_id,workflow_kind,coordinator_actor_id,idempotency_key,started_at)
VALUES('duplicate-run',?,?,?,?,1)`, input.ArtifactID, input.WorkflowKind, input.CoordinatorActorID, key); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("duplicate workflow operation bypassed the unique index: %v", err)
	}
	changed := input
	changed.Phase = workflowStringPtr("retry")
	changed.SourceArtifactVersionID = &otherVersion.Record.ArtifactVersionID
	if _, err := s.StartWorkflowRun(ctx, changed); err == nil || !strings.Contains(err.Error(), "source artifact version does not belong") {
		t.Fatalf("foreign source version accepted before replay lookup: %v", err)
	}
	changed.SourceArtifactVersionID = input.SourceArtifactVersionID
	replay, err := s.StartWorkflowRun(ctx, changed)
	if err != nil || replay == nil || !replay.Replayed || replay.Record.WorkflowRunID != first.Record.WorkflowRunID || replay.Record.Phase != nil {
		t.Fatalf("same coordinator/artifact/kind/key did not replay original: %+v %v", replay, err)
	}
	otherCoordinator, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", AgentID: workflowStringPtr("different-coordinator"), DisplayName: "Other coordinator"})
	if err != nil {
		t.Fatal(err)
	}
	otherRun, err := s.StartWorkflowRun(ctx, WorkflowRunInsert{
		ArtifactID: artifact.ArtifactID, WorkflowKind: input.WorkflowKind, CoordinatorActorID: otherCoordinator.ActorID, IdempotencyKey: &key,
	})
	if err != nil || otherRun == nil || otherRun.Replayed || otherRun.Record.WorkflowRunID == first.Record.WorkflowRunID {
		t.Fatalf("coordinator was missing from operation key: %+v %v", otherRun, err)
	}
	list, err := s.WorkflowRuns(ctx, "workflow-runs", artifact.ArtifactID)
	if err != nil || len(list) != 2 {
		t.Fatalf("artifact workflow run list: %+v %v", list, err)
	}
	if foreign, err := s.WorkflowRuns(ctx, "elsewhere", artifact.ArtifactID); err == nil || foreign != nil {
		t.Fatalf("cross-project run list accepted: %+v %v", foreign, err)
	}
}

func TestWorkflowRunRetryValidationAndResumedCompletion(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", DisplayName: "Coordinator"})
	if err != nil {
		t.Fatal(err)
	}
	artifact := workflowRunTestArtifact(t, s, "workflow-resume", "Spec", actor)
	runWrite, err := s.StartWorkflowRun(ctx, WorkflowRunInsert{ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_task_generation", CoordinatorActorID: actor.ActorID, IsResumable: true})
	if err != nil {
		t.Fatal(err)
	}
	run := runWrite.Record
	failed, reason := "failed", "partial output"
	reasonValue := &reason
	tasks, chunks := []string{"task-existing"}, []string{"chunk-existing"}
	failedRun, err := s.UpdateWorkflowRun(ctx, run.WorkflowRunID, WorkflowRunUpdate{State: &failed, FailureReason: &reasonValue, GeneratedTaskIDs: &tasks, GeneratedChunkIDs: &chunks})
	if err != nil || failedRun.State != "failed" || failedRun.EndedAt == nil {
		t.Fatalf("failed state transition: %+v %v", failedRun, err)
	}
	validated, err := s.ValidateWorkflowRunForMutation(ctx, artifact.ArtifactID, &run.WorkflowRunID)
	if err != nil || validated == nil || validated.State != "failed" {
		t.Fatalf("resumable failed run was rejected: %+v %v", validated, err)
	}
	contribution, version, link := "contribution-1", "version-1", "link-1"
	if err := s.CompleteResumedWorkflowRun(ctx, validated, &contribution, &version, &link); err != nil {
		t.Fatal(err)
	}
	completed, err := s.WorkflowRun(ctx, run.WorkflowRunID)
	if err != nil || completed.State != "succeeded" || completed.FailureReason != nil || completed.EndedAt == nil || len(completed.GeneratedContributionIDs) != 1 || len(completed.GeneratedVersionIDs) != 1 || len(completed.GeneratedLinkIDs) != 1 || len(completed.GeneratedTaskIDs) != 1 || len(completed.GeneratedChunkIDs) != 1 {
		t.Fatalf("resumed run completion lost outputs: %+v %v", completed, err)
	}
	if err := s.CompleteResumedWorkflowRun(ctx, validated, &contribution, &version, &link); err != nil {
		t.Fatalf("idempotent completion replay: %v", err)
	}
	completed, err = s.WorkflowRun(ctx, run.WorkflowRunID)
	if err != nil || len(completed.GeneratedContributionIDs) != 1 || len(completed.GeneratedVersionIDs) != 1 || len(completed.GeneratedLinkIDs) != 1 {
		t.Fatalf("completion duplicated output IDs: %+v %v", completed, err)
	}
	if validated, err = s.ValidateWorkflowRunForMutation(ctx, artifact.ArtifactID, &run.WorkflowRunID); err != nil || validated == nil || validated.State != "succeeded" {
		t.Fatalf("succeeded run should remain retryable for idempotent mutations: %+v %v", validated, err)
	}
	if absent, err := s.ValidateWorkflowRunForMutation(ctx, artifact.ArtifactID, nil); err != nil || absent != nil {
		t.Fatalf("missing run should be optional: %+v %v", absent, err)
	}
}

func TestWorkflowRunTerminalAndNonresumableGuards(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", DisplayName: "Coordinator"})
	if err != nil {
		t.Fatal(err)
	}
	artifact := workflowRunTestArtifact(t, s, "workflow-terminal", "Spec", actor)
	started, err := s.StartWorkflowRun(ctx, WorkflowRunInsert{ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_iteration", CoordinatorActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	failed, reason := "failed", "not resumable"
	reasonValue := &reason
	failedRun, err := s.UpdateWorkflowRun(ctx, started.Record.WorkflowRunID, WorkflowRunUpdate{State: &failed, FailureReason: &reasonValue})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ValidateWorkflowRunForMutation(ctx, artifact.ArtifactID, &failedRun.WorkflowRunID); err == nil || !strings.Contains(err.Error(), "non-resumable failed") {
		t.Fatalf("non-resumable failed run was accepted: %v", err)
	}
	succeeded := "succeeded"
	if _, err := s.UpdateWorkflowRun(ctx, failedRun.WorkflowRunID, WorkflowRunUpdate{State: &succeeded}); err == nil || !strings.Contains(err.Error(), "invalid workflow_run state transition") {
		t.Fatalf("non-resumable failed run recovered: %v", err)
	}
	started, err = s.StartWorkflowRun(ctx, WorkflowRunInsert{ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_task_generation", CoordinatorActorID: actor.ActorID, IsResumable: true})
	if err != nil {
		t.Fatal(err)
	}
	cancelled := "cancelled"
	cancelledRun, err := s.UpdateWorkflowRun(ctx, started.Record.WorkflowRunID, WorkflowRunUpdate{State: &cancelled})
	if err != nil || cancelledRun.EndedAt == nil {
		t.Fatalf("cancelled transition: %+v %v", cancelledRun, err)
	}
	if _, err := s.ValidateWorkflowRunForMutation(ctx, artifact.ArtifactID, &cancelledRun.WorkflowRunID); err == nil || !strings.Contains(err.Error(), "cancelled workflow_run") {
		t.Fatalf("cancelled run was accepted: %v", err)
	}
	if _, err := s.UpdateWorkflowRun(ctx, cancelledRun.WorkflowRunID, WorkflowRunUpdate{State: &succeeded}); err == nil || !strings.Contains(err.Error(), "invalid workflow_run state transition") {
		t.Fatalf("cancelled run was reopened: %v", err)
	}
	if _, err := s.ValidateWorkflowRunForMutation(ctx, "different-artifact", &failedRun.WorkflowRunID); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("foreign artifact run accepted: %v", err)
	}
}

func TestWorkflowRunAppendOutputsIsIdempotentAndReadSetIsBounded(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", DisplayName: "Coordinator"})
	if err != nil {
		t.Fatal(err)
	}
	artifact := workflowRunTestArtifact(t, s, "workflow-outputs", "Spec", actor)
	tooMany := make([]string, workflowRunReadSetMaxRefs+1)
	for i := range tooMany {
		tooMany[i] = "ref"
	}
	encoded, err := json.Marshal(tooMany)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartWorkflowRun(ctx, WorkflowRunInsert{ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_iteration", CoordinatorActorID: actor.ActorID, ReadSet: encoded}); err == nil || !strings.Contains(err.Error(), "read_set_too_large") {
		t.Fatalf("oversized read set accepted: %v", err)
	}
	run, err := s.StartWorkflowRun(ctx, WorkflowRunInsert{ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_iteration", CoordinatorActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	contribution, version := "contribution", "version"
	for range 2 {
		if _, err := s.AppendWorkflowRunOutputs(ctx, run.Record.WorkflowRunID, &contribution, &version); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := s.WorkflowRun(ctx, run.Record.WorkflowRunID)
	if err != nil || len(stored.GeneratedContributionIDs) != 1 || len(stored.GeneratedVersionIDs) != 1 {
		t.Fatalf("appended outputs were duplicated: %+v %v", stored, err)
	}
}

func TestWorkflowRunConcurrentStartReplayAndOutputAppend(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", DisplayName: "Coordinator"})
	if err != nil {
		t.Fatal(err)
	}
	artifact := workflowRunTestArtifact(t, s, "workflow-concurrent", "Spec", actor)
	key := "concurrent-start"
	input := WorkflowRunInsert{ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_iteration", CoordinatorActorID: actor.ActorID, IdempotencyKey: &key}
	const workers = 12
	start := make(chan struct{})
	results := make(chan *WorkflowRunWrite, workers)
	errors := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			result, err := s.StartWorkflowRun(ctx, input)
			if err != nil {
				errors <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Errorf("concurrent start: %v", err)
	}
	var runID string
	created := 0
	for result := range results {
		if result == nil || result.Record == nil {
			t.Fatal("concurrent start returned no run")
		}
		if runID == "" {
			runID = result.Record.WorkflowRunID
		}
		if result.Record.WorkflowRunID != runID {
			t.Fatalf("same start key created multiple runs: %s and %s", runID, result.Record.WorkflowRunID)
		}
		if !result.Replayed {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("new runs for one operation key=%d, want 1", created)
	}

	const outputs = 10
	start = make(chan struct{})
	errors = make(chan error, outputs)
	for i := range outputs {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			value := "contribution-" + string(rune('a'+index))
			_, err := s.AppendWorkflowRunOutputs(ctx, runID, &value, nil)
			if err != nil {
				errors <- err
			}
		}(i)
	}
	close(start)
	group.Wait()
	close(errors)
	for err := range errors {
		t.Errorf("concurrent append: %v", err)
	}
	stored, err := s.WorkflowRun(ctx, runID)
	if err != nil || len(stored.GeneratedContributionIDs) != outputs {
		t.Fatalf("concurrent output append lost values: %+v %v", stored, err)
	}
}

func workflowStringPtr(value string) *string { return &value }
