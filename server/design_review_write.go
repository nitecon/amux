package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/nitecon/amux/storage"
)

type designReviewCreateRequest struct {
	Title                   string   `json:"title"`
	Labels                  []string `json:"labels"`
	Body                    *string  `json:"body"`
	BodyFormat              *string  `json:"body_format"`
	SourceArtifactID        *string  `json:"source_artifact_id"`
	SourceArtifactVersionID *string  `json:"source_artifact_version_id"`
	ActorDisplayName        *string  `json:"actor_display_name"`
}

type designReviewRoundRequest struct {
	RoundID                 *string         `json:"round_id"`
	ParticipantActorIDs     []string        `json:"participant_actor_ids"`
	SourceArtifactVersionID string          `json:"source_artifact_version_id"`
	ReadSet                 json.RawMessage `json:"read_set"`
	ActorDisplayName        *string         `json:"actor_display_name"`
}

type designReviewCreateResponse struct {
	Artifact storage.Artifact         `json:"artifact"`
	Version  *storage.ArtifactVersion `json:"version"`
}

type designReviewRoundResponse struct {
	WorkflowRun *storage.WorkflowRun `json:"workflow_run"`
	Artifact    *storage.Artifact    `json:"artifact"`
}

func (a *api) createDesignReview(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "artifact.write", "artifact_version.create")
	if !ok {
		return
	}
	var body designReviewCreateRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		writeError(w, http.StatusBadRequest, "title_required")
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
	payload, err := json.Marshal(map[string]any{
		"workflow": "design_review",
		"source": map[string]any{
			"artifact_id":         body.SourceArtifactID,
			"artifact_version_id": body.SourceArtifactVersionID,
		},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "design review payload unavailable")
		return
	}
	if a.disableArtifactBodySchema && len(payload) != 0 {
		writeError(w, http.StatusServiceUnavailable, "artifact_body_schema_disabled")
		return
	}
	projectIdent := r.PathValue("ident")
	project, err := a.store.Project(r.Context(), projectIdent)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if project == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	actor, err := a.resolveArtifactMutationActor(r.Context(), mutation, body.ActorDisplayName)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	artifactInput := storage.ArtifactInsert{
		ProjectIdent: projectIdent, Kind: "design_review", Title: title,
		Labels: body.Labels, CreatedByActorID: actor.ActorID,
	}
	var artifactWrite *storage.ArtifactWrite
	var versionWrite *storage.ArtifactVersionWrite
	if body.Body == nil {
		artifactWrite, err = a.store.CreateArtifactWrite(r.Context(), artifactInput)
	} else {
		versionLabel := "source"
		initialVersionKey := mutation.idempotencyKey + ":initial-version"
		versionWriteInput := storage.ArtifactVersionInsert{
			VersionLabel: &versionLabel, BodyFormat: bodyFormat, Body: body.Body,
			StructuredPayload: payload, CreatedByActorID: actor.ActorID, VersionState: "under_review",
			IdempotencyKey: &initialVersionKey,
		}
		artifactWrite, versionWrite, err = a.store.CreateSpecArtifactWithInitialVersion(r.Context(), artifactInput, versionWriteInput)
	}
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if artifactWrite == nil || artifactWrite.Record == nil {
		writeError(w, http.StatusInternalServerError, "design review creation was not confirmed")
		return
	}
	var version *storage.ArtifactVersion
	warnings := artifactWrite.Warnings
	var versionID *string
	if versionWrite != nil {
		version = versionWrite.Record
		versionID = &version.ArtifactVersionID
		warnings = append(warnings, versionWrite.Warnings...)
	}
	response := designReviewCreateResponse{Artifact: *artifactWrite.Record, Version: version}
	writeArtifactMutation(w, http.StatusCreated, response, actor, mutation, authorization,
		map[string]any{"artifact_id": artifactWrite.Record.ArtifactID, "artifact_version_id": versionID}, false, warnings)
}

func (a *api) createDesignReviewRound(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "workflow_run.start", "artifact.write")
	if !ok {
		return
	}
	var body designReviewRoundRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	sourceVersionID := strings.TrimSpace(body.SourceArtifactVersionID)
	if sourceVersionID == "" {
		writeError(w, http.StatusBadRequest, "source_artifact_version_id_required")
		return
	}
	roundID := "review-round:" + mutation.idempotencyKey
	if body.RoundID != nil && strings.TrimSpace(*body.RoundID) != "" {
		roundID = *body.RoundID
	}
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
	participants := body.ParticipantActorIDs
	if participants == nil {
		participants = []string{}
	}
	phase := "pass_1"
	write, err := a.store.StartWorkflowRun(r.Context(), storage.WorkflowRunInsert{
		ArtifactID: artifactID, WorkflowKind: "design_review_round", Phase: &phase,
		RoundID: &roundID, CoordinatorActorID: actor.ActorID, ParticipantActorIDs: participants,
		SourceArtifactVersionID: &sourceVersionID, ReadSet: body.ReadSet,
		IdempotencyKey: &mutation.idempotencyKey, IsResumable: false,
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if write == nil || write.Record == nil {
		writeError(w, http.StatusInternalServerError, "workflow run creation was not confirmed")
		return
	}
	updatedArtifact, err := a.store.MarkDesignReviewCollecting(r.Context(), projectIdent, artifactID)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if updatedArtifact == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	writeArtifactMutation(w, artifactMutationStatus(write.Replayed), designReviewRoundResponse{
		WorkflowRun: write.Record, Artifact: updatedArtifact,
	}, actor, mutation, authorization,
		map[string]any{"workflow_run_id": write.Record.WorkflowRunID}, write.Replayed, nil)
}
