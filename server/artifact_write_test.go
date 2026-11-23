package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func newArtifactWriteHTTPFixture(t *testing.T, enforceScopes bool) (*storage.Store, http.Handler) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "artifact-write.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertProject(ctx, storage.Project{Ident: "write-demo", ChannelName: "write-demo", Kind: "adhoc"}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	handler, err := NewHandlerWithConfig(store, "fixture-key", HandlerConfig{ArtifactAuthEnforced: enforceScopes})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, handler
}

func artifactWriteRequest(handler http.Handler, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	var content bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&content).Encode(body)
	}
	request := httptest.NewRequest(method, path, &content)
	request.Header.Set("Authorization", "Bearer fixture-key")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "fixture-idempotency-key")
	request.Header.Set("X-Agent-Id", "agent-17")
	request.Header.Set("X-Agent-System", "codex")
	request.Header.Set("X-Gateway-User", "forged-admin")
	request.Header.Set("X-Gateway-User-Role", "admin")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func artifactWriteJSON(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("invalid response JSON: %v: %s", err, response.Body.String())
	}
	return value
}

func createArtifactHTTP(t *testing.T, handler http.Handler, title string, headers map[string]string) (string, map[string]any) {
	t.Helper()
	response := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/artifacts", map[string]any{
		"kind": "spec", "title": title,
	}, headers)
	if response.Code != http.StatusCreated {
		t.Fatalf("create artifact status=%d body=%s", response.Code, response.Body.String())
	}
	value := artifactWriteJSON(t, response)
	data, ok := value["data"].(map[string]any)
	if !ok {
		t.Fatalf("missing artifact data: %#v", value)
	}
	return data["artifact_id"].(string), value
}

func TestArtifactWritesThroughNewHandlerEnforceAuthorizationAndProvenance(t *testing.T) {
	_, handler := newArtifactWriteHTTPFixture(t, true)
	artifactPath := "/v1/projects/write-demo/artifacts"
	response := artifactWriteRequest(handler, http.MethodPost, artifactPath, map[string]any{"kind": "spec", "title": "Unauthenticated"}, map[string]string{
		"Authorization": "",
	})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer authentication status=%d body=%s", response.Code, response.Body.String())
	}

	response = artifactWriteRequest(handler, http.MethodPost, artifactPath, map[string]any{"kind": "spec", "title": "Denied"}, map[string]string{
		"X-Agent-Project": "write-demo", "X-Agent-Scopes": "artifact.read",
	})
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "artifact.write") {
		t.Fatalf("missing write scope status=%d body=%s", response.Code, response.Body.String())
	}
	response = artifactWriteRequest(handler, http.MethodPost, artifactPath, map[string]any{"kind": "spec", "title": "Denied"}, map[string]string{
		"X-Agent-Scopes": "artifact.write",
	})
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "x-agent-project") {
		t.Fatalf("missing project scope status=%d body=%s", response.Code, response.Body.String())
	}
	response = artifactWriteRequest(handler, http.MethodPost, artifactPath, map[string]any{"kind": "spec", "title": "Denied"}, map[string]string{
		"X-Agent-Project": "write-demo", "X-Agent-Scopes": "artifact.write",
		"X-Artifact-Quota-Override": " YeS ",
	})
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "project.administer") {
		t.Fatalf("quota override without administer status=%d body=%s", response.Code, response.Body.String())
	}
	if response = artifactWriteRequest(handler, http.MethodPost, artifactPath, map[string]any{"kind": "spec", "title": "Denied"}, map[string]string{
		"X-Agent-Project": "write-demo", "X-Agent-Scopes": "artifact.write",
		"X-Actor-Type": "user",
	}); response.Code != http.StatusForbidden {
		t.Fatalf("client-selected user identity status=%d body=%s", response.Code, response.Body.String())
	}

	id, value := createArtifactHTTP(t, handler, "Authorized", map[string]string{
		"X-Agent-Project": "write-demo", "X-Agent-Scopes": "artifact.write project.administer",
		"X-Artifact-Quota-Override": " TRUE ",
	})
	provenance := value["provenance"].(map[string]any)
	if provenance["idempotency_key"] != "fixture-idempotency-key" || provenance["replay"] != false {
		t.Fatalf("unexpected provenance: %#v", provenance)
	}
	authorization := provenance["authorization"].(map[string]any)
	if authorization["boundary"] != "project-scoped" || fmt.Sprint(authorization["required_scopes"]) != "[artifact.write]" {
		t.Fatalf("quota gate leaked into operation provenance: %#v", authorization)
	}
	actor := provenance["actor"].(map[string]any)
	if actor["actor_type"] != "agent" || actor["agent_id"] != "agent-17" || actor["display_name"] != "agent-17" {
		t.Fatalf("spoofed gateway user changed artifact attribution: %#v", actor)
	}
	if value["data"].(map[string]any)["artifact_id"] != id {
		t.Fatalf("response artifact id mismatch: %#v", value["data"])
	}
	detailPath := "/v1/projects/WRITE-DEMO/artifacts/" + id
	response = artifactWriteRequest(handler, http.MethodGet, detailPath, nil, nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("read without project claim status=%d body=%s", response.Code, response.Body.String())
	}
	response = artifactWriteRequest(handler, http.MethodGet, detailPath, nil, map[string]string{
		"X-Agent-Project": "write-demo", "X-Agent-Scopes": "artifact.read",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("read with project claim status=%d body=%s", response.Code, response.Body.String())
	}
	detail := artifactWriteJSON(t, response)["data"].(map[string]any)["artifact"].(map[string]any)
	if detail["title"] != "Authorized" || detail["project_ident"] != "write-demo" {
		t.Fatalf("scoped artifact detail: %#v", detail)
	}
}

func TestArtifactWritesThroughNewHandlerReturnCreatedReplayAndTreatNullPayloadAsAbsent(t *testing.T) {
	store, handler := newArtifactWriteHTTPFixture(t, false)
	artifactID, _ := createArtifactHTTP(t, handler, "Versioned", nil)
	actor, err := store.ResolveArtifactActor(context.Background(), storage.ArtifactActorIdentity{
		ActorType: "agent", AgentSystem: stringPointer("codex"), AgentID: stringPointer("agent-17"),
		DisplayName: "agent-17",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartWorkflowRun(context.Background(), storage.WorkflowRunInsert{
		ArtifactID: artifactID, WorkflowKind: "spec_iteration", CoordinatorActorID: actor.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/write-demo/artifacts/" + artifactID + "/versions"
	headers := map[string]string{"X-Workflow-Run-ID": run.Record.WorkflowRunID}
	first := artifactWriteRequest(handler, http.MethodPost, path, json.RawMessage(`{"version_label":"v1","body_format":"markdown","body":"content","structured_payload":null}`), headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("first version status=%d body=%s", first.Code, first.Body.String())
	}
	firstValue := artifactWriteJSON(t, first)
	firstID := firstValue["data"].(map[string]any)["artifact_version_id"]
	if firstValue["data"].(map[string]any)["structured_payload"] != nil {
		t.Fatalf("JSON null was persisted as a structured payload: %#v", firstValue["data"])
	}
	second := artifactWriteRequest(handler, http.MethodPost, path, json.RawMessage(`{"version_label":"v1","body_format":"markdown","body":"content","structured_payload":null}`), headers)
	if second.Code != http.StatusOK {
		t.Fatalf("replay version status=%d body=%s", second.Code, second.Body.String())
	}
	secondValue := artifactWriteJSON(t, second)
	if secondValue["data"].(map[string]any)["artifact_version_id"] != firstID || secondValue["provenance"].(map[string]any)["replay"] != true {
		t.Fatalf("version replay was not reported: first=%#v second=%#v", firstValue, secondValue)
	}
	provenance := secondValue["provenance"].(map[string]any)
	if provenance["generated_resources"].(map[string]any)["artifact_version_id"] != firstID ||
		fmt.Sprint(provenance["authorization"].(map[string]any)["required_scopes"]) != "[artifact_version.create]" {
		t.Fatalf("version replay provenance: %#v", provenance)
	}
}

func TestArtifactBodySchemaSwitchTreatsNullAsAbsent(t *testing.T) {
	store, handler := newArtifactWriteHTTPFixture(t, false)
	disabled, err := NewHandlerWithConfig(store, "fixture-key", HandlerConfig{DisableArtifactBodySchema: true})
	if err != nil {
		t.Fatal(err)
	}
	artifactID, _ := createArtifactHTTP(t, handler, "Body schema disabled", nil)
	path := "/v1/projects/write-demo/artifacts/" + artifactID + "/versions"
	response := artifactWriteRequest(disabled, http.MethodPost, path, json.RawMessage(`{"body_format":"markdown","body":"plain markdown","structured_payload":null}`), nil)
	if response.Code != http.StatusCreated {
		t.Fatalf("null payload should count as absent: status=%d body=%s", response.Code, response.Body.String())
	}
	response = artifactWriteRequest(disabled, http.MethodPost, path, json.RawMessage(`{"body_format":"markdown","structured_payload":{"typed":true}}`), nil)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "artifact_body_schema_disabled") {
		t.Fatalf("structured payload should be disabled: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestArtifactWritesThroughNewHandlerRejectOversizedLabelsAndBody(t *testing.T) {
	_, handler := newArtifactWriteHTTPFixture(t, false)
	tooManyLabels := make([]string, 33)
	for i := range tooManyLabels {
		tooManyLabels[i] = fmt.Sprintf("label-%d", i)
	}
	response := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/artifacts", map[string]any{
		"kind": "spec", "title": "Labels too large", "labels": tooManyLabels,
	}, nil)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "labels_too_many") {
		t.Fatalf("oversized labels status=%d body=%s", response.Code, response.Body.String())
	}
	artifactID, _ := createArtifactHTTP(t, handler, "Body too large", nil)
	response = artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/artifacts/"+artifactID+"/versions", map[string]any{
		"body_format": "markdown", "body": strings.Repeat("x", (1<<20)+1),
	}, nil)
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "artifact_version_body_too_large") {
		t.Fatalf("oversized version body status=%d body=%s", response.Code, response.Body.String())
	}
}

func stringPointer(value string) *string { return &value }

func TestArtifactVersionWriteResumesFailedRunAndRejectsCancelledRun(t *testing.T) {
	_, handler := newArtifactWriteHTTPFixture(t, false)
	artifactID, _ := createArtifactHTTP(t, handler, "Workflow recovery", nil)
	base := "/v1/projects/write-demo/artifacts/" + artifactID
	source := artifactWriteRequest(handler, http.MethodPost, base+"/versions", map[string]string{"body": "source"}, nil)
	if source.Code != http.StatusCreated {
		t.Fatalf("source version status=%d body=%s", source.Code, source.Body.String())
	}
	parentID := artifactWriteJSON(t, source)["data"].(map[string]any)["artifact_version_id"].(string)
	for _, test := range []struct {
		state string
		want  int
	}{
		{"failed", http.StatusCreated},
		{"cancelled", http.StatusBadRequest},
	} {
		t.Run(test.state, func(t *testing.T) {
			start := artifactWriteRequest(handler, http.MethodPost, base+"/workflow-runs", map[string]any{
				"workflow_kind": "spec_task_generation", "source_artifact_version_id": parentID, "is_resumable": true,
			}, map[string]string{"Idempotency-Key": "start-" + test.state})
			if start.Code != http.StatusCreated {
				t.Fatalf("start status=%d body=%s", start.Code, start.Body.String())
			}
			runID := artifactWriteJSON(t, start)["data"].(map[string]any)["workflow_run_id"].(string)
			change := map[string]string{"state": test.state}
			if test.state == "failed" {
				change["failure_reason"] = "transient"
			}
			complete := artifactWriteRequest(handler, http.MethodPatch, base+"/workflow-runs/"+runID, change, nil)
			if complete.Code != http.StatusOK {
				t.Fatalf("state change status=%d body=%s", complete.Code, complete.Body.String())
			}
			write := artifactWriteRequest(handler, http.MethodPost, base+"/versions", map[string]string{
				"body": "recovered", "parent_version_id": parentID,
			}, map[string]string{"X-Workflow-Run-ID": runID})
			if write.Code != test.want {
				t.Fatalf("write status=%d want=%d body=%s", write.Code, test.want, write.Body.String())
			}
		})
	}
}

func TestArtifactMutationsRejectInvalidActorParentAndMissingReadSet(t *testing.T) {
	_, handler := newArtifactWriteHTTPFixture(t, false)
	artifactID, _ := createArtifactHTTP(t, handler, "Validation", nil)
	base := "/v1/projects/write-demo/artifacts/" + artifactID
	for _, test := range []struct {
		name, suffix, want string
		body               map[string]string
		headers            map[string]string
	}{
		{"missing agent", "/versions", "x_agent_id_required",
			map[string]string{"body": "body"}, map[string]string{"X-Agent-Id": ""}},
		{"missing key", "/versions", "idempotency_key_required",
			map[string]string{"body": "body"}, map[string]string{"Idempotency-Key": ""}},
		{"missing parent", "/versions", "parent version does not belong to artifact",
			map[string]string{"body": "body", "parent_version_id": "missing-version"}, nil},
		{"missing synthesis read set", "/contributions", "read_set_required",
			map[string]string{"target_kind": "artifact", "target_id": artifactID, "contribution_kind": "synthesis", "phase": "synthesis", "role": "analyst", "body": "synthesis"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := artifactWriteRequest(handler, http.MethodPost, base+test.suffix, test.body, test.headers)
			if response.Code != http.StatusBadRequest || artifactWriteJSON(t, response)["error"] != test.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
