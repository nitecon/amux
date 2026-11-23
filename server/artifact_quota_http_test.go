package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

const artifactQuotaHTTPHelper = "AMUX_ARTIFACT_QUOTA_HTTP_HELPER"

func TestArtifactQuotaHTTPRouteWarningsAndHardReject(t *testing.T) {
	if os.Getenv(artifactQuotaHTTPHelper) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestArtifactQuotaHTTPRouteWarningsAndHardReject$")
		cmd.Env = artifactQuotaTestEnvironment(os.Environ(), map[string]string{"PROJECT_ARTIFACT_SOFT": "2", "PROJECT_ARTIFACT_HARD": "3"})
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated quota HTTP test failed: %v\n%s", err, output)
		}
		return
	}

	store, handler := newArtifactWriteHTTPFixture(t, false)
	for index := 1; index <= 2; index++ {
		response := postQuotaArtifact(t, handler, index)
		if response.Code != http.StatusCreated {
			t.Fatalf("artifact %d status=%d body=%s", index, response.Code, response.Body.String())
		}
		if warnings := artifactWriteJSON(t, response)["provenance"].(map[string]any)["warnings"].([]any); len(warnings) != 0 {
			t.Fatalf("artifact %d unexpectedly warned: %#v", index, warnings)
		}
	}

	soft := postQuotaArtifact(t, handler, 3)
	softValue := artifactWriteJSON(t, soft)
	warnings := softValue["provenance"].(map[string]any)["warnings"].([]any)
	if soft.Code != http.StatusCreated || len(warnings) != 1 || warnings[0] != "quota_artifact_soft" {
		t.Fatalf("soft quota response status=%d warnings=%#v body=%s", soft.Code, warnings, soft.Body.String())
	}

	hard := postQuotaArtifact(t, handler, 4)
	if hard.Code != http.StatusTooManyRequests || !strings.Contains(hard.Body.String(), "quota_artifact_exceeded") {
		t.Fatalf("hard quota response status=%d body=%s", hard.Code, hard.Body.String())
	}
	artifacts, err := store.SearchArtifacts(context.Background(), "write-demo", storage.ArtifactFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 3 {
		t.Fatalf("hard quota rejection changed artifact count: got %d want 3", len(artifacts))
	}
}

func postQuotaArtifact(t *testing.T, handler http.Handler, index int) *httptest.ResponseRecorder {
	t.Helper()
	return artifactWriteRequest(handler, http.MethodPost, "/v1/projects/write-demo/artifacts", map[string]any{
		"kind": "spec", "title": fmt.Sprintf("quota fixture %d", index),
	}, map[string]string{"Idempotency-Key": fmt.Sprintf("quota-fixture-%d", index)})
}

func artifactQuotaTestEnvironment(parent []string, limits map[string]string) []string {
	keys := map[string]bool{
		"ARTIFACT_VERSION_BODY_MAX_BYTES":               true,
		"ARTIFACT_VERSION_STRUCTURED_PAYLOAD_MAX_BYTES": true,
		"CONTRIBUTION_BODY_MAX_BYTES":                   true,
		"COMMENT_BODY_MAX_BYTES":                        true,
		"READ_SET_MAX_REFS":                             true,
		"ARTIFACT_LABELS_MAX_COUNT":                     true,
		"ARTIFACT_LABEL_MAX_BYTES":                      true,
		"PROJECT_ARTIFACT_SOFT":                         true,
		"PROJECT_ARTIFACT_HARD":                         true,
		"PROJECT_VERSION_SOFT":                          true,
		"PROJECT_VERSION_HARD":                          true,
		"PROJECT_LINK_SOFT":                             true,
		"PROJECT_LINK_HARD":                             true,
	}
	env := make([]string, 0, len(parent)+3)
	for _, entry := range parent {
		key, _, _ := strings.Cut(entry, "=")
		if key == artifactQuotaHTTPHelper || keys[key] {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, artifactQuotaHTTPHelper+"=1")
	for key, value := range limits {
		env = append(env, key+"="+value)
	}
	return env
}
