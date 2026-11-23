package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/nitecon/amux/storage"
)

type createArtifactCommentRequest struct {
	TargetKind       string  `json:"target_kind"`
	TargetID         string  `json:"target_id"`
	ChildAddress     *string `json:"child_address"`
	ParentCommentID  *string `json:"parent_comment_id"`
	Body             string  `json:"body"`
	ActorDisplayName *string `json:"actor_display_name"`
}

type resolveArtifactCommentRequest struct {
	ResolutionNote   *string `json:"resolution_note"`
	ActorDisplayName *string `json:"actor_display_name"`
}

type reopenArtifactCommentRequest struct {
	NoteBody         *string `json:"note_body"`
	ActorDisplayName *string `json:"actor_display_name"`
}

type createArtifactContributionRequest struct {
	TargetKind       string          `json:"target_kind"`
	TargetID         string          `json:"target_id"`
	ContributionKind string          `json:"contribution_kind"`
	Phase            *string         `json:"phase"`
	Role             string          `json:"role"`
	ReadSet          json.RawMessage `json:"read_set"`
	BodyFormat       *string         `json:"body_format"`
	Body             string          `json:"body"`
	ActorDisplayName *string         `json:"actor_display_name"`
}

func (a *api) registerArtifactDiscourseWriteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{ident}/artifacts/{artifact_id}/comments", a.createArtifactComment)
	mux.HandleFunc("POST /v1/projects/{ident}/artifacts/{artifact_id}/comments/{comment_id}/resolve", a.resolveArtifactComment)
	mux.HandleFunc("POST /v1/projects/{ident}/artifacts/{artifact_id}/comments/{comment_id}/reopen", a.reopenArtifactComment)
	mux.HandleFunc("POST /v1/projects/{ident}/artifacts/{artifact_id}/contributions", a.createArtifactContribution)
}

func (a *api) createArtifactComment(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "comment.write")
	if !ok {
		return
	}
	var body createArtifactCommentRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	targetKind, ok := trimArtifactWriteField(w, body.TargetKind, "target_kind")
	if !ok {
		return
	}
	targetID, ok := trimArtifactWriteField(w, body.TargetID, "target_id")
	if !ok {
		return
	}
	commentBody, ok := trimArtifactWriteField(w, body.Body, "body")
	if !ok {
		return
	}
	projectIdent, artifactID := r.PathValue("ident"), r.PathValue("artifact_id")
	_, ok = a.loadProjectArtifactForMutation(w, r, projectIdent, artifactID)
	if !ok {
		return
	}
	actor, err := a.resolveArtifactMutationActor(r.Context(), mutation, body.ActorDisplayName)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if _, err := a.store.ValidateWorkflowRunForMutation(r.Context(), artifactID, mutation.workflowRunID); err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	comment, replayed, err := a.store.AddArtifactComment(r.Context(), projectIdent, artifactID, storage.ArtifactCommentInsert{
		TargetKind: targetKind, TargetID: targetID, ChildAddress: body.ChildAddress,
		ParentCommentID: body.ParentCommentID, ActorID: actor.ActorID, Body: commentBody,
		IdempotencyKey: &mutation.idempotencyKey,
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if comment == nil {
		writeError(w, http.StatusInternalServerError, "artifact comment was not confirmed")
		return
	}
	writeArtifactMutation(w, artifactMutationStatus(replayed), comment, actor, mutation, authorization,
		map[string]any{"comment_id": comment.CommentID}, replayed, nil)
}

func (a *api) resolveArtifactComment(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "comment.resolve")
	if !ok {
		return
	}
	var body resolveArtifactCommentRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	projectIdent, artifactID := r.PathValue("ident"), r.PathValue("artifact_id")
	actor, err := a.resolveArtifactMutationActor(r.Context(), mutation, body.ActorDisplayName)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if _, err := a.store.ValidateWorkflowRunForMutation(r.Context(), artifactID, mutation.workflowRunID); err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	comment, err := a.store.ResolveArtifactComment(r.Context(), projectIdent, artifactID, r.PathValue("comment_id"), storage.ArtifactCommentResolve{
		ActorID: actor.ActorID, WorkflowRunID: mutation.workflowRunID, Note: body.ResolutionNote,
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if comment == nil {
		writeError(w, http.StatusNotFound, "comment not found")
		return
	}
	writeArtifactMutation(w, http.StatusOK, comment, actor, mutation, authorization, map[string]any{"comment_id": comment.CommentID}, false, nil)
}

func (a *api) reopenArtifactComment(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "comment.write")
	if !ok {
		return
	}
	var body reopenArtifactCommentRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	projectIdent, artifactID := r.PathValue("ident"), r.PathValue("artifact_id")
	actor, err := a.resolveArtifactMutationActor(r.Context(), mutation, body.ActorDisplayName)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if _, err := a.store.ValidateWorkflowRunForMutation(r.Context(), artifactID, mutation.workflowRunID); err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	noteBody := "reopened comment"
	if body.NoteBody != nil {
		noteBody = *body.NoteBody
	}
	comment, err := a.store.ReopenArtifactComment(r.Context(), projectIdent, artifactID, r.PathValue("comment_id"), actor.ActorID, noteBody, &mutation.idempotencyKey)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if comment == nil {
		writeError(w, http.StatusNotFound, "comment not found")
		return
	}
	writeArtifactMutation(w, http.StatusOK, comment, actor, mutation, authorization, map[string]any{"comment_id": comment.CommentID}, false, nil)
}

func (a *api) createArtifactContribution(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "contribution.write")
	if !ok {
		return
	}
	var body createArtifactContributionRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	targetKind, ok := trimArtifactWriteField(w, body.TargetKind, "target_kind")
	if !ok {
		return
	}
	targetID, ok := trimArtifactWriteField(w, body.TargetID, "target_id")
	if !ok {
		return
	}
	contributionKind, ok := trimArtifactWriteField(w, body.ContributionKind, "contribution_kind")
	if !ok {
		return
	}
	if len(body.ReadSet) == 0 || strings.TrimSpace(string(body.ReadSet)) == "null" {
		body.ReadSet = nil
	}
	if body.ReadSet == nil && (body.Phase != nil && (*body.Phase == "pass_2" || *body.Phase == "synthesis") || contributionKind == "synthesis") {
		writeError(w, http.StatusBadRequest, "read_set_required")
		return
	}
	role, ok := trimArtifactWriteField(w, body.Role, "role")
	if !ok {
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
	contributionBody, ok := trimArtifactWriteField(w, body.Body, "body")
	if !ok {
		return
	}
	projectIdent, artifactID := r.PathValue("ident"), r.PathValue("artifact_id")
	_, ok = a.loadProjectArtifactForMutation(w, r, projectIdent, artifactID)
	if !ok {
		return
	}
	actor, err := a.resolveArtifactMutationActor(r.Context(), mutation, body.ActorDisplayName)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	write, replayed, err := a.store.AddArtifactContributionWithWorkflow(r.Context(), projectIdent, artifactID, storage.ArtifactContributionInsert{
		TargetKind: targetKind, TargetID: targetID, ContributionKind: contributionKind, Phase: body.Phase,
		Role: role, ActorID: actor.ActorID, WorkflowRunID: mutation.workflowRunID,
		ReadSet: body.ReadSet, BodyFormat: bodyFormat, Body: contributionBody, IdempotencyKey: &mutation.idempotencyKey,
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if write == nil {
		writeError(w, http.StatusInternalServerError, "artifact contribution was not confirmed")
		return
	}
	writeArtifactMutation(w, artifactMutationStatus(replayed), write, actor, mutation, authorization,
		map[string]any{"contribution_id": write.ContributionID, "workflow_run_id": mutation.workflowRunID}, replayed, nil)
}

func trimArtifactWriteField(w http.ResponseWriter, value, field string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		writeError(w, http.StatusBadRequest, field+"_required")
		return "", false
	}
	return trimmed, true
}
