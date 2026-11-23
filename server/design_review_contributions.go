package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/nitecon/amux/storage"
)

type designReviewContributionRequest struct {
	Phase            string          `json:"phase"`
	Role             *string         `json:"role"`
	ReviewedVersion  *string         `json:"reviewed_version_id"`
	ReadSet          json.RawMessage `json:"read_set"`
	BodyFormat       *string         `json:"body_format"`
	Body             string          `json:"body"`
	ActorDisplayName *string         `json:"actor_display_name"`
}

func (a *api) createDesignReviewContribution(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "contribution.write")
	if !ok {
		return
	}
	workflowRunID := r.PathValue("workflow_run_id")
	mutation.workflowRunID = &workflowRunID
	var body designReviewContributionRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	phase := strings.TrimSpace(body.Phase)
	if phase == "" {
		writeError(w, http.StatusBadRequest, "phase_required")
		return
	}
	role := "reviewer"
	if body.Role != nil {
		role = *body.Role
	}
	role = strings.TrimSpace(role)
	if role == "" {
		writeError(w, http.StatusBadRequest, "role_required")
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
	contributionBody := strings.TrimSpace(body.Body)
	if contributionBody == "" {
		writeError(w, http.StatusBadRequest, "body_required")
		return
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
	run, err := a.store.ValidateWorkflowRunForMutation(r.Context(), artifactID, &workflowRunID)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "workflow run not found")
		return
	}
	if run.WorkflowKind != "design_review_round" {
		writeError(w, http.StatusBadRequest, "workflow run is not a design_review_round")
		return
	}
	reviewedVersionID := run.SourceArtifactVersionID
	if body.ReviewedVersion != nil {
		reviewedVersionID = body.ReviewedVersion
	}
	if reviewedVersionID == nil {
		writeError(w, http.StatusBadRequest, "reviewed version required")
		return
	}
	readSet := normalizedArtifactPayload(body.ReadSet)
	if len(readSet) == 0 {
		readSet, _ = json.Marshal(map[string][]string{"versions": {*reviewedVersionID}})
	}
	if phase != "pass_1" && phase != "pass_2" {
		writeError(w, http.StatusBadRequest, "invalid_review_phase")
		return
	}
	if phase == "pass_2" && !designReviewReadSetHasContribution(readSet) {
		writeError(w, http.StatusBadRequest, "pass_2_read_set_contributions_required")
		return
	}
	phaseValue := phase
	contribution, replayed, err := a.store.AddDesignReviewContributionWithWorkflow(r.Context(), projectIdent, artifactID, workflowRunID, storage.ArtifactContributionInsert{
		TargetKind: "artifact_version", TargetID: *reviewedVersionID, ContributionKind: "review", Phase: &phaseValue,
		Role: role, ActorID: actor.ActorID, WorkflowRunID: &workflowRunID, ReadSet: readSet,
		BodyFormat: bodyFormat, Body: contributionBody, IdempotencyKey: &mutation.idempotencyKey,
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if contribution == nil {
		writeError(w, http.StatusInternalServerError, "artifact contribution was not confirmed")
		return
	}
	writeArtifactMutation(w, artifactMutationStatus(replayed), contribution, actor, mutation, authorization,
		map[string]any{"contribution_id": contribution.ContributionID, "workflow_run_id": workflowRunID}, replayed, nil)
}

func designReviewReadSetHasContribution(readSet json.RawMessage) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(readSet, &object) != nil || object == nil {
		return false
	}
	for _, key := range []string{"contribution", "contributions"} {
		var values []json.RawMessage
		if json.Unmarshal(object[key], &values) != nil {
			continue
		}
		for _, raw := range values {
			var value string
			if json.Unmarshal(raw, &value) == nil {
				return true
			}
		}
	}
	return false
}

func (a *api) listDesignReviewContributions(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorizeArtifact(w, r, "artifact.read"); !ok {
		return
	}
	query := r.URL.Query()
	items, err := a.store.ListDesignReviewContributions(r.Context(), r.PathValue("ident"), r.PathValue("artifact_id"), storage.DesignReviewContributionFilter{
		RoundID: query.Get("round_id"), Phase: query.Get("phase"), Role: query.Get("role"),
		ReviewedVersionID: query.Get("reviewed_version_id"), ReadSetContains: query.Get("read_set_contains"),
	})
	if err != nil {
		if errors.Is(err, storage.ErrArtifactNotDesignReview) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeRelatedArtifactReadError(w, err)
		return
	}
	writeJSON(w, artifactReadEnvelope[[]storage.ArtifactContribution]{Data: items, ChunkingStatus: noArtifactChunks()})
}
