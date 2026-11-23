package server

import (
	"errors"
	"net/http"

	"github.com/nitecon/amux/storage"
)

func (a *api) listArtifactComments(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorizeArtifact(w, r, "artifact.read"); !ok {
		return
	}
	items, err := a.store.ArtifactComments(r.Context(), r.PathValue("ident"), r.PathValue("artifact_id"))
	if err != nil {
		writeRelatedArtifactReadError(w, err)
		return
	}
	writeJSON(w, artifactReadEnvelope[[]storage.ArtifactComment]{Data: items, ChunkingStatus: noArtifactChunks()})
}

func (a *api) listArtifactContributions(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorizeArtifact(w, r, "artifact.read"); !ok {
		return
	}
	items, err := a.store.ArtifactContributions(r.Context(), r.PathValue("ident"), r.PathValue("artifact_id"))
	if err != nil {
		writeRelatedArtifactReadError(w, err)
		return
	}
	writeJSON(w, artifactReadEnvelope[[]storage.ArtifactContribution]{Data: items, ChunkingStatus: noArtifactChunks()})
}

func (a *api) listArtifactLinks(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorizeArtifact(w, r, "artifact.read"); !ok {
		return
	}
	query := r.URL.Query()
	items, err := a.store.ListArtifactLinks(r.Context(), r.PathValue("ident"), storage.ArtifactLinkFilter{
		LinkType: query.Get("link_type"), SourceKind: query.Get("source_kind"), SourceID: query.Get("source_id"),
		TargetKind: query.Get("target_kind"), TargetID: query.Get("target_id"),
	})
	if err != nil {
		writeRelatedArtifactReadError(w, err)
		return
	}
	writeJSON(w, artifactReadEnvelope[[]storage.ArtifactLink]{Data: items, ChunkingStatus: noArtifactChunks()})
}

func writeRelatedArtifactReadError(w http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrArtifactNotFound) {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	writeError(w, http.StatusInternalServerError, "cannot read artifact records")
}
