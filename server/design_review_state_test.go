package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func designReviewStateFixture(t *testing.T) (*storage.Store, http.Handler, string, string) {
	t.Helper()
	store, handler := newArtifactWriteHTTPFixture(t, true)
	ctx := context.Background()
	actor, err := store.ResolveArtifactActor(ctx, storage.ArtifactActorIdentity{
		ActorType: "agent", AgentSystem: stringPointer("codex"), AgentID: stringPointer("agent-17"), DisplayName: "agent-17",
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CreateArtifact(ctx, storage.ArtifactInsert{
		ProjectIdent: "write-demo", Kind: "design_review", Title: "Review state", CreatedByActorID: actor.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartWorkflowRun(ctx, storage.WorkflowRunInsert{
		ArtifactID: artifact.ArtifactID, WorkflowKind: "design_review_round", CoordinatorActorID: actor.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, handler, artifact.ArtifactID, run.Record.WorkflowRunID
}

func TestDesignReviewStateRouteAuditsAndReplaysAtomically(t *testing.T) {
	store, handler, artifactID, workflowRunID := designReviewStateFixture(t)
	path := "/v1/projects/write-demo/design-reviews/" + artifactID + "/state"
	headers := map[string]string{
		"X-Agent-Project": "write-demo", "X-Agent-Scopes": "artifact.write contribution.write",
		"X-Workflow-Run-ID": workflowRunID, "Idempotency-Key": "state-change",
	}
	denied := artifactWriteRequest(handler, http.MethodPost, path, map[string]any{"review_state": "accepted"}, map[string]string{
		"X-Agent-Project": "write-demo", "X-Agent-Scopes": "artifact.write", "Idempotency-Key": "state-denied",
	})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("missing contribution scope status=%d body=%s", denied.Code, denied.Body.String())
	}
	missing := artifactWriteRequest(handler, http.MethodPost, path, map[string]any{"note": "missing state"}, headers)
	if missing.Code != http.StatusBadRequest || !strings.Contains(missing.Body.String(), "state_required") {
		t.Fatalf("missing state status=%d body=%s", missing.Code, missing.Body.String())
	}
	for _, invalid := range []map[string]any{{"lifecycle_state": "banana"}, {"review_state": "banana"}} {
		response := artifactWriteRequest(handler, http.MethodPost, path, invalid, headers)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid artifact") {
			t.Fatalf("invalid state status=%d body=%s", response.Code, response.Body.String())
		}
	}
	contributions, err := store.ArtifactContributions(context.Background(), "write-demo", artifactID)
	if err != nil || len(contributions) != 0 {
		t.Fatalf("rejected requests wrote audit contributions=%+v err=%v", contributions, err)
	}
	body := map[string]any{"lifecycle_state": "active", "review_state": "accepted", "note": "Decision recorded"}
	first := artifactWriteRequest(handler, http.MethodPost, path, body, headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("state transition status=%d body=%s", first.Code, first.Body.String())
	}
	value := artifactWriteJSON(t, first)
	data := value["data"].(map[string]any)
	artifact := data["artifact"].(map[string]any)
	contribution := data["contribution"].(map[string]any)
	if artifact["lifecycle_state"] != "active" || artifact["review_state"] != "accepted" || contribution["contribution_kind"] != "state_transition" || contribution["target_kind"] != "artifact" || contribution["target_id"] != artifactID || contribution["phase"] != "state_transition" || contribution["role"] != "coordinator" || contribution["body"] != "Decision recorded" || contribution["workflow_run_id"] != workflowRunID {
		t.Fatalf("state response data=%#v", data)
	}
	if value["provenance"].(map[string]any)["replay"] != false {
		t.Fatalf("first request unexpectedly replayed: %#v", value["provenance"])
	}
	run, err := store.WorkflowRun(context.Background(), workflowRunID)
	if err != nil || len(run.GeneratedContributionIDs) != 1 || run.GeneratedContributionIDs[0] != contribution["contribution_id"] {
		t.Fatalf("workflow outputs not appended: run=%+v err=%v", run, err)
	}
	replay := artifactWriteRequest(handler, http.MethodPost, path, body, headers)
	if replay.Code != http.StatusOK || artifactWriteJSON(t, replay)["provenance"].(map[string]any)["replay"] != true {
		t.Fatalf("state replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	contributions, err = store.ArtifactContributions(context.Background(), "write-demo", artifactID)
	if err != nil || len(contributions) != 1 {
		t.Fatalf("state replay duplicated contribution: %+v err=%v", contributions, err)
	}
	run, err = store.WorkflowRun(context.Background(), workflowRunID)
	if err != nil || len(run.GeneratedContributionIDs) != 1 {
		t.Fatalf("state replay duplicated workflow output: %+v err=%v", run, err)
	}
}
