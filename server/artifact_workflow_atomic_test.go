package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestArtifactVersionHTTPWriteCompletesResumedRunWithoutDroppingOutputs(t *testing.T) {
	store, handler := newArtifactWriteHTTPFixture(t, false)
	artifactID, _ := createArtifactHTTP(t, handler, "Resume output preservation", nil)
	actor, err := store.ResolveArtifactActor(context.Background(), storage.ArtifactActorIdentity{
		ActorType: "agent", AgentSystem: stringPointer("codex"), AgentID: stringPointer("agent-17"), DisplayName: "agent-17",
	})
	if err != nil {
		t.Fatal(err)
	}
	runWrite, err := store.StartWorkflowRun(context.Background(), storage.WorkflowRunInsert{
		ArtifactID: artifactID, WorkflowKind: "spec_task_generation", CoordinatorActorID: actor.ActorID, IsResumable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	reason := "interrupted"
	reasonValue := &reason
	state := "failed"
	contributions := []string{"contribution-existing"}
	versions := []string{"version-existing"}
	tasks := []string{"task-existing"}
	links := []string{"link-existing"}
	chunks := []string{"chunk-existing"}
	if _, err := store.UpdateWorkflowRun(context.Background(), runWrite.Record.WorkflowRunID, storage.WorkflowRunUpdate{
		State: &state, FailureReason: &reasonValue, GeneratedContributionIDs: &contributions,
		GeneratedVersionIDs: &versions, GeneratedTaskIDs: &tasks, GeneratedLinkIDs: &links, GeneratedChunkIDs: &chunks,
	}); err != nil {
		t.Fatal(err)
	}
	response := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/artifacts/"+artifactID+"/versions", map[string]string{
		"body": "recovered output",
	}, map[string]string{"X-Workflow-Run-ID": runWrite.Record.WorkflowRunID, "Idempotency-Key": "resume-version"})
	if response.Code != http.StatusCreated {
		t.Fatalf("resumed version write status=%d body=%s", response.Code, response.Body.String())
	}
	versionID := artifactWriteJSON(t, response)["data"].(map[string]any)["artifact_version_id"].(string)
	run, err := store.WorkflowRun(context.Background(), runWrite.Record.WorkflowRunID)
	if err != nil || run.State != "succeeded" || run.FailureReason != nil {
		t.Fatalf("resumed run did not complete atomically: %+v %v", run, err)
	}
	if !equalStrings(run.GeneratedContributionIDs, []string{"contribution-existing"}) ||
		!equalStrings(run.GeneratedVersionIDs, []string{"version-existing", versionID}) ||
		!equalStrings(run.GeneratedTaskIDs, tasks) || !equalStrings(run.GeneratedLinkIDs, links) || !equalStrings(run.GeneratedChunkIDs, chunks) {
		t.Fatalf("resumed run output history was lost or duplicated: %+v", run)
	}
}

func TestArtifactContributionHTTPWriteUsesTransactionBoundWorkflowHelper(t *testing.T) {
	store, handler := newArtifactWriteHTTPFixture(t, false)
	artifactID, _ := createArtifactHTTP(t, handler, "Contribution workflow", nil)
	versionResponse := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/artifacts/"+artifactID+"/versions", map[string]string{"body": "source"}, nil)
	if versionResponse.Code != http.StatusCreated {
		t.Fatalf("source version status=%d body=%s", versionResponse.Code, versionResponse.Body.String())
	}
	versionID := artifactWriteJSON(t, versionResponse)["data"].(map[string]any)["artifact_version_id"].(string)
	actor, err := store.ResolveArtifactActor(context.Background(), storage.ArtifactActorIdentity{
		ActorType: "agent", AgentSystem: stringPointer("codex"), AgentID: stringPointer("agent-17"), DisplayName: "agent-17",
	})
	if err != nil {
		t.Fatal(err)
	}
	runWrite, err := store.StartWorkflowRun(context.Background(), storage.WorkflowRunInsert{
		ArtifactID: artifactID, WorkflowKind: "spec_iteration", CoordinatorActorID: actor.ActorID, IsResumable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	reason, state := "interrupted", "failed"
	reasonValue := &reason
	previous := []string{"contribution-existing"}
	if _, err := store.UpdateWorkflowRun(context.Background(), runWrite.Record.WorkflowRunID, storage.WorkflowRunUpdate{
		State: &state, FailureReason: &reasonValue, GeneratedContributionIDs: &previous,
	}); err != nil {
		t.Fatal(err)
	}
	response := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/artifacts/"+artifactID+"/contributions", map[string]any{
		"target_kind": "artifact_version", "target_id": versionID, "contribution_kind": "review", "role": "reviewer", "body": "resumed review",
	}, map[string]string{"X-Workflow-Run-ID": runWrite.Record.WorkflowRunID, "Idempotency-Key": "resume-contribution"})
	if response.Code != http.StatusCreated {
		t.Fatalf("resumed contribution status=%d body=%s", response.Code, response.Body.String())
	}
	contributionID := artifactWriteJSON(t, response)["data"].(map[string]any)["contribution_id"].(string)
	run, err := store.WorkflowRun(context.Background(), runWrite.Record.WorkflowRunID)
	if err != nil || run.State != "succeeded" || !equalStrings(run.GeneratedContributionIDs, []string{"contribution-existing", contributionID}) {
		t.Fatalf("contribution workflow output was not committed with the contribution: %+v %v", run, err)
	}
}

func TestArtifactVersionHTTPWriteRacesCancellationAtomically(t *testing.T) {
	for i := 0; i < 8; i++ {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			store, handler := newArtifactWriteHTTPFixture(t, false)
			artifactID, _ := createArtifactHTTP(t, handler, "Workflow cancellation race", nil)
			actor, err := store.ResolveArtifactActor(context.Background(), storage.ArtifactActorIdentity{
				ActorType: "agent", AgentSystem: stringPointer("codex"), AgentID: stringPointer("agent-17"), DisplayName: "agent-17",
			})
			if err != nil {
				t.Fatal(err)
			}
			runWrite, err := store.StartWorkflowRun(context.Background(), storage.WorkflowRunInsert{
				ArtifactID: artifactID, WorkflowKind: "spec_task_generation", CoordinatorActorID: actor.ActorID, IsResumable: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			reason, state := "interrupted", "failed"
			reasonValue := &reason
			if _, err := store.UpdateWorkflowRun(context.Background(), runWrite.Record.WorkflowRunID, storage.WorkflowRunUpdate{
				State: &state, FailureReason: &reasonValue,
			}); err != nil {
				t.Fatal(err)
			}

			type result struct {
				responseCode int
				body         string
			}
			start := make(chan struct{})
			results := make(chan result, 2)
			var group sync.WaitGroup
			group.Add(2)
			go func() {
				defer group.Done()
				<-start
				response := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/artifacts/"+artifactID+"/versions", map[string]string{"body": "race output"}, map[string]string{
					"X-Workflow-Run-ID": runWrite.Record.WorkflowRunID, "Idempotency-Key": "race-version",
				})
				results <- result{response.Code, response.Body.String()}
			}()
			go func() {
				defer group.Done()
				<-start
				response := artifactWriteRequest(handler, http.MethodPatch, "/v1/projects/write-demo/artifacts/"+artifactID+"/workflow-runs/"+runWrite.Record.WorkflowRunID, map[string]string{"state": "cancelled"}, map[string]string{
					"Idempotency-Key": "race-cancel",
				})
				results <- result{response.Code, response.Body.String()}
			}()
			close(start)
			group.Wait()
			close(results)
			var writeResult, cancelResult result
			for got := range results {
				if got.responseCode == http.StatusCreated || got.responseCode == http.StatusBadRequest && strings.Contains(got.body, "cancelled workflow_run") {
					writeResult = got
				} else {
					cancelResult = got
				}
			}
			run, err := store.WorkflowRun(context.Background(), runWrite.Record.WorkflowRunID)
			if err != nil {
				t.Fatal(err)
			}
			versions, err := store.ArtifactVersions(context.Background(), "write-demo", artifactID)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case writeResult.responseCode == http.StatusCreated:
				if run.State != "succeeded" || cancelResult.responseCode != http.StatusBadRequest || len(versions) != 1 || len(run.GeneratedVersionIDs) != 1 || run.GeneratedVersionIDs[0] != versions[0].ArtifactVersionID {
					t.Fatalf("write won without committing its version and run output together: run=%+v versions=%+v cancel=%+v", run, versions, cancelResult)
				}
			case writeResult.responseCode == http.StatusBadRequest:
				if run.State != "cancelled" || cancelResult.responseCode != http.StatusOK || len(versions) != 0 {
					t.Fatalf("cancellation won but version mutation leaked: run=%+v versions=%+v write=%+v cancel=%+v", run, versions, writeResult, cancelResult)
				}
			default:
				t.Fatalf("unexpected workflow write result: write=%+v cancel=%+v", writeResult, cancelResult)
			}
		})
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
