package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
)

func TestSpecTaskGenerationRequiresConfirmationAndAcceptedVersion(t *testing.T) {
	store, handler := newArtifactWriteHTTPFixture(t, false)
	artifactID, _ := createArtifactHTTP(t, handler, "Draft spec", nil)
	path := "/v1/projects/write-demo/specs/" + artifactID + "/generate-tasks"
	headers := map[string]string{
		"X-Agent-Session-Id":  "11111111-1111-4111-8111-111111111111",
		"X-Agent-Instance-Id": "22222222-2222-4222-8222-222222222222",
		"X-Agent-Provider":    "codex", "X-Agent-Os": "linux",
	}
	for _, test := range []struct {
		confirmed bool
		want      string
	}{
		{false, "task_generation_confirmation_required"},
		{true, "accepted spec version required"},
	} {
		response := artifactWriteRequest(handler, http.MethodPost, path, map[string]bool{"confirmed": test.confirmed}, headers)
		if response.Code != http.StatusBadRequest || artifactWriteJSON(t, response)["error"] != test.want {
			t.Fatalf("confirmed=%v status=%d body=%s", test.confirmed, response.Code, response.Body.String())
		}
	}
	runs, err := store.WorkflowRuns(t.Context(), "write-demo", artifactID)
	if err != nil || len(runs) != 0 {
		t.Fatalf("rejected generation started a workflow: %+v, %v", runs, err)
	}
}

func TestConcurrentSpecGenerationWithDistinctKeysKeepsOneTaskAndLinkPerItem(t *testing.T) {
	store, handler, artifactID, _, _, _ := acceptedSpecTaskFixture(t, false)
	path := "/v1/projects/write-demo/specs/" + artifactID + "/generate-tasks"
	body := map[string]bool{"confirmed": true}
	type result struct {
		status int
		body   string
	}
	start := make(chan struct{})
	results := make(chan result, 4)
	for i := range 4 {
		headers := specTaskOriginHeaders(fmt.Sprintf("concurrent-generation-%d", i))
		go func() {
			<-start
			res := artifactWriteRequest(handler, http.MethodPost, path, body, headers)
			results <- result{res.Code, res.Body.String()}
		}()
	}
	close(start)
	var expected []string
	for range 4 {
		got := <-results
		if got.status != http.StatusCreated && got.status != http.StatusOK {
			t.Errorf("generation status=%d body=%s", got.status, got.body)
			continue
		}
		var response struct {
			Data struct {
				Tasks []string `json:"generated_task_ids"`
				Links []string `json:"generated_link_ids"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(got.body), &response); err != nil || len(response.Data.Tasks) != 2 || len(response.Data.Links) != 2 {
			t.Errorf("generation output=%s err=%v", got.body, err)
			continue
		}
		ids := append(response.Data.Tasks, response.Data.Links...)
		if expected == nil {
			expected = ids
		} else if !slices.Equal(expected, ids) {
			t.Errorf("concurrent generation IDs=%v want %v", ids, expected)
		}
	}
	tasks, err := store.Tasks(t.Context(), "write-demo", []string{"todo"}, true)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("generated task count=%d err=%v", len(tasks), err)
	}
	links, err := store.CountArtifactLinks(t.Context(), "write-demo")
	if err != nil || links != 2 {
		t.Fatalf("generated link count=%d err=%v", links, err)
	}
}
