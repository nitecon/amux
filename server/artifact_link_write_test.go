package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestArtifactLinkRouteSupportsSameProjectAndExternalLinks(t *testing.T) {
	store, handler := newArtifactWriteHTTPFixture(t, true)
	artifactID, _ := createArtifactHTTP(t, handler, "link source", map[string]string{"X-Agent-Project": "write-demo", "X-Agent-Scopes": "artifact.write"})
	versionResponse := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/artifacts/"+artifactID+"/versions", map[string]any{"body": "spec"}, map[string]string{"X-Agent-Project": "write-demo", "X-Agent-Scopes": "artifact_version.create"})
	if versionResponse.Code != http.StatusCreated {
		t.Fatalf("version status=%d body=%s", versionResponse.Code, versionResponse.Body.String())
	}
	versionID := artifactWriteJSON(t, versionResponse)["data"].(map[string]any)["artifact_version_id"].(string)
	run, err := store.StartWorkflowRun(t.Context(), storage.WorkflowRunInsert{ArtifactID: artifactID, WorkflowKind: "spec_task_generation", CoordinatorActorID: mustArtifactCreator(t, store, artifactID), IsResumable: true})
	if err != nil {
		t.Fatal(err)
	}
	failedState := "failed"
	failureReason := "resumed test run"
	failureReasonPtr := &failureReason
	if _, err := store.UpdateWorkflowRun(t.Context(), run.Record.WorkflowRunID, storage.WorkflowRunUpdate{State: &failedState, FailureReason: &failureReasonPtr}); err != nil {
		t.Fatal(err)
	}
	projectBody := func(targetKind, targetID string) map[string]any {
		return map[string]any{"link_type": "doc_referenced_by_spec", "source_kind": "artifact", "source_id": artifactID, "target_kind": targetKind, "target_id": targetID}
	}
	post := func(key string, body any, runID string) *httptest.ResponseRecorder {
		headers := map[string]string{"Idempotency-Key": key, "X-Agent-Project": "write-demo", "X-Agent-Scopes": "link.write"}
		if runID != "" {
			headers["X-Workflow-Run-ID"] = runID
		}
		return artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/artifacts/links", body, headers)
	}
	local := post("link-local", projectBody("artifact_version", versionID), run.Record.WorkflowRunID)
	if local.Code != http.StatusCreated {
		t.Fatalf("same-project link status=%d body=%s", local.Code, local.Body.String())
	}
	type linkResponse struct {
		Data       storage.ArtifactLink       `json:"data"`
		Provenance artifactMutationProvenance `json:"provenance"`
	}
	var localResult linkResponse
	if err := json.Unmarshal(local.Body.Bytes(), &localResult); err != nil {
		t.Fatal(err)
	}
	if localResult.Data.LinkID == "" || localResult.Data.SourceID != artifactID || localResult.Data.TargetKind != "artifact_version" || localResult.Data.TargetID != versionID || localResult.Provenance.Authorization.RequiredScopes[0] != "link.write" {
		t.Fatalf("same-project response %+v", localResult)
	}
	replay := post("link-local", projectBody("artifact_version", versionID), run.Record.WorkflowRunID)
	var retries linkResponse
	if replay.Code != http.StatusOK || json.Unmarshal(replay.Body.Bytes(), &retries) != nil ||
		!retries.Provenance.Replay || retries.Data.LinkID != localResult.Data.LinkID ||
		strings.Join(retries.Provenance.Authorization.RequiredScopes, ",") != "link.write" {
		t.Fatalf("same-project replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	completed, err := store.WorkflowRun(t.Context(), run.Record.WorkflowRunID)
	if err != nil || completed == nil || completed.State != "succeeded" || len(completed.GeneratedLinkIDs) != 1 || completed.GeneratedLinkIDs[0] != localResult.Data.LinkID {
		t.Fatalf("resumable run completion: %+v err=%v", completed, err)
	}
	external := post("link-external", projectBody("external_url", "https://example.test/spec"), "")
	if external.Code != http.StatusCreated {
		t.Fatalf("artifact-to-external link status=%d body=%s", external.Code, external.Body.String())
	}
}

func TestArtifactLinkRouteRejectsCrossProjectWritesAndReplay(t *testing.T) {
	store, handler := newArtifactWriteHTTPFixture(t, true)
	artifactA, _ := createArtifactHTTP(t, handler, "project A", map[string]string{"X-Agent-Project": "write-demo", "X-Agent-Scopes": "artifact.write"})
	if err := store.InsertProject(t.Context(), storage.Project{Ident: "project-b", ChannelName: "project-b", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	owner, err := store.ResolveArtifactActor(t.Context(), storage.ArtifactActorIdentity{ActorType: "agent", DisplayName: "foreign owner"})
	if err != nil {
		t.Fatal(err)
	}
	artifactB, err := store.CreateArtifact(t.Context(), storage.ArtifactInsert{ProjectIdent: "project-b", Kind: "spec", Title: "project B", CreatedByActorID: owner.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	versionB, err := store.CreateArtifactVersion(t.Context(), "project-b", artifactB.ArtifactID, storage.ArtifactVersionInsert{BodyFormat: "markdown", CreatedByActorID: owner.ActorID})
	if err != nil || versionB == nil || versionB.Record == nil {
		t.Fatalf("create foreign version: %+v err=%v", versionB, err)
	}
	workflow, err := store.StartWorkflowRun(t.Context(), storage.WorkflowRunInsert{ArtifactID: artifactA, WorkflowKind: "spec_task_generation", CoordinatorActorID: mustArtifactCreator(t, store, artifactA), IsResumable: true})
	if err != nil {
		t.Fatal(err)
	}
	base := storage.ArtifactLinkInsert{LinkType: "doc_referenced_by_spec", SourceKind: "artifact", SourceID: artifactB.ArtifactID, TargetKind: "external_url", TargetID: "https://example.test/b", CreatedByActorID: owner.ActorID, CreatedViaWorkflowRunID: &workflow.Record.WorkflowRunID, IdempotencyKey: stringPtr("cross-project-op")}
	if _, err := store.CreateArtifactLink(t.Context(), base); err != nil {
		t.Fatalf("seed foreign replay: %v", err)
	}
	body := map[string]any{"link_type": "doc_referenced_by_spec", "source_kind": "artifact", "source_id": artifactA, "target_kind": "artifact", "target_id": artifactB.ArtifactID}
	response := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/artifacts/links", body, map[string]string{"Idempotency-Key": "cross-project-write", "X-Workflow-Run-ID": workflow.Record.WorkflowRunID, "X-Agent-Project": "write-demo", "X-Agent-Scopes": "link.write"})
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "outside the requested project") {
		t.Fatalf("cross-project write status=%d body=%s", response.Code, response.Body.String())
	}
	versionAnchor := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/artifacts/links", map[string]any{"link_type": "doc_referenced_by_spec", "source_kind": "artifact", "source_id": artifactA, "target_kind": "external_url", "target_id": "https://example.test/a", "target_version_id": versionB.Record.ArtifactVersionID}, map[string]string{"Idempotency-Key": "cross-project-version", "X-Agent-Project": "write-demo", "X-Agent-Scopes": "link.write"})
	if versionAnchor.Code != http.StatusForbidden || !strings.Contains(versionAnchor.Body.String(), "outside the requested project") {
		t.Fatalf("cross-project version anchor status=%d body=%s", versionAnchor.Code, versionAnchor.Body.String())
	}
	foreignRun, err := store.StartWorkflowRun(t.Context(), storage.WorkflowRunInsert{ArtifactID: artifactB.ArtifactID, WorkflowKind: "spec_task_generation", CoordinatorActorID: owner.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	foreignWorkflow := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/artifacts/links", map[string]any{"link_type": "doc_referenced_by_spec", "source_kind": "artifact", "source_id": artifactA, "target_kind": "external_url", "target_id": "https://example.test/a"}, map[string]string{"Idempotency-Key": "cross-project-run", "X-Workflow-Run-ID": foreignRun.Record.WorkflowRunID, "X-Agent-Project": "write-demo", "X-Agent-Scopes": "link.write"})
	if foreignWorkflow.Code != http.StatusForbidden || !strings.Contains(foreignWorkflow.Body.String(), "outside the requested project") {
		t.Fatalf("cross-project workflow status=%d body=%s", foreignWorkflow.Code, foreignWorkflow.Body.String())
	}
	count, err := store.CountArtifactLinks(t.Context(), "project-b")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("cross-project write created a row: %d", count)
	}
	replayBody := map[string]any{"link_type": "doc_referenced_by_spec", "source_kind": "artifact", "source_id": artifactA, "target_kind": "external_url", "target_id": "https://example.test/a"}
	replay := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/artifacts/links", replayBody, map[string]string{"Idempotency-Key": "cross-project-op", "X-Workflow-Run-ID": workflow.Record.WorkflowRunID, "X-Agent-Project": "write-demo", "X-Agent-Scopes": "link.write"})
	if replay.Code != http.StatusForbidden || !strings.Contains(replay.Body.String(), "outside the requested project") {
		t.Fatalf("cross-project replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	if count, err = store.CountArtifactLinks(t.Context(), "project-b"); err != nil || count != 1 {
		t.Fatalf("replay mutated links: count=%d err=%v", count, err)
	}
}

func TestArtifactLinkRouteRejectsCancelledWorkflowRun(t *testing.T) {
	store, handler := newArtifactWriteHTTPFixture(t, true)
	artifactID, _ := createArtifactHTTP(t, handler, "cancelled link run", map[string]string{"X-Agent-Project": "write-demo", "X-Agent-Scopes": "artifact.write"})
	run, err := store.StartWorkflowRun(t.Context(), storage.WorkflowRunInsert{ArtifactID: artifactID, WorkflowKind: "spec_task_generation", CoordinatorActorID: mustArtifactCreator(t, store, artifactID)})
	if err != nil {
		t.Fatal(err)
	}
	cancelled := "cancelled"
	if _, err := store.UpdateWorkflowRun(t.Context(), run.Record.WorkflowRunID, storage.WorkflowRunUpdate{State: &cancelled}); err != nil {
		t.Fatal(err)
	}
	response := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/artifacts/links", map[string]any{"link_type": "doc_referenced_by_spec", "source_kind": "artifact", "source_id": artifactID, "target_kind": "external_url", "target_id": "https://example.test/spec"}, map[string]string{"Idempotency-Key": "cancelled-link", "X-Workflow-Run-ID": run.Record.WorkflowRunID, "X-Agent-Project": "write-demo", "X-Agent-Scopes": "link.write"})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "cancelled workflow_run is not retryable") {
		t.Fatalf("cancelled run status=%d body=%s", response.Code, response.Body.String())
	}
	count, err := store.CountArtifactLinks(t.Context(), "write-demo")
	if err != nil || count != 0 {
		t.Fatalf("cancelled run wrote links: count=%d err=%v", count, err)
	}
}

func mustArtifactCreator(t *testing.T, store *storage.Store, artifactID string) string {
	t.Helper()
	artifact, err := store.Artifact(t.Context(), "write-demo", artifactID)
	if err != nil || artifact == nil {
		t.Fatalf("artifact lookup: %+v %v", artifact, err)
	}
	return artifact.CreatedByActorID
}
