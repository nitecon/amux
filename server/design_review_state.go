package server

import (
	"net/http"
)

type designReviewStateRequest struct {
	LifecycleState   *string `json:"lifecycle_state"`
	ReviewState      *string `json:"review_state"`
	Note             *string `json:"note"`
	ActorDisplayName *string `json:"actor_display_name"`
}

func (a *api) updateDesignReviewState(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "artifact.write", "contribution.write")
	if !ok {
		return
	}
	var body designReviewStateRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	if body.LifecycleState == nil && body.ReviewState == nil {
		writeError(w, http.StatusBadRequest, "state_required")
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
	note := "design review state transition"
	if body.Note != nil {
		note = *body.Note
	}
	write, err := a.store.UpdateDesignReviewState(r.Context(), projectIdent, artifactID,
		body.LifecycleState, body.ReviewState, actor.ActorID, note, mutation.workflowRunID, mutation.idempotencyKey)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if write == nil || write.Artifact == nil || write.Contribution == nil {
		writeError(w, http.StatusInternalServerError, "design review state transition was not confirmed")
		return
	}
	generated := map[string]any{
		"artifact_id":     write.Artifact.ArtifactID,
		"contribution_id": write.Contribution.ContributionID,
		"workflow_run_id": mutation.workflowRunID,
	}
	writeArtifactMutation(w, artifactMutationStatus(write.Replayed), map[string]any{
		"artifact": write.Artifact, "contribution": write.Contribution,
	}, actor, mutation, authorization, generated, write.Replayed, nil)
}
