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

func TestSpecManifestReadRoutesSelectVersionAndReturnPersistedStatus(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "spec-manifest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "manifest-read", ChannelName: "manifest-read", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	creator, err := store.ResolveArtifactActor(ctx, storage.ArtifactActorIdentity{ActorType: "user", DisplayName: "Manifest test"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CreateArtifact(ctx, storage.ArtifactInsert{ProjectIdent: "manifest-read", Kind: "spec", Title: "Manifest", CreatedByActorID: creator.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateArtifactVersion(ctx, "manifest-read", artifact.ArtifactID, storage.ArtifactVersionInsert{
		BodyFormat: "markdown", Body: specManifestPtr("first body"), StructuredPayload: manifestPayload("First"), CreatedByActorID: creator.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateArtifactChunk(ctx, "manifest-read", artifact.ArtifactID, storage.ArtifactChunkInsert{
		ArtifactVersionID: first.Record.ArtifactVersionID, ChildAddress: "manifest.items[first]", Text: "first chunk",
	}); err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateArtifactVersion(ctx, "manifest-read", artifact.ArtifactID, storage.ArtifactVersionInsert{
		ParentVersionID: &first.Record.ArtifactVersionID, BodyFormat: "markdown", Body: specManifestPtr("second body"),
		StructuredPayload: manifestPayload("Second"), CreatedByActorID: creator.ActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := "accept-for-manifest-read"
	if _, err := store.AcceptArtifactVersion(ctx, "manifest-read", artifact.ArtifactID, first.Record.ArtifactVersionID, creator.ActorID, nil, &key); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandlerWithConfig(store, "manifest-key", HandlerConfig{ArtifactAuthEnforced: true})
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/projects/manifest-read/specs/" + artifact.ArtifactID + "/manifest"
	request := func(path, project, scopes string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer manifest-key")
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
	get := func(path string) artifactReadEnvelope[specManifestResponse] {
		t.Helper()
		res := request(path, "manifest-read", "artifact.read")
		if res.Code != http.StatusOK {
			t.Fatalf("GET %s => %d: %s", path, res.Code, res.Body.String())
		}
		var envelope artifactReadEnvelope[specManifestResponse]
		if err := json.Unmarshal(res.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope
	}
	accepted := get(base)
	if accepted.Data.ArtifactVersionID != first.Record.ArtifactVersionID || len(accepted.Data.Items) != 1 || accepted.Data.Items[0].Title != "First" || accepted.ChunkingStatus.CurrentChunkCount != 1 || accepted.ChunkingStatus.Status != "current" {
		t.Fatalf("default read did not select accepted version or persisted status: %+v", accepted)
	}
	current := get(base + "?version_id=" + second.Record.ArtifactVersionID)
	if current.Data.ArtifactVersionID != second.Record.ArtifactVersionID || current.Data.Items[0].Title != "Second" {
		t.Fatalf("explicit current version not selected: %+v", current.Data)
	}
	explicitAccepted := get(base + "?version_id=" + first.Record.ArtifactVersionID)
	if explicitAccepted.Data.ArtifactVersionID != first.Record.ArtifactVersionID {
		t.Fatalf("explicit accepted version not selected: %+v", explicitAccepted.Data)
	}
	item := request(base+"/first?version_id="+second.Record.ArtifactVersionID, "manifest-read", "artifact.read")
	if item.Code != http.StatusOK || !strings.Contains(item.Body.String(), `"title":"Second"`) || !strings.Contains(item.Body.String(), `"chunking_status":{"status":"none"`) {
		t.Fatalf("item read => %d: %s", item.Code, item.Body.String())
	}
	missingItem := request(base+"/absent", "manifest-read", "artifact.read")
	if missingItem.Code != http.StatusNotFound || !strings.Contains(missingItem.Body.String(), "manifest_item not found") {
		t.Fatalf("missing item => %d: %s", missingItem.Code, missingItem.Body.String())
	}
	for _, tc := range []struct {
		project, scopes string
		want            int
	}{{"", "", http.StatusForbidden}, {"other-project", "artifact.read", http.StatusForbidden}} {
		res := request(base, tc.project, tc.scopes)
		if res.Code != tc.want {
			t.Errorf("read auth project=%q scopes=%q => %d, want %d", tc.project, tc.scopes, res.Code, tc.want)
		}
	}
	missingVersion := request(base+"?version_id=missing", "manifest-read", "artifact.read")
	if missingVersion.Code != http.StatusNotFound || !strings.Contains(missingVersion.Body.String(), "version not found") {
		t.Fatalf("missing version => %d: %s", missingVersion.Code, missingVersion.Body.String())
	}
	currentOnly, err := store.CreateArtifact(ctx, storage.ArtifactInsert{ProjectIdent: "manifest-read", Kind: "spec", Title: "Current only", CreatedByActorID: creator.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	currentOnlyVersion, err := store.CreateArtifactVersion(ctx, "manifest-read", currentOnly.ArtifactID, storage.ArtifactVersionInsert{StructuredPayload: manifestPayload("Current only"), CreatedByActorID: creator.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	currentOnlyPath := "/v1/projects/manifest-read/specs/" + currentOnly.ArtifactID + "/manifest"
	currentOnlyEnvelope := get(currentOnlyPath)
	if currentOnlyEnvelope.Data.ArtifactVersionID != currentOnlyVersion.Record.ArtifactVersionID {
		t.Fatalf("current-version fallback not selected: %+v", currentOnlyEnvelope.Data)
	}
	noVersion, err := store.CreateArtifact(ctx, storage.ArtifactInsert{ProjectIdent: "manifest-read", Kind: "spec", Title: "No versions", CreatedByActorID: creator.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	noVersionPath := "/v1/projects/manifest-read/specs/" + noVersion.ArtifactID + "/manifest"
	noVersionResponse := request(noVersionPath, "manifest-read", "artifact.read")
	if noVersionResponse.Code != http.StatusNotFound || !strings.Contains(noVersionResponse.Body.String(), "spec version not found") {
		t.Fatalf("no version => %d: %s", noVersionResponse.Code, noVersionResponse.Body.String())
	}
}

func manifestPayload(title string) json.RawMessage {
	return json.RawMessage(`{"schema":"gateway.spec_manifest.v1","manifest":{"name":"` + title + `"},"items":[{"manifest_item_id":"first","title":"` + title + `","dependencies":[],"labels":[],"touch_surface":[],"acceptance_criteria":[],"validation_plan":[],"metadata":{"priority":1}}]}`)
}

func specManifestPtr(value string) *string { return &value }

func TestSpecManifestReadRoutesReturnSourceErrors(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "manifest-errors.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "manifest-errors", ChannelName: "manifest-errors", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	actor, err := store.ResolveArtifactActor(ctx, storage.ArtifactActorIdentity{ActorType: "user", DisplayName: "Error fixture"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CreateArtifact(ctx, storage.ArtifactInsert{ProjectIdent: "manifest-errors", Kind: "spec", Title: "No manifest", CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateArtifactVersion(ctx, "manifest-errors", artifact.ArtifactID, storage.ArtifactVersionInsert{Body: specManifestPtr("text only"), CreatedByActorID: actor.ActorID}); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(store, "manifest-key")
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/manifest-errors/specs/" + artifact.ArtifactID + "/manifest"
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer manifest-key")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "spec_manifest_missing") {
		t.Fatalf("missing stored manifest => %d: %s", res.Code, res.Body.String())
	}
	invalid, err := store.CreateArtifactVersion(ctx, artifact.ProjectIdent, artifact.ArtifactID, storage.ArtifactVersionInsert{
		CreatedByActorID: actor.ActorID, StructuredPayload: json.RawMessage(`{"items":[{"manifest_item_id":"T001"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "/T001"} {
		req := httptest.NewRequest(http.MethodGet, path+suffix+"?version_id="+invalid.Record.ArtifactVersionID, nil)
		req.Header.Set("Authorization", "Bearer manifest-key")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusInternalServerError || !strings.Contains(res.Body.String(), "invalid stored spec manifest items") {
			t.Errorf("invalid stored manifest on %q => %d: %s", suffix, res.Code, res.Body.String())
		}
	}
}
