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

func TestArtifactReadRoutesReturnRustEnvelopesAndPersistedChunkStatus(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "artifacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "artifact-read", ChannelName: "artifact-read", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	actor, err := store.ResolveArtifactActor(ctx, storage.ArtifactActorIdentity{ActorType: "user", DisplayName: "Tester"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CreateArtifact(ctx, storage.ArtifactInsert{ProjectIdent: "artifact-read", Kind: "spec", Title: "Read contract", Labels: []string{"api"}, CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	firstBody := "one\ntwo\n"
	first, err := store.CreateArtifactVersion(ctx, "artifact-read", artifact.ArtifactID, storage.ArtifactVersionInsert{Body: &firstBody, CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	secondBody := "one\nthree\n"
	second, err := store.CreateArtifactVersion(ctx, "artifact-read", artifact.ArtifactID, storage.ArtifactVersionInsert{ParentVersionID: &first.Record.ArtifactVersionID, Body: &secondBody, CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.CreateArtifactChunk(ctx, "artifact-read", artifact.ArtifactID, storage.ArtifactChunkInsert{
		ArtifactVersionID: first.Record.ArtifactVersionID, ChildAddress: "/spec/stale", Text: "stale",
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := store.ResolveArtifactActor(ctx, storage.ArtifactActorIdentity{ActorType: "agent", AgentID: artifactTestString("reviewer"), DisplayName: "Reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.AddArtifactContribution(ctx, "artifact-read", artifact.ArtifactID, storage.ArtifactContributionInsert{
		TargetKind: "artifact_version", TargetID: first.Record.ArtifactVersionID, ContributionKind: "review",
		Role: "reviewer", ActorID: reviewer.ActorID, Body: "monotonic token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddArtifactComment(ctx, "artifact-read", artifact.ArtifactID, storage.ArtifactCommentInsert{
		TargetKind: "artifact_version", TargetID: first.Record.ArtifactVersionID, ActorID: reviewer.ActorID, Body: "original discussion",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateArtifactLink(ctx, storage.ArtifactLinkInsert{
		LinkType: "doc_referenced_by_spec", SourceKind: "artifact", SourceID: artifact.ArtifactID,
		TargetKind: "external_url", TargetID: "https://example.test/linked-reference", CreatedByActorID: reviewer.ActorID,
	}); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(store, "artifact-key")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	request := func(path string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer artifact-key")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var body json.RawMessage
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return res, body
	}
	base := "/v1/projects/artifact-read/artifacts/" + artifact.ArtifactID
	list, body := request("/v1/projects/artifact-read/artifacts?q=contract&kind=spec")
	if list.StatusCode != http.StatusOK || !strings.Contains(string(body), `"chunking_status":{"status":"none"`) || !strings.Contains(string(body), artifact.ArtifactID) {
		t.Fatalf("list %d %s", list.StatusCode, body)
	}
	for _, filter := range []struct {
		query string
		match bool
	}{
		{"q=two&kind=spec&label=api&actor_id=" + actor.ActorID, true},
		{"q=three&lifecycle_state=draft", true},
		{"q=monotonic+token&actor_id=" + reviewer.ActorID, true},
		{"q=linked-reference&actor_id=" + reviewer.ActorID, true},
		{"q=two&label=missing", false},
		{"q=two&actor_id=other", false},
	} {
		response, body := request("/v1/projects/artifact-read/artifacts?" + filter.query)
		if response.StatusCode != http.StatusOK || strings.Contains(string(body), artifact.ArtifactID) != filter.match {
			t.Fatalf("filter %s status=%d body=%s", filter.query, response.StatusCode, body)
		}
	}
	stale, body := request(base)
	if stale.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"stale","current_chunk_count":0,"stale_chunk_count":1,"superseded_chunk_count":0,"failed_addresses":[]`) {
		t.Fatalf("stale detail %d %s", stale.StatusCode, body)
	}
	_, _, err = store.CreateArtifactChunk(ctx, "artifact-read", artifact.ArtifactID, storage.ArtifactChunkInsert{ArtifactVersionID: second.Record.ArtifactVersionID, ChildAddress: "/spec/summary", Text: "summary", Metadata: json.RawMessage(`{"status":"failed"}`)})
	if err != nil {
		t.Fatalf("seed persisted chunk (chunk migration must be registered): %v", err)
	}
	_, _, err = store.CreateArtifactChunk(ctx, "artifact-read", artifact.ArtifactID, storage.ArtifactChunkInsert{
		ArtifactVersionID: second.Record.ArtifactVersionID, ChildAddress: "/spec/alternate", Text: "alternate",
		Metadata: json.RawMessage(`{"status":null,"chunking_status":"failed"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	detail, body := request(base)
	if detail.StatusCode != http.StatusOK || !strings.Contains(string(body), `"current_version":{"artifact_version_id":"`+second.Record.ArtifactVersionID+`"`) || !strings.Contains(string(body), `"failed_addresses":["/spec/summary"]`) {
		t.Fatalf("detail %d %s", detail.StatusCode, body)
	}
	history, body := request(base + "/versions")
	if history.StatusCode != http.StatusOK || !strings.Contains(string(body), first.Record.ArtifactVersionID) || !strings.Contains(string(body), second.Record.ArtifactVersionID) {
		t.Fatalf("versions %d %s", history.StatusCode, body)
	}
	version, body := request(base + "/versions/" + second.Record.ArtifactVersionID)
	if version.StatusCode != http.StatusOK || strings.Count(string(body), `"chunking_status":{"status":"partial"`) != 2 || strings.Count(string(body), `"failed_addresses":["/spec/summary"]`) != 2 {
		t.Fatalf("version %d %s", version.StatusCode, body)
	}
	diff, body := request(base + "/versions/" + second.Record.ArtifactVersionID + "/diff")
	if diff.StatusCode != http.StatusOK || !strings.Contains(string(body), `"byte_delta":2`) || !strings.Contains(string(body), `-two\n+three\n`) || strings.Count(string(body), `"chunking_status":{"status":"partial"`) != 2 || !strings.Contains(string(body), `"failed_addresses":["/spec/summary"]`) {
		t.Fatalf("diff %d %s", diff.StatusCode, body)
	}
	missing, body := request("/v1/projects/other/artifacts/" + artifact.ArtifactID)
	if missing.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "artifact not found") {
		t.Fatalf("cross-project detail %d %s", missing.StatusCode, body)
	}
	for _, check := range []struct {
		path string
		want string
	}{
		{base + "/comments", "original discussion"},
		{base + "/contributions", "monotonic token"},
		{"/v1/projects/artifact-read/artifacts/links?source_id=" + artifact.ArtifactID, "linked-reference"},
	} {
		response, body := request(check.path)
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), check.want) ||
			!strings.Contains(string(body), `"chunking_status":{"status":"none"`) {
			t.Fatalf("read %s: %d %s", check.path, response.StatusCode, body)
		}
	}
	for _, suffix := range []string{"/comments", "/contributions"} {
		response, body := request("/v1/projects/other/artifacts/" + artifact.ArtifactID + suffix)
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("cross-project read %s: %d %s", suffix, response.StatusCode, body)
		}
	}
}

func artifactTestString(value string) *string { return &value }

func TestArtifactDiffPreservesStructuredBodyNumbersAndText(t *testing.T) {
	base := storage.ArtifactVersion{StructuredPayload: json.RawMessage(`{"count":18446744073709551614,"text":"<spec>"}`)}
	target := storage.ArtifactVersion{StructuredPayload: json.RawMessage(`{"count":18446744073709551615,"text":"<spec>"}`)}
	from, to := artifactBodyText(base), artifactBodyText(target)
	if !strings.Contains(from, `"<spec>"`) || !strings.Contains(to, `"<spec>"`) {
		t.Fatalf("structured body text changed: %s %s", from, to)
	}
	diff := simpleArtifactDiff(from, to)
	if !strings.Contains(diff, `-  "count": 18446744073709551614,`) || !strings.Contains(diff, `+  "count": 18446744073709551615,`) {
		t.Fatalf("integer change was lost in diff: %s", diff)
	}
}

func TestArtifactReadRoutesRequireExistingBearerAdmission(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "artifacts-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler, err := NewHandler(store, "artifact-key")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/projects/p/artifacts", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated read status=%d body=%s", res.Code, res.Body.String())
	}
}
