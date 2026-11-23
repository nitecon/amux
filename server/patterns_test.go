package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestPatternRoutesUseRegisteredProductionHandlerAndAuthenticatedAttribution(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "patterns-http.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler, err := NewHandler(store, "patterns-api-key")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	request := func(method, path, payload string, agentID string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, bytes.NewBufferString(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer patterns-api-key")
		req.Header.Set("Content-Type", "application/json")
		if agentID != "" {
			req.Header.Set("X-Agent-Id", agentID)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, body
	}

	unauthorized, err := http.Get(srv.URL + "/v1/patterns")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status=%d", unauthorized.StatusCode)
	}

	status, body := request(http.MethodPost, "/v1/patterns", `{"title":" Release Guide ","slug":"release-guide","summary":"Search needle in summary","body":"Markdown needle in body","labels":"deploy, stable","version":"latest","state":"active"}`, "pattern-agent")
	if status != http.StatusOK {
		t.Fatalf("create status=%d body=%s", status, body)
	}
	var created storage.Pattern
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Title != "Release Guide" || created.Author != "pattern-agent" || created.Version != "latest" || len(created.Labels) != 2 || created.Labels[0] != "deploy" {
		t.Fatalf("create response: %+v", created)
	}
	status, body = request(http.MethodPost, "/v1/patterns/"+created.ID+"/comments", `{"content":"review note"}`, "pattern-agent")
	var comment storage.PatternComment
	if status != http.StatusOK || json.Unmarshal(body, &comment) != nil || comment.Author != "pattern-agent" || comment.AuthorType != "agent" || comment.PatternID != created.ID {
		t.Fatalf("comment status=%d response=%+v body=%s", status, comment, body)
	}
	status, body = request(http.MethodGet, "/v1/patterns?q=needle&label=deploy&version=latest", "", "")
	var summaries []storage.PatternSummary
	if status != http.StatusOK || json.Unmarshal(body, &summaries) != nil || len(summaries) != 1 || summaries[0].CommentCount != 1 || summaries[0].Slug != created.Slug {
		t.Fatalf("filtered list status=%d summaries=%+v body=%s", status, summaries, body)
	}
	for _, test := range []struct{ query, message string }{
		{"version=", "invalid version ''"},
		{"state=", "invalid state ''"},
	} {
		status, body = request(http.MethodGet, "/v1/patterns?"+test.query, "", "")
		if status != http.StatusBadRequest || !strings.Contains(string(body), test.message) {
			t.Fatalf("explicit empty filter %q status=%d body=%s", test.query, status, body)
		}
	}
	status, body = request(http.MethodGet, "/v1/patterns/"+created.Slug, "", "")
	var detail map[string]json.RawMessage
	if status != http.StatusOK || json.Unmarshal(body, &detail) != nil || detail["comments"] != nil {
		t.Fatalf("detail should omit comments: status=%d body=%s", status, body)
	}
	status, body = request(http.MethodGet, "/v1/patterns/"+created.Slug+"/comments", "", "")
	var comments []storage.PatternComment
	if status != http.StatusOK || json.Unmarshal(body, &comments) != nil || len(comments) != 1 || comments[0].ID != comment.ID {
		t.Fatalf("explicit comments status=%d comments=%+v body=%s", status, comments, body)
	}

	status, body = request(http.MethodPatch, "/v1/patterns/"+created.ID, `{"summary":null,"labels":null,"body":""}`, "")
	var updated storage.Pattern
	if status != http.StatusOK || json.Unmarshal(body, &updated) != nil || updated.Summary != nil || len(updated.Labels) != 0 || updated.Body != "" {
		t.Fatalf("patch nullable fields status=%d item=%+v body=%s", status, updated, body)
	}
	status, body = request(http.MethodPatch, "/v1/patterns/"+created.ID, `{"version":"superseded"}`, "")
	if status != http.StatusBadRequest || !strings.Contains(string(body), "superseded_by is required") {
		t.Fatalf("missing supersession target status=%d body=%s", status, body)
	}
	status, body = request(http.MethodPatch, "/v1/patterns/"+created.ID, `{"version":"superseded","superseded_by":"replacement"}`, "")
	if status != http.StatusOK {
		t.Fatalf("set lifecycle status=%d body=%s", status, body)
	}
	status, body = request(http.MethodGet, "/v1/patterns?superseded_by=replacement", "", "")
	if status != http.StatusOK || !strings.Contains(string(body), `"version":"superseded"`) {
		t.Fatalf("lifecycle filter status=%d body=%s", status, body)
	}
	status, body = request(http.MethodPatch, "/v1/patterns/"+created.ID, `{"version":"latest"}`, "")
	if status != http.StatusOK || strings.Contains(string(body), "replacement") {
		t.Fatalf("clear superseded_by status=%d body=%s", status, body)
	}
	status, body = request(http.MethodDelete, "/v1/patterns/"+created.ID, "", "")
	if status != http.StatusOK || string(body) != "{\"deleted\":true}\n" {
		t.Fatalf("delete status=%d body=%s", status, body)
	}
	status, body = request(http.MethodGet, "/v1/patterns/"+created.ID, "", "")
	if status != http.StatusNotFound {
		t.Fatalf("deleted pattern status=%d body=%s", status, body)
	}
}
