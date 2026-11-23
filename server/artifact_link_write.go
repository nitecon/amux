package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/nitecon/amux/storage"
)

type createArtifactLinkRequest struct {
	LinkType           string  `json:"link_type"`
	SourceKind         string  `json:"source_kind"`
	SourceID           string  `json:"source_id"`
	SourceVersionID    *string `json:"source_version_id"`
	SourceChildAddress *string `json:"source_child_address"`
	TargetKind         string  `json:"target_kind"`
	TargetID           string  `json:"target_id"`
	TargetVersionID    *string `json:"target_version_id"`
	TargetChildAddress *string `json:"target_child_address"`
	SupersedesLinkID   *string `json:"supersedes_link_id"`
	ActorDisplayName   *string `json:"actor_display_name"`
}

func (a *api) registerArtifactLinkWriteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{ident}/artifacts/links", a.createArtifactLink)
}

func (a *api) createArtifactLink(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "link.write")
	if !ok {
		return
	}
	var body createArtifactLinkRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	trim := func(value *string) *string {
		if value == nil {
			return nil
		}
		trimmed := strings.TrimSpace(*value)
		return &trimmed
	}
	linkType, ok := trimArtifactWriteField(w, body.LinkType, "link_type")
	if !ok {
		return
	}
	sourceKind, ok := trimArtifactWriteField(w, body.SourceKind, "source_kind")
	if !ok {
		return
	}
	sourceID, ok := trimArtifactWriteField(w, body.SourceID, "source_id")
	if !ok {
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
	project, err := a.store.Project(r.Context(), r.PathValue("ident"))
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
	write, err := a.store.CreateArtifactLinkForProject(r.Context(), r.PathValue("ident"), storage.ArtifactLinkInsert{
		LinkType: linkType, SourceKind: sourceKind, SourceID: sourceID,
		SourceVersionID: trim(body.SourceVersionID), SourceChildAddress: trim(body.SourceChildAddress),
		TargetKind: targetKind, TargetID: targetID,
		TargetVersionID: trim(body.TargetVersionID), TargetChildAddress: trim(body.TargetChildAddress),
		CreatedByActorID: actor.ActorID, CreatedViaWorkflowRunID: mutation.workflowRunID,
		IdempotencyKey: &mutation.idempotencyKey, SupersedesLinkID: trim(body.SupersedesLinkID),
	})
	if err != nil {
		if errors.Is(err, storage.ErrArtifactLinkProjectMismatch) {
			writeError(w, http.StatusForbidden, "artifact_authorization_forbidden: artifact link is outside the requested project")
			return
		}
		writeArtifactWriteError(w, err)
		return
	}
	if write == nil || write.Record == nil {
		writeError(w, http.StatusInternalServerError, "artifact link was not confirmed")
		return
	}
	writeArtifactMutation(w, artifactMutationStatus(write.Replayed), write.Record, actor, mutation, authorization,
		map[string]any{"link_id": write.Record.LinkID}, write.Replayed, write.Warnings)
}
