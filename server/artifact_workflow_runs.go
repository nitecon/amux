package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/nitecon/amux/storage"
)

func (a *api) registerArtifactWorkflowRunRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/projects/{ident}/artifacts/{artifact_id}/workflow-runs", a.listArtifactWorkflowRuns)
	mux.HandleFunc("POST /v1/projects/{ident}/artifacts/{artifact_id}/workflow-runs", a.startArtifactWorkflowRun)
	mux.HandleFunc("GET /v1/projects/{ident}/artifacts/{artifact_id}/workflow-runs/{workflow_run_id}", a.getArtifactWorkflowRun)
	mux.HandleFunc("PATCH /v1/projects/{ident}/artifacts/{artifact_id}/workflow-runs/{workflow_run_id}", a.completeArtifactWorkflowRun)
}

type startArtifactWorkflowRunRequest struct {
	WorkflowKind            string          `json:"workflow_kind"`
	Phase                   *string         `json:"phase"`
	RoundID                 *string         `json:"round_id"`
	ParticipantActorIDs     []string        `json:"participant_actor_ids"`
	SourceArtifactVersionID *string         `json:"source_artifact_version_id"`
	ReadSet                 json.RawMessage `json:"read_set"`
	IsResumable             *bool           `json:"is_resumable"`
	ActorDisplayName        *string         `json:"actor_display_name"`
}

type completeArtifactWorkflowRunRequest struct {
	State                    string   `json:"state"`
	FailureReason            *string  `json:"failure_reason"`
	GeneratedContributionIDs []string `json:"generated_contribution_ids"`
	GeneratedVersionIDs      []string `json:"generated_version_ids"`
	GeneratedTaskIDs         []string `json:"generated_task_ids"`
	GeneratedLinkIDs         []string `json:"generated_link_ids"`
	GeneratedChunkIDs        []string `json:"generated_chunk_ids"`
	ActorDisplayName         *string  `json:"actor_display_name"`
}

func (a *api) startArtifactWorkflowRun(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "workflow_run.start")
	if !ok {
		return
	}
	var body startArtifactWorkflowRunRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	kind := strings.TrimSpace(body.WorkflowKind)
	if kind == "" {
		writeError(w, http.StatusBadRequest, "workflow_kind_required")
		return
	}
	switch kind {
	case "design_review_round", "spec_iteration", "spec_acceptance", "spec_task_generation":
	default:
		writeError(w, http.StatusBadRequest, "invalid_workflow_kind")
		return
	}
	if body.ParticipantActorIDs == nil {
		body.ParticipantActorIDs = []string{}
	}
	resumable := kind == "spec_task_generation"
	if body.IsResumable != nil {
		resumable = *body.IsResumable
	}
	project, artifactID := r.PathValue("ident"), r.PathValue("artifact_id")
	artifact, err := a.store.Artifact(r.Context(), project, artifactID)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if artifact == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	actor, err := a.resolveArtifactMutationActor(r.Context(), mutation, body.ActorDisplayName)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	write, err := a.store.StartWorkflowRun(r.Context(), storage.WorkflowRunInsert{
		ArtifactID: artifactID, WorkflowKind: kind, Phase: body.Phase, RoundID: body.RoundID,
		CoordinatorActorID: actor.ActorID, ParticipantActorIDs: body.ParticipantActorIDs,
		SourceArtifactVersionID: body.SourceArtifactVersionID, ReadSet: body.ReadSet,
		IdempotencyKey: &mutation.idempotencyKey, IsResumable: resumable,
	})
	if err != nil {
		if err.Error() == "read_set_too_large" {
			writeError(w, http.StatusBadRequest, "read_set_too_large")
			return
		}
		writeArtifactWriteError(w, err)
		return
	}
	if write == nil || write.Record == nil {
		writeError(w, http.StatusInternalServerError, "workflow run creation was not confirmed")
		return
	}
	writeArtifactMutation(w, artifactMutationStatus(write.Replayed), write.Record, actor, mutation, authorization,
		map[string]any{"workflow_run_id": write.Record.WorkflowRunID}, write.Replayed, nil)
}

func (a *api) completeArtifactWorkflowRun(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "workflow_run.complete")
	if !ok {
		return
	}
	var body completeArtifactWorkflowRunRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	state := strings.TrimSpace(body.State)
	if state == "" {
		writeError(w, http.StatusBadRequest, "state_required")
		return
	}
	switch state {
	case "succeeded", "failed", "cancelled":
	default:
		writeError(w, http.StatusBadRequest, "invalid_workflow_run_state")
		return
	}
	project, artifactID, runID := r.PathValue("ident"), r.PathValue("artifact_id"), r.PathValue("workflow_run_id")
	artifact, err := a.store.Artifact(r.Context(), project, artifactID)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if artifact == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	actor, err := a.resolveArtifactMutationActor(r.Context(), mutation, body.ActorDisplayName)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	run, err := a.store.ArtifactWorkflowRun(r.Context(), project, artifactID, runID)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "workflow_run not found")
		return
	}
	if run.CoordinatorActorID != actor.ActorID {
		writeError(w, http.StatusInternalServerError, "workflow_run coordinator mismatch")
		return
	}
	failureReason := body.FailureReason
	update := storage.WorkflowRunUpdate{
		State: &state, FailureReason: &failureReason,
		GeneratedContributionIDs: optionalWorkflowRunOutput(body.GeneratedContributionIDs),
		GeneratedVersionIDs:      optionalWorkflowRunOutput(body.GeneratedVersionIDs),
		GeneratedTaskIDs:         optionalWorkflowRunOutput(body.GeneratedTaskIDs),
		GeneratedLinkIDs:         optionalWorkflowRunOutput(body.GeneratedLinkIDs),
		GeneratedChunkIDs:        optionalWorkflowRunOutput(body.GeneratedChunkIDs),
	}
	updated, err := a.store.UpdateWorkflowRun(r.Context(), runID, update)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "workflow_run not found")
		return
	}
	generated := map[string]any{
		"workflow_run_id": updated.WorkflowRunID, "state": updated.State,
		"generated_contribution_ids": updated.GeneratedContributionIDs,
		"generated_version_ids":      updated.GeneratedVersionIDs,
		"generated_task_ids":         updated.GeneratedTaskIDs,
		"generated_link_ids":         updated.GeneratedLinkIDs,
		"generated_chunk_ids":        updated.GeneratedChunkIDs,
	}
	writeArtifactMutation(w, http.StatusOK, updated, actor, mutation, authorization, generated, false, nil)
}

func optionalWorkflowRunOutput(values []string) *[]string {
	if values == nil {
		return nil
	}
	return &values
}

func (a *api) listArtifactWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorizeArtifact(w, r, "artifact.read"); !ok {
		return
	}
	project, artifactID := r.PathValue("ident"), r.PathValue("artifact_id")
	artifact, err := a.store.Artifact(r.Context(), project, artifactID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read artifact")
		return
	}
	if artifact == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	runs, err := a.store.WorkflowRuns(r.Context(), project, artifactID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read workflow runs")
		return
	}
	writeJSON(w, artifactReadEnvelope[[]storage.WorkflowRun]{Data: runs, ChunkingStatus: noArtifactChunks()})
}

func (a *api) getArtifactWorkflowRun(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorizeArtifact(w, r, "artifact.read"); !ok {
		return
	}
	run, err := a.store.ArtifactWorkflowRun(r.Context(), r.PathValue("ident"), r.PathValue("artifact_id"), r.PathValue("workflow_run_id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read workflow run")
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "workflow_run not found")
		return
	}
	writeJSON(w, artifactReadEnvelope[storage.WorkflowRun]{Data: *run, ChunkingStatus: noArtifactChunks()})
}
