package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/nitecon/amux/storage"
)

type designReviewSynthesisRequest struct {
	ReviewedVersionID *string         `json:"reviewed_version_id"`
	ReadSet           json.RawMessage `json:"read_set"`
	BodyFormat        *string         `json:"body_format"`
	Body              string          `json:"body"`
	CreateVersion     *bool           `json:"create_version"`
	VersionLabel      *string         `json:"version_label"`
	ActorDisplayName  *string         `json:"actor_display_name"`
}

func (a *api) createDesignReviewSynthesis(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "contribution.write", "artifact_version.create", "artifact.write")
	if !ok {
		return
	}
	workflowRunID := r.PathValue("workflow_run_id")
	mutation.workflowRunID = &workflowRunID
	var body designReviewSynthesisRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	if !designReviewReadSetHasContribution(body.ReadSet) {
		writeError(w, http.StatusBadRequest, "synthesis_read_set_contributions_required")
		return
	}
	bodyFormat := "markdown"
	if body.BodyFormat != nil {
		bodyFormat = *body.BodyFormat
	}
	if bodyFormat != "markdown" {
		writeError(w, http.StatusBadRequest, "invalid_body_format")
		return
	}
	synthesisBody := strings.TrimSpace(body.Body)
	if synthesisBody == "" {
		writeError(w, http.StatusBadRequest, "body_required")
		return
	}
	createVersion := body.CreateVersion == nil || *body.CreateVersion
	projectIdent, artifactID := r.PathValue("ident"), r.PathValue("artifact_id")
	artifact, ok := a.loadProjectArtifactForMutation(w, r, projectIdent, artifactID)
	if !ok {
		return
	}
	if artifact.Kind != "design_review" {
		writeError(w, http.StatusBadRequest, "artifact is not a design_review")
		return
	}
	actor, err := a.resolveArtifactMutationActor(r.Context(), mutation, body.ActorDisplayName)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	reviewedVersionID := valueOrEmpty(body.ReviewedVersionID)
	write, err := a.store.CreateDesignReviewSynthesis(r.Context(), projectIdent, artifactID, storage.DesignReviewSynthesisInsert{
		ReviewedVersionID: reviewedVersionID, ActorID: actor.ActorID,
		WorkflowRunID: workflowRunID, IdempotencyKey: mutation.idempotencyKey, ReadSet: normalizedArtifactPayload(body.ReadSet),
		BodyFormat: bodyFormat, Body: synthesisBody, CreateVersion: createVersion, VersionLabel: body.VersionLabel,
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if write == nil || write.Contribution == nil || write.Artifact == nil {
		writeError(w, http.StatusInternalServerError, "design review synthesis was not confirmed")
		return
	}
	data := map[string]any{"contribution": write.Contribution, "version": write.Version, "artifact": write.Artifact}
	generated := map[string]any{
		"contribution_id": write.Contribution.ContributionID, "artifact_version_id": nil,
		"workflow_run_id": workflowRunID,
	}
	if write.Version != nil {
		generated["artifact_version_id"] = write.Version.ArtifactVersionID
	}
	writeArtifactMutation(w, artifactMutationStatus(write.Replayed), data, actor, mutation, authorization, generated, write.Replayed, write.Warnings)
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
