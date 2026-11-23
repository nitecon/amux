package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestArtifactWorkflowRunReadRoutesMatchRustEnvelopeAndAuthorization(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "workflow-runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "workflow-routes", ChannelName: "workflow-routes", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	actor, err := store.ResolveArtifactActor(ctx, storage.ArtifactActorIdentity{ActorType: "user", DisplayName: "Workflow test"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CreateArtifact(ctx, storage.ArtifactInsert{
		ProjectIdent: "workflow-routes", Kind: "spec", Title: "Workflow contract", CreatedByActorID: actor.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherArtifact, err := store.CreateArtifact(ctx, storage.ArtifactInsert{
		ProjectIdent: "workflow-routes", Kind: "spec", Title: "Other artifact", CreatedByActorID: actor.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertProject(ctx, storage.Project{Ident: "workflow-other", ChannelName: "workflow-other", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	otherProjectArtifact, err := store.CreateArtifact(ctx, storage.ArtifactInsert{
		ProjectIdent: "workflow-other", Kind: "spec", Title: "Other project artifact", CreatedByActorID: actor.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := store.StartWorkflowRun(ctx, storage.WorkflowRunInsert{
		ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_iteration", CoordinatorActorID: actor.ActorID,
		ParticipantActorIDs: []string{}, ReadSet: json.RawMessage(`{"artifact_version_ids":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandlerWithConfig(store, "workflow-key", HandlerConfig{ArtifactAuthEnforced: true})
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/projects/workflow-routes/artifacts/" + artifact.ArtifactID + "/workflow-runs"
	request := func(path, scopes, project string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer workflow-key")
		if project != "" {
			req.Header.Set("X-Agent-Project", project)
		}
		if scopes != "" {
			req.Header.Set("X-Agent-Scopes", scopes)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	for _, test := range []struct {
		path, scopes, project string
		want                  int
	}{
		{path: base, want: http.StatusForbidden},
		{path: base, scopes: "artifact.read", project: "other-project", want: http.StatusForbidden},
		{path: base, scopes: "artifact.read", project: "workflow-routes", want: http.StatusOK},
		{path: base + "/" + started.Record.WorkflowRunID, scopes: "artifact.read", project: "workflow-routes", want: http.StatusOK},
		{path: "/v1/projects/workflow-routes/artifacts/missing/workflow-runs", scopes: "artifact.read", project: "workflow-routes", want: http.StatusNotFound},
		{path: base + "/missing", scopes: "artifact.read", project: "workflow-routes", want: http.StatusNotFound},
		{path: "/v1/projects/workflow-routes/artifacts/missing/workflow-runs/" + started.Record.WorkflowRunID, scopes: "artifact.read", project: "workflow-routes", want: http.StatusNotFound},
		{path: "/v1/projects/other-project/artifacts/" + artifact.ArtifactID + "/workflow-runs/" + started.Record.WorkflowRunID, scopes: "artifact.read", project: "other-project", want: http.StatusNotFound},
		{path: "/v1/projects/workflow-routes/artifacts/" + otherArtifact.ArtifactID + "/workflow-runs/" + started.Record.WorkflowRunID, scopes: "artifact.read", project: "workflow-routes", want: http.StatusNotFound},
		{path: "/v1/projects/workflow-other/artifacts/" + otherProjectArtifact.ArtifactID + "/workflow-runs/" + started.Record.WorkflowRunID, scopes: "artifact.read", project: "workflow-other", want: http.StatusNotFound},
	} {
		res := request(test.path, test.scopes, test.project)
		if res.Code != test.want {
			t.Errorf("GET %s => %d, want %d: %s", test.path, res.Code, test.want, res.Body.String())
			continue
		}
		if res.Code == http.StatusOK {
			body := res.Body.String()
			if !strings.Contains(body, `"chunking_status":{"status":"none"`) || !strings.Contains(body, started.Record.WorkflowRunID) || !strings.Contains(body, `"generated_contribution_ids":[]`) {
				t.Errorf("GET %s returned non-Rust workflow envelope: %s", test.path, body)
			}
		}
	}
	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, base, nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET => %d, want %d", unauthenticated.Code, http.StatusUnauthorized)
	}
}

func TestArtifactWorkflowRunWriteRoutesReplayAndEnforceCoordinatorAndScope(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "workflow-run-writes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "workflow-writes", ChannelName: "workflow-writes", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	seedActor, err := store.ResolveArtifactActor(ctx, storage.ArtifactActorIdentity{ActorType: "user", DisplayName: "Seed"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CreateArtifact(ctx, storage.ArtifactInsert{
		ProjectIdent: "workflow-writes", Kind: "spec", Title: "Workflow writes", CreatedByActorID: seedActor.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherArtifact, err := store.CreateArtifact(ctx, storage.ArtifactInsert{
		ProjectIdent: "workflow-writes", Kind: "spec", Title: "Other workflow artifact", CreatedByActorID: seedActor.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertProject(ctx, storage.Project{Ident: "workflow-writes-other", ChannelName: "workflow-writes-other", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	otherProjectArtifact, err := store.CreateArtifact(ctx, storage.ArtifactInsert{
		ProjectIdent: "workflow-writes-other", Kind: "spec", Title: "Other project artifact", CreatedByActorID: seedActor.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandlerWithConfig(store, "workflow-write-key", HandlerConfig{ArtifactAuthEnforced: true})
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/projects/workflow-writes/artifacts/" + artifact.ArtifactID + "/workflow-runs"
	call := func(method, path, body, agentID, key, scopes string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer workflow-write-key")
		req.Header.Set("Content-Type", "application/json")
		project := "workflow-writes"
		if strings.HasPrefix(path, "/v1/projects/workflow-writes-other/") {
			project = "workflow-writes-other"
		}
		req.Header.Set("X-Agent-Project", project)
		req.Header.Set("X-Agent-Scopes", scopes)
		req.Header.Set("X-Agent-Id", agentID)
		req.Header.Set("X-Agent-System", "codex")
		req.Header.Set("Idempotency-Key", key)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	startBody := `{"workflow_kind":"spec_task_generation","participant_actor_ids":[],"read_set":{"artifact_version_ids":[]}}`
	start := call(http.MethodPost, base, startBody, "agent-one", "run-start-key", "workflow_run.start")
	if start.Code != http.StatusCreated {
		t.Fatalf("start => %d: %s", start.Code, start.Body.String())
	}
	var started struct {
		Data       storage.WorkflowRun `json:"data"`
		Provenance struct {
			Replay bool `json:"replay"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.Data.WorkflowRunID == "" || started.Data.State != "started" || !started.Data.IsResumable || started.Provenance.Replay {
		t.Fatalf("unexpected start response: %+v", started)
	}
	replay := call(http.MethodPost, base, startBody, "agent-one", "run-start-key", "workflow_run.start")
	var replayed struct {
		Data       storage.WorkflowRun `json:"data"`
		Provenance struct {
			Replay bool `json:"replay"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replay.Code != http.StatusOK || !replayed.Provenance.Replay || replayed.Data.WorkflowRunID != started.Data.WorkflowRunID {
		t.Fatalf("start replay => %d: %s", replay.Code, replay.Body.String())
	}
	invalidKind := call(http.MethodPost, base, `{"workflow_kind":"made_up"}`, "agent-one", "invalid-kind", "workflow_run.start")
	if invalidKind.Code != http.StatusBadRequest {
		t.Fatalf("invalid workflow kind => %d: %s", invalidKind.Code, invalidKind.Body.String())
	}
	missingKind := call(http.MethodPost, base, `{}`, "agent-one", "missing-kind", "workflow_run.start")
	if missingKind.Code != http.StatusBadRequest || !strings.Contains(missingKind.Body.String(), "workflow_kind_required") {
		t.Fatalf("missing workflow kind => %d: %s", missingKind.Code, missingKind.Body.String())
	}
	wrongArtifact := call(http.MethodPatch, "/v1/projects/workflow-writes/artifacts/"+otherArtifact.ArtifactID+"/workflow-runs/"+started.Data.WorkflowRunID, `{"state":"succeeded"}`, "agent-one", "wrong-artifact", "workflow_run.complete")
	if wrongArtifact.Code != http.StatusNotFound {
		t.Fatalf("cross-artifact completion => %d: %s", wrongArtifact.Code, wrongArtifact.Body.String())
	}
	wrongProject := call(http.MethodPatch, "/v1/projects/workflow-writes-other/artifacts/"+otherProjectArtifact.ArtifactID+"/workflow-runs/"+started.Data.WorkflowRunID, `{"state":"succeeded"}`, "agent-one", "wrong-project", "workflow_run.complete")
	if wrongProject.Code != http.StatusNotFound {
		t.Fatalf("cross-project completion => %d: %s", wrongProject.Code, wrongProject.Body.String())
	}
	wrongActor := call(http.MethodPatch, base+"/"+started.Data.WorkflowRunID, `{"state":"succeeded"}`, "agent-two", "wrong-actor", "workflow_run.complete")
	if wrongActor.Code != http.StatusInternalServerError {
		t.Fatalf("non-coordinator completion => %d: %s", wrongActor.Code, wrongActor.Body.String())
	}
	missingFailure := call(http.MethodPatch, base+"/"+started.Data.WorkflowRunID, `{"state":"failed"}`, "agent-one", "missing-failure", "workflow_run.complete")
	if missingFailure.Code != http.StatusInternalServerError {
		t.Fatalf("failed completion without reason => %d: %s", missingFailure.Code, missingFailure.Body.String())
	}
	missingState := call(http.MethodPatch, base+"/"+started.Data.WorkflowRunID, `{}`, "agent-one", "missing-state", "workflow_run.complete")
	if missingState.Code != http.StatusBadRequest || !strings.Contains(missingState.Body.String(), "state_required") {
		t.Fatalf("missing completion state => %d: %s", missingState.Code, missingState.Body.String())
	}
	failure := call(http.MethodPatch, base+"/"+started.Data.WorkflowRunID, `{"state":"failed","failure_reason":"temporary interruption","generated_task_ids":["task-1"],"generated_chunk_ids":["chunk-1"]}`, "agent-one", "run-failed", "workflow_run.complete")
	if failure.Code != http.StatusOK || !strings.Contains(failure.Body.String(), `"state":"failed"`) || !strings.Contains(failure.Body.String(), `"generated_task_ids":["task-1"]`) {
		t.Fatalf("failed completion => %d: %s", failure.Code, failure.Body.String())
	}
	completion := call(http.MethodPatch, base+"/"+started.Data.WorkflowRunID, `{"state":"succeeded","generated_contribution_ids":["contribution-1"],"generated_version_ids":["version-1"],"generated_link_ids":["link-1"]}`, "agent-one", "run-complete", "workflow_run.complete")
	if completion.Code != http.StatusOK {
		t.Fatalf("resumed completion => %d: %s", completion.Code, completion.Body.String())
	}
	for _, field := range []string{`"state":"succeeded"`, `"generated_contribution_ids":["contribution-1"]`, `"generated_version_ids":["version-1"]`, `"generated_task_ids":["task-1"]`, `"generated_link_ids":["link-1"]`, `"generated_chunk_ids":["chunk-1"]`} {
		if !strings.Contains(completion.Body.String(), field) {
			t.Errorf("completion omitted %s: %s", field, completion.Body.String())
		}
	}
	terminalReplay := call(http.MethodPatch, base+"/"+started.Data.WorkflowRunID, `{"state":"cancelled"}`, "agent-one", "run-terminal", "workflow_run.complete")
	if terminalReplay.Code != http.StatusBadRequest {
		t.Fatalf("terminal transition => %d: %s", terminalReplay.Code, terminalReplay.Body.String())
	}
}
