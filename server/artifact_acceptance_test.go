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

func TestArtifactAcceptanceRouteUsesMutationAuthorizationAndPersistsAcceptance(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "acceptance.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "accept-route", ChannelName: "accept-route", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	creator, err := store.ResolveArtifactActor(ctx, storage.ArtifactActorIdentity{ActorType: "user", DisplayName: "Creator"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CreateArtifact(ctx, storage.ArtifactInsert{ProjectIdent: "accept-route", Kind: "spec", Title: "Acceptance", CreatedByActorID: creator.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	version, err := store.CreateArtifactVersion(ctx, "accept-route", artifact.ArtifactID, storage.ArtifactVersionInsert{BodyFormat: "markdown", Body: stringPtr("spec body"), CreatedByActorID: creator.ActorID})
	if err != nil || version == nil || version.Record == nil {
		t.Fatalf("create version: %+v %v", version, err)
	}
	handler, err := NewHandlerWithConfig(store, "accept-key", HandlerConfig{ArtifactAuthEnforced: true})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/accept-route/specs/" + artifact.ArtifactID + "/accept"
	request := func(scopes string, withKey bool, versionID string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"version_id":"`+versionID+`","actor_display_name":"Reviewer"}`))
		req.Header.Set("Authorization", "Bearer accept-key")
		req.Header.Set("X-Agent-Project", "accept-route")
		req.Header.Set("X-Agent-Scopes", scopes)
		if withKey {
			req.Header.Set("Idempotency-Key", "accept-once")
		}
		req.Header.Set("X-Agent-Id", "reviewer-agent")
		req.Header.Set("X-Agent-System", "codex")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	denied := request("artifact_version.accept", true, version.Record.ArtifactVersionID)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("missing artifact.write scope => %d: %s", denied.Code, denied.Body.String())
	}
	deniedBeforeContext := request("artifact_version.accept", false, version.Record.ArtifactVersionID)
	if deniedBeforeContext.Code != http.StatusForbidden {
		t.Fatalf("scope authorization should precede mutation-context parsing: %d: %s", deniedBeforeContext.Code, deniedBeforeContext.Body.String())
	}
	scopes := "artifact_version.accept artifact.write"
	missingVersion := request(scopes, true, "missing-version")
	if missingVersion.Code != http.StatusNotFound || !strings.Contains(missingVersion.Body.String(), "version not found") {
		t.Fatalf("unknown version => %d: %s", missingVersion.Code, missingVersion.Body.String())
	}
	accepted := request(scopes, true, version.Record.ArtifactVersionID)
	if accepted.Code != http.StatusOK {
		t.Fatalf("accept => %d: %s", accepted.Code, accepted.Body.String())
	}
	var response struct {
		Data       storage.ArtifactContribution `json:"data"`
		Provenance struct {
			Replay         bool   `json:"replay"`
			IdempotencyKey string `json:"idempotency_key"`
			Authorization  struct {
				RequiredScopes []string `json:"required_scopes"`
			} `json:"authorization"`
			Generated map[string]string `json:"generated_resources"`
			Actor     struct {
				DisplayName string `json:"display_name"`
			} `json:"actor"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(accepted.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.TargetID != version.Record.ArtifactVersionID || response.Data.ContributionID == "" || response.Provenance.Replay || response.Provenance.IdempotencyKey != "accept-once" || response.Provenance.Actor.DisplayName != "Reviewer" || strings.Join(response.Provenance.Authorization.RequiredScopes, ",") != "artifact_version.accept,artifact.write" || response.Provenance.Generated["artifact_version_id"] != version.Record.ArtifactVersionID || response.Provenance.Generated["contribution_id"] != response.Data.ContributionID {
		t.Fatalf("unexpected acceptance envelope: %+v", response)
	}
	persisted, err := store.Artifact(ctx, "accept-route", artifact.ArtifactID)
	if err != nil || persisted.AcceptedVersionID == nil || *persisted.AcceptedVersionID != version.Record.ArtifactVersionID || persisted.CurrentVersionID == nil || *persisted.CurrentVersionID != version.Record.ArtifactVersionID {
		t.Fatalf("acceptance pointers not persisted: %+v %v", persisted, err)
	}
	persistedVersion, err := store.ArtifactVersion(ctx, "accept-route", artifact.ArtifactID, version.Record.ArtifactVersionID)
	if err != nil || persistedVersion == nil || persistedVersion.VersionState != "accepted" {
		t.Fatalf("version state not persisted: %+v %v", persistedVersion, err)
	}
	replayed := request(scopes, true, version.Record.ArtifactVersionID)
	var retry struct {
		Data storage.ArtifactContribution `json:"data"`
	}
	if replayed.Code != http.StatusOK || json.Unmarshal(replayed.Body.Bytes(), &retry) != nil || retry.Data.ContributionID != response.Data.ContributionID {
		t.Fatalf("acceptance retry => %d: %s", replayed.Code, replayed.Body.String())
	}
	contributions, err := store.ArtifactContributions(ctx, "accept-route", artifact.ArtifactID)
	if err != nil || len(contributions) != 1 || contributions[0].ContributionID != response.Data.ContributionID {
		t.Fatalf("acceptance audit missing: %+v %v", contributions, err)
	}
}
