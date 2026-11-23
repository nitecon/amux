package server

import (
	"net/http"
	"strings"
)

type acceptArtifactVersionRequest struct {
	VersionID        string  `json:"version_id"`
	ActorDisplayName *string `json:"actor_display_name"`
}

func (a *api) registerArtifactAcceptanceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{ident}/specs/{artifact_id}/accept", a.acceptArtifactVersion)
}

func (a *api) acceptArtifactVersion(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "artifact_version.accept", "artifact.write")
	if !ok {
		return
	}
	var body acceptArtifactVersionRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	versionID := strings.TrimSpace(body.VersionID)
	if versionID == "" {
		writeError(w, http.StatusBadRequest, "version_id_required")
		return
	}
	projectIdent, artifactID := r.PathValue("ident"), r.PathValue("artifact_id")
	artifact, ok := a.loadProjectArtifactForMutation(w, r, projectIdent, artifactID)
	if !ok {
		return
	}
	if artifact.Kind != "spec" {
		writeError(w, http.StatusBadRequest, "artifact is not a spec")
		return
	}
	actor, err := a.resolveArtifactMutationActor(r.Context(), mutation, body.ActorDisplayName)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	contribution, err := a.store.AcceptArtifactVersion(r.Context(), projectIdent, artifactID, versionID, actor.ActorID, mutation.workflowRunID, &mutation.idempotencyKey)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if contribution == nil {
		writeError(w, http.StatusInternalServerError, "artifact acceptance was not confirmed")
		return
	}
	generated := map[string]any{"artifact_version_id": versionID, "contribution_id": contribution.ContributionID}
	writeArtifactMutation(w, http.StatusOK, contribution, actor, mutation, authorization,
		generated, false, nil)
}
