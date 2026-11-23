package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestSpecImportManifestRoundTripAndFetchStableItemThroughHTTP(t *testing.T) {
	_, handler := newArtifactWriteHTTPFixture(t, false)
	manifest := json.RawMessage(`{"spec_version":1,"source_doc":"../gateway-features.md","phases":[{"id":"phase-1","name":"Phase 1","tasks":[{"id":"T001","team":"backend","title":"Create substrate contract","depends_on":[],"labels":["gateway-features","phase-1"],"touch_surface":["docs/artifact-substrate-v1.md"],"acceptance_criteria":["contract is durable"],"validation_plan":["go test ./server"],"spec_file":"backend/T001.md","status":"todo"},{"id":"T002","team":"backend","title":"Implement task generation","depends_on":["T001"],"labels":["gateway-features","phase-1"],"touch_surface":["server/spec_manifest_write.go"],"acceptance_criteria":["tasks are generated idempotently"],"validation_plan":["go test ./server"],"spec_file":"backend/T002.md","status":"todo"}]}]}`)
	files := map[string]string{
		"backend/T001.md": "# T001\n\nFocused handoff for substrate.",
		"backend/T002.md": "# T002\n\nFocused handoff for task generation.",
	}
	created := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/specs", map[string]any{
		"title": "Gateway Features", "labels": []string{"gateway-features"}, "body": "# Gateway Features Spec",
		"manifest": manifest, "file_bodies": files, "source_doc": "../gateway-features.md",
	}, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("spec import status=%d body=%s", created.Code, created.Body.String())
	}
	createdValue := artifactWriteJSON(t, created)
	if createdValue["provenance"].(map[string]any)["replay"] != false {
		t.Fatalf("initial spec creation incorrectly reported replay: %#v", createdValue["provenance"])
	}
	data := createdValue["data"].(map[string]any)
	artifactID := data["artifact"].(map[string]any)["artifact_id"].(string)
	firstVersionID := data["version"].(map[string]any)["artifact_version_id"].(string)
	base := "/v1/projects/write-demo/specs/" + artifactID + "/manifest"
	manifestResponse := artifactWriteRequest(handler, http.MethodGet, base+"?version_id="+firstVersionID, nil, nil)
	if manifestResponse.Code != http.StatusOK {
		t.Fatalf("manifest fetch status=%d body=%s", manifestResponse.Code, manifestResponse.Body.String())
	}
	var fetched artifactReadEnvelope[specManifestResponse]
	if err := json.Unmarshal(manifestResponse.Body.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.Data.ArtifactVersionID != firstVersionID || len(fetched.Data.Items) != 2 || fetched.Data.Items[0].ManifestItemID != "phase-1:T001" || fetched.Data.Items[0].Phase == nil || *fetched.Data.Items[0].Phase != "Phase 1" || fetched.Data.Items[1].ManifestItemID != "phase-1:T002" {
		t.Fatalf("imported stable manifest items mismatch: %+v", fetched.Data)
	}
	var policy map[string]string
	if err := json.Unmarshal(fetched.Data.StabilityPolicy, &policy); err != nil || policy["renamed"] != "preserve manifest_item_id when the task id/code is unchanged" {
		t.Fatalf("imported stability policy: %+v, %v", policy, err)
	}
	if fetched.Data.Items[0].SpecBody == nil || *fetched.Data.Items[0].SpecBody != files["backend/T001.md"] || fetched.Data.Items[1].SpecBody == nil || *fetched.Data.Items[1].SpecBody != files["backend/T002.md"] {
		t.Fatalf("source file bodies were not attached to manifest items: %+v", fetched.Data.Items)
	}
	itemResponse := artifactWriteRequest(handler, http.MethodGet, base+"/phase-1:T002", nil, nil)
	if itemResponse.Code != http.StatusOK || !strings.Contains(itemResponse.Body.String(), `"task_code":"T002"`) || !strings.Contains(itemResponse.Body.String(), `"acceptance_criteria":["tasks are generated idempotently"]`) {
		t.Fatalf("stable item fetch status=%d body=%s", itemResponse.Code, itemResponse.Body.String())
	}

	workflowPath := "/v1/projects/write-demo/artifacts/" + artifactID + "/workflow-runs"
	workflow := artifactWriteRequest(handler, http.MethodPost, workflowPath, map[string]string{"workflow_kind": "spec_iteration"}, nil)
	if workflow.Code != http.StatusCreated {
		t.Fatalf("start import workflow status=%d body=%s", workflow.Code, workflow.Body.String())
	}
	runID := artifactWriteJSON(t, workflow)["data"].(map[string]any)["workflow_run_id"].(string)
	versionPath := "/v1/projects/write-demo/specs/" + artifactID + "/versions"
	payload := map[string]any{
		"parent_version_id": firstVersionID, "body": "Updated source", "manifest": manifest, "file_bodies": files,
	}
	headers := map[string]string{"Idempotency-Key": "spec-version-import", "X-Workflow-Run-ID": runID}
	next := artifactWriteRequest(handler, http.MethodPost, versionPath, payload, headers)
	if next.Code != http.StatusCreated {
		t.Fatalf("spec version import status=%d body=%s", next.Code, next.Body.String())
	}
	nextVersionID := artifactWriteJSON(t, next)["data"].(map[string]any)["artifact_version_id"].(string)
	retry := artifactWriteRequest(handler, http.MethodPost, versionPath, payload, headers)
	if retry.Code != http.StatusOK {
		t.Fatalf("spec version retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	replayed := artifactWriteJSON(t, retry)
	if replayed["data"].(map[string]any)["artifact_version_id"] != nextVersionID || replayed["provenance"].(map[string]any)["replay"] != true {
		t.Fatalf("spec version retry changed the operation result: %#v", replayed)
	}
	current := artifactWriteRequest(handler, http.MethodGet, base, nil, nil)
	if current.Code != http.StatusOK || !strings.Contains(current.Body.String(), `"artifact_version_id":"`+nextVersionID+`"`) || !strings.Contains(current.Body.String(), `"manifest_item_id":"phase-1:T001"`) {
		t.Fatalf("current imported manifest version was not readable: status=%d body=%s", current.Code, current.Body.String())
	}
}

func TestSpecImportRollsBackArtifactWhenInitialVersionFails(t *testing.T) {
	store, handler := newArtifactWriteHTTPFixture(t, false)
	manifest := json.RawMessage(`{"items":[{"manifest_item_id":"stable-1","title":"Atomic import","dependencies":[],"labels":[],"touch_surface":[],"acceptance_criteria":[],"validation_plan":[],"metadata":{}}]}`)
	response := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/specs", map[string]any{
		"title": "Atomic import", "manifest": manifest, "body": strings.Repeat("x", (1<<20)+1),
	}, nil)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized initial version status=%d body=%s", response.Code, response.Body.String())
	}
	artifacts, err := store.SearchArtifacts(context.Background(), "write-demo", storage.ArtifactFilters{Kind: "spec", Query: "Atomic import"})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("failed initial version left an artifact behind: %+v", artifacts)
	}
}

func TestSpecManifestImportRejectsMissingAndDuplicateStableIDs(t *testing.T) {
	_, handler := newArtifactWriteHTTPFixture(t, false)
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"missing stable identity", `{"items":[{"title":"No id"}]}`, "manifest_item_id_required"},
		{"duplicate stable identity", `{"items":[{"manifest_item_id":"same","title":"One"},{"stable_id":"same","title":"Two"}]}`, "duplicate_manifest_item_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/specs", map[string]any{
				"title": "Invalid import", "manifest": json.RawMessage(tc.raw),
			}, map[string]string{"Idempotency-Key": fmt.Sprintf("invalid-%s", strings.ReplaceAll(tc.name, " ", "-"))})
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), tc.want) {
				t.Fatalf("invalid manifest status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
