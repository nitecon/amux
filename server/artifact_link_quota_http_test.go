package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestArtifactLinkQuotaHTTPWarnsRejectsAndReplays(t *testing.T) {
	if os.Getenv(artifactQuotaHTTPHelper) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestArtifactLinkQuotaHTTPWarnsRejectsAndReplays$")
		cmd.Env = artifactQuotaTestEnvironment(os.Environ(), map[string]string{"PROJECT_LINK_SOFT": "1", "PROJECT_LINK_HARD": "2"})
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated link quota HTTP test failed: %v\n%s", err, output)
		}
		return
	}

	store, handler := newArtifactWriteHTTPFixture(t, false)
	artifactID, _ := createArtifactHTTP(t, handler, "link quota source", nil)
	actorID := mustArtifactCreator(t, store, artifactID)
	run, err := store.StartWorkflowRun(t.Context(), storage.WorkflowRunInsert{
		ArtifactID: artifactID, WorkflowKind: "spec_task_generation", CoordinatorActorID: actorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/write-demo/artifacts/links"
	payload := map[string]any{
		"link_type": "doc_referenced_by_spec", "source_kind": "artifact", "source_id": artifactID,
		"target_kind": "external_url", "target_id": "https://example.test/quota",
	}
	post := func(key string) *httptest.ResponseRecorder {
		t.Helper()
		return artifactWriteRequest(handler, http.MethodPost, path, payload, map[string]string{
			"Idempotency-Key": key, "X-Workflow-Run-ID": run.Record.WorkflowRunID,
		})
	}

	first := post("link-quota-first")
	if first.Code != http.StatusCreated {
		t.Fatalf("first link status=%d body=%s", first.Code, first.Body.String())
	}
	firstValue := artifactWriteJSON(t, first)
	firstID := firstValue["data"].(map[string]any)["link_id"]
	if warnings := firstValue["provenance"].(map[string]any)["warnings"].([]any); len(warnings) != 0 {
		t.Fatalf("first link unexpectedly warned: %#v", warnings)
	}

	payload["target_id"] = "https://example.test/quota-2"
	second := post("link-quota-second")
	secondValue := artifactWriteJSON(t, second)
	warnings := secondValue["provenance"].(map[string]any)["warnings"].([]any)
	if second.Code != http.StatusCreated || len(warnings) != 1 || warnings[0] != "quota_link_soft" {
		t.Fatalf("soft quota response status=%d warnings=%#v body=%s", second.Code, warnings, second.Body.String())
	}

	payload["target_id"] = "https://example.test/quota-3"
	hard := post("link-quota-third")
	if hard.Code != http.StatusTooManyRequests || !strings.Contains(hard.Body.String(), "quota_link_exceeded") {
		t.Fatalf("hard quota response status=%d body=%s", hard.Code, hard.Body.String())
	}

	payload["target_id"] = "https://example.test/quota"
	replay := post("link-quota-first")
	replayValue := artifactWriteJSON(t, replay)
	if replay.Code != http.StatusOK || replayValue["provenance"].(map[string]any)["replay"] != true || replayValue["data"].(map[string]any)["link_id"] != firstID {
		t.Fatalf("link replay at hard quota status=%d body=%s", replay.Code, replay.Body.String())
	}
	if warnings := replayValue["provenance"].(map[string]any)["warnings"].([]any); len(warnings) != 0 {
		t.Fatalf("replay returned new-write quota warnings: %#v", warnings)
	}
	count, err := store.CountArtifactLinks(t.Context(), "write-demo")
	if err != nil || count != 2 {
		t.Fatalf("hard reject/replay changed link count: count=%d err=%v", count, err)
	}
}
