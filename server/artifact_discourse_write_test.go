package server

import (
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

func TestArtifactDiscourseMutationRoutesUseSharedEnvelopeAndProjectScope(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "discourse-http.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "discourse-http", ChannelName: "discourse-http", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	creator, err := store.ResolveArtifactActor(ctx, storage.ArtifactActorIdentity{ActorType: "user", DisplayName: "Creator"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CreateArtifact(ctx, storage.ArtifactInsert{ProjectIdent: "discourse-http", Kind: "spec", Title: "HTTP discourse", CreatedByActorID: creator.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	version, err := store.CreateArtifactVersion(ctx, "discourse-http", artifact.ArtifactID, storage.ArtifactVersionInsert{BodyFormat: "markdown", Body: stringPtr("spec body"), CreatedByActorID: creator.ActorID})
	if err != nil || version == nil || version.Record == nil {
		t.Fatalf("create version: %+v %v", version, err)
	}
	handler, err := NewHandlerWithConfig(store, "discourse-key", HandlerConfig{ArtifactAuthEnforced: true})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	request := func(path, payload, key, scopes string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer discourse-key")
		req.Header.Set("X-Agent-Project", "discourse-http")
		req.Header.Set("X-Agent-Scopes", scopes)
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("X-Agent-Id", "reviewer-agent")
		req.Header.Set("X-Agent-System", "codex")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return res, body
	}
	base := "/v1/projects/discourse-http/artifacts/" + artifact.ArtifactID
	commentPath := base + "/comments"
	commentReq := `{"target_kind":"artifact_version","target_id":"` + version.Record.ArtifactVersionID + `","child_address":"manifest.items[item-1]","body":"Looks good","actor_display_name":"Review bot"}`
	denied, body := request(commentPath, commentReq, "comment-once", "contribution.write")
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong scope => %d %s", denied.StatusCode, body)
	}
	created, body := request(commentPath, commentReq, "comment-once", "comment.write")
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("comment create => %d %s", created.StatusCode, body)
	}
	var commentResponse struct {
		Data       storage.ArtifactComment `json:"data"`
		Provenance struct {
			Replay        bool           `json:"replay"`
			Generated     map[string]any `json:"generated_resources"`
			Authorization struct {
				RequiredScopes []string `json:"required_scopes"`
			} `json:"authorization"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(body, &commentResponse); err != nil {
		t.Fatal(err)
	}
	comment := commentResponse.Data
	if comment.CommentID == "" || comment.TargetID != version.Record.ArtifactVersionID || comment.ChildAddress == nil || *comment.ChildAddress != "manifest.items[item-1]" || commentResponse.Provenance.Replay || commentResponse.Provenance.Generated["comment_id"] != comment.CommentID || strings.Join(commentResponse.Provenance.Authorization.RequiredScopes, ",") != "comment.write" {
		t.Fatalf("comment response mismatch: %+v", commentResponse)
	}
	replayed, body := request(commentPath, commentReq, "comment-once", "comment.write")
	var replayResponse struct {
		Data       storage.ArtifactComment `json:"data"`
		Provenance struct {
			Replay bool `json:"replay"`
		} `json:"provenance"`
	}
	if replayed.StatusCode != http.StatusOK || json.Unmarshal(body, &replayResponse) != nil || !replayResponse.Provenance.Replay || replayResponse.Data.CommentID != comment.CommentID {
		t.Fatalf("comment replay => %d %+v %s", replayed.StatusCode, replayResponse, body)
	}
	oversizedComment := `{"target_kind":"artifact_version","target_id":"` + version.Record.ArtifactVersionID + `","body":"` + strings.Repeat("x", (32<<10)+1) + `"}`
	tooLargeComment, body := request(commentPath, oversizedComment, "too-large-comment", "comment.write")
	if tooLargeComment.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(body), "comment_body_too_large") {
		t.Fatalf("oversized comment => %d %s", tooLargeComment.StatusCode, body)
	}
	resolve, body := request(commentPath+"/"+comment.CommentID+"/resolve", `{"resolution_note":"addressed"}`, "resolve-once", "comment.resolve")
	var resolved struct {
		Data storage.ArtifactComment `json:"data"`
	}
	if resolve.StatusCode != http.StatusOK || json.Unmarshal(body, &resolved) != nil || resolved.Data.State != "resolved" || resolved.Data.ResolutionNote == nil || *resolved.Data.ResolutionNote != "addressed" {
		t.Fatalf("resolve => %d %+v %s", resolve.StatusCode, resolved, body)
	}
	reopen, body := request(commentPath+"/"+comment.CommentID+"/reopen", `{"note_body":"please revisit"}`, "reopen-once", "comment.write")
	var reopened struct {
		Data storage.ArtifactComment `json:"data"`
	}
	if reopen.StatusCode != http.StatusOK || json.Unmarshal(body, &reopened) != nil || reopened.Data.State != "open" || reopened.Data.ResolvedAt != nil {
		t.Fatalf("reopen => %d %+v %s", reopen.StatusCode, reopened, body)
	}

	contributionPath := base + "/contributions"
	contributionReq := `{"target_kind":"artifact_version","target_id":"` + version.Record.ArtifactVersionID + `","contribution_kind":"review","phase":"pass_1","role":"reviewer","read_set":{"versions":["` + version.Record.ArtifactVersionID + `"],"manifest_items":["manifest.items[deferred]"]},"body":"Review notes"}`
	contribution, body := request(contributionPath, contributionReq, "contribution-once", "contribution.write")
	var contributionResponse struct {
		Data       storage.ArtifactContribution `json:"data"`
		Provenance struct {
			Generated map[string]any `json:"generated_resources"`
			Replay    bool           `json:"replay"`
		} `json:"provenance"`
	}
	if contribution.StatusCode != http.StatusCreated || json.Unmarshal(body, &contributionResponse) != nil || contributionResponse.Data.TargetID != version.Record.ArtifactVersionID || contributionResponse.Data.Body != "Review notes" || contributionResponse.Provenance.Generated["contribution_id"] != contributionResponse.Data.ContributionID {
		t.Fatalf("contribution => %d %+v %s", contribution.StatusCode, contributionResponse, body)
	}
	contributionRetry, body := request(contributionPath, contributionReq, "contribution-once", "contribution.write")
	contributionID := contributionResponse.Data.ContributionID
	if contributionRetry.StatusCode != http.StatusOK || json.Unmarshal(body, &contributionResponse) != nil || !contributionResponse.Provenance.Replay || contributionResponse.Data.ContributionID != contributionID {
		t.Fatalf("contribution replay => %d %+v %s", contributionRetry.StatusCode, contributionResponse, body)
	}
	tooManyRefs := make([]string, 257)
	for i := range tooManyRefs {
		tooManyRefs[i] = "deferred"
	}
	readSet, err := json.Marshal(map[string][]string{"manifest_items": tooManyRefs})
	if err != nil {
		t.Fatal(err)
	}
	oversizedReadSet := `{"target_kind":"artifact_version","target_id":"` + version.Record.ArtifactVersionID + `","contribution_kind":"review","role":"reviewer","read_set":` + string(readSet) + `,"body":"small"}`
	tooMany, body := request(contributionPath, oversizedReadSet, "too-many-read-refs", "contribution.write")
	if tooMany.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "read_set_too_large") {
		t.Fatalf("oversized read_set => %d %s", tooMany.StatusCode, body)
	}
	oversizedBody := `{"target_kind":"artifact_version","target_id":"` + version.Record.ArtifactVersionID + `","contribution_kind":"review","role":"reviewer","body":"` + strings.Repeat("x", 256<<10+1) + `"}`
	tooLarge, body := request(contributionPath, oversizedBody, "too-large-contribution", "contribution.write")
	if tooLarge.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(body), "contribution_body_too_large") {
		t.Fatalf("oversized contribution => %d %s", tooLarge.StatusCode, body)
	}
	foreign, body := request("/v1/projects/other/artifacts/"+artifact.ArtifactID+"/contributions", contributionReq, "foreign-contribution", "contribution.write")
	if foreign.StatusCode != http.StatusForbidden {
		t.Fatalf("project authorization status=%d body=%s", foreign.StatusCode, body)
	}
}
