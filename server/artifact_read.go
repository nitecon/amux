package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/nitecon/amux/storage"
)

type artifactChunkingStatus struct {
	Status               string   `json:"status"`
	CurrentChunkCount    int      `json:"current_chunk_count"`
	StaleChunkCount      int      `json:"stale_chunk_count"`
	SupersededChunkCount int      `json:"superseded_chunk_count"`
	FailedAddresses      []string `json:"failed_addresses"`
}

type artifactReadEnvelope[T any] struct {
	Data           T                      `json:"data"`
	ChunkingStatus artifactChunkingStatus `json:"chunking_status"`
}

type artifactVersionRead struct {
	storage.ArtifactVersion
	ChunkingStatus artifactChunkingStatus `json:"chunking_status"`
}

type artifactDetailRead struct {
	Artifact        storage.Artifact     `json:"artifact"`
	CurrentVersion  *artifactVersionRead `json:"current_version"`
	AcceptedVersion *artifactVersionRead `json:"accepted_version"`
}

type artifactDiffRead struct {
	FromVersionID  *string                `json:"from_version_id"`
	ToVersionID    string                 `json:"to_version_id"`
	Format         string                 `json:"format"`
	ByteDelta      int                    `json:"byte_delta"`
	Diff           string                 `json:"diff"`
	ChunkingStatus artifactChunkingStatus `json:"chunking_status"`
}

func (a *api) registerArtifactReadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/projects/{ident}/artifacts", a.listArtifacts)
	mux.HandleFunc("GET /v1/projects/{ident}/artifacts/{artifact_id}", a.getArtifact)
	mux.HandleFunc("GET /v1/projects/{ident}/artifacts/{artifact_id}/versions", a.listArtifactVersions)
	mux.HandleFunc("GET /v1/projects/{ident}/artifacts/{artifact_id}/versions/{version_id}", a.getArtifactVersion)
	mux.HandleFunc("GET /v1/projects/{ident}/artifacts/{artifact_id}/versions/{version_id}/diff", a.diffArtifactVersion)
	mux.HandleFunc("GET /v1/projects/{ident}/specs/{artifact_id}/manifest", a.getSpecManifest)
	mux.HandleFunc("GET /v1/projects/{ident}/specs/{artifact_id}/manifest/{manifest_item_id}", a.getSpecManifestItem)
	mux.HandleFunc("GET /v1/projects/{ident}/artifacts/{artifact_id}/comments", a.listArtifactComments)
	mux.HandleFunc("GET /v1/projects/{ident}/artifacts/{artifact_id}/contributions", a.listArtifactContributions)
	mux.HandleFunc("GET /v1/projects/{ident}/design-reviews/{artifact_id}/contributions", a.listDesignReviewContributions)
	mux.HandleFunc("GET /v1/projects/{ident}/artifacts/links", a.listArtifactLinks)
}

func (a *api) listArtifacts(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorizeArtifact(w, r, "artifact.read"); !ok {
		return
	}
	q := r.URL.Query()
	items, err := a.store.SearchArtifacts(r.Context(), r.PathValue("ident"), storage.ArtifactFilters{
		Kind: q.Get("kind"), Subkind: q.Get("subkind"), LifecycleState: q.Get("lifecycle_state"),
		Label: q.Get("label"), ActorID: q.Get("actor_id"), Query: q.Get("q"),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read artifacts")
		return
	}
	writeJSON(w, artifactReadEnvelope[[]storage.Artifact]{Data: items, ChunkingStatus: noArtifactChunks()})
}

func (a *api) getArtifact(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorizeArtifact(w, r, "artifact.read"); !ok {
		return
	}
	project, id := r.PathValue("ident"), r.PathValue("artifact_id")
	detail, err := a.store.ArtifactDetail(r.Context(), project, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read artifact")
		return
	}
	if detail == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	chunks, err := a.store.ListArtifactChunks(r.Context(), project, id, storage.ArtifactChunkFilter{IncludeSuperseded: true})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read artifact chunks")
		return
	}
	status := artifactStatus(detail.Artifact, chunks)
	var current, accepted *artifactVersionRead
	if detail.CurrentVersion != nil {
		current = &artifactVersionRead{ArtifactVersion: *detail.CurrentVersion, ChunkingStatus: statusForVersion(detail.Artifact, *detail.CurrentVersion, chunks)}
	}
	if detail.AcceptedVersion != nil {
		accepted = &artifactVersionRead{ArtifactVersion: *detail.AcceptedVersion, ChunkingStatus: statusForVersion(detail.Artifact, *detail.AcceptedVersion, chunks)}
	}
	writeJSON(w, artifactReadEnvelope[artifactDetailRead]{Data: artifactDetailRead{Artifact: detail.Artifact, CurrentVersion: current, AcceptedVersion: accepted}, ChunkingStatus: status})
}

func (a *api) listArtifactVersions(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorizeArtifact(w, r, "artifact.read"); !ok {
		return
	}
	project, id := r.PathValue("ident"), r.PathValue("artifact_id")
	artifact, err := a.store.Artifact(r.Context(), project, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read artifact")
		return
	}
	if artifact == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	versions, err := a.store.ArtifactVersions(r.Context(), project, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read artifact versions")
		return
	}
	chunks, err := a.store.ListArtifactChunks(r.Context(), project, id, storage.ArtifactChunkFilter{IncludeSuperseded: true})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read artifact chunks")
		return
	}
	data := make([]artifactVersionRead, 0, len(versions))
	for _, version := range versions {
		data = append(data, artifactVersionRead{ArtifactVersion: version, ChunkingStatus: statusForVersion(*artifact, version, chunks)})
	}
	writeJSON(w, artifactReadEnvelope[[]artifactVersionRead]{Data: data, ChunkingStatus: artifactStatus(*artifact, chunks)})
}

func (a *api) getArtifactVersion(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorizeArtifact(w, r, "artifact.read"); !ok {
		return
	}
	project, id, versionID := r.PathValue("ident"), r.PathValue("artifact_id"), r.PathValue("version_id")
	artifact, version, chunks, ok := a.readArtifactVersion(w, r, project, id, versionID)
	if !ok {
		return
	}
	status := artifactStatus(*artifact, chunks)
	writeJSON(w, artifactReadEnvelope[artifactVersionRead]{Data: artifactVersionRead{ArtifactVersion: *version, ChunkingStatus: status}, ChunkingStatus: status})
}

func (a *api) diffArtifactVersion(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorizeArtifact(w, r, "artifact.read"); !ok {
		return
	}
	project, id, versionID := r.PathValue("ident"), r.PathValue("artifact_id"), r.PathValue("version_id")
	artifact, target, chunks, ok := a.readArtifactVersion(w, r, project, id, versionID)
	if !ok {
		return
	}
	baseID := target.ParentVersionID
	if values, exists := r.URL.Query()["base_version_id"]; exists {
		if len(values) != 1 {
			writeError(w, http.StatusBadRequest, "invalid base_version_id")
			return
		}
		baseID = &values[0]
	}
	fromText := ""
	if baseID != nil {
		base, err := a.store.ArtifactVersion(r.Context(), project, id, *baseID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cannot read base artifact version")
			return
		}
		if base == nil {
			writeError(w, http.StatusNotFound, "base artifact_version not found")
			return
		}
		fromText = artifactBodyText(*base)
	}
	toText := artifactBodyText(*target)
	status := artifactStatus(*artifact, chunks)
	writeJSON(w, artifactReadEnvelope[artifactDiffRead]{Data: artifactDiffRead{FromVersionID: baseID, ToVersionID: target.ArtifactVersionID, Format: "unified", ByteDelta: len(toText) - len(fromText), Diff: simpleArtifactDiff(fromText, toText), ChunkingStatus: status}, ChunkingStatus: status})
}

func (a *api) readArtifactVersion(w http.ResponseWriter, r *http.Request, project, id, versionID string) (*storage.Artifact, *storage.ArtifactVersion, []storage.ArtifactChunk, bool) {
	return a.readArtifactVersionWithMissingToken(w, r, project, id, versionID, "artifact_version not found")
}

func (a *api) readArtifactVersionWithMissingToken(w http.ResponseWriter, r *http.Request, project, id, versionID, missingToken string) (*storage.Artifact, *storage.ArtifactVersion, []storage.ArtifactChunk, bool) {
	artifact, err := a.store.Artifact(r.Context(), project, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read artifact")
		return nil, nil, nil, false
	}
	if artifact == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return nil, nil, nil, false
	}
	version, err := a.store.ArtifactVersion(r.Context(), project, id, versionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read artifact version")
		return nil, nil, nil, false
	}
	if version == nil {
		writeError(w, http.StatusNotFound, missingToken)
		return nil, nil, nil, false
	}
	chunks, err := a.store.ListArtifactChunks(r.Context(), project, id, storage.ArtifactChunkFilter{ArtifactVersionID: versionID, IncludeSuperseded: true})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read artifact chunks")
		return nil, nil, nil, false
	}
	return artifact, version, chunks, true
}

func artifactBodyText(version storage.ArtifactVersion) string {
	if version.Body != nil {
		return *version.Body
	}
	if len(version.StructuredPayload) == 0 {
		return ""
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(version.StructuredPayload))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return string(version.StructuredPayload)
	}
	var pretty bytes.Buffer
	encoder := json.NewEncoder(&pretty)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return string(version.StructuredPayload)
	}
	return strings.TrimSuffix(pretty.String(), "\n")
}

func simpleArtifactDiff(from, to string) string {
	fromLines, toLines := strings.Split(strings.TrimSuffix(from, "\n"), "\n"), strings.Split(strings.TrimSuffix(to, "\n"), "\n")
	if from == "" {
		fromLines = nil
	}
	if to == "" {
		toLines = nil
	}
	var out strings.Builder
	out.WriteString("--- base\n+++ target\n")
	for _, line := range fromLines {
		if !containsArtifactLine(toLines, line) {
			out.WriteByte('-')
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	for _, line := range toLines {
		if !containsArtifactLine(fromLines, line) {
			out.WriteByte('+')
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	return out.String()
}
func containsArtifactLine(lines []string, target string) bool {
	for _, line := range lines {
		if line == target {
			return true
		}
	}
	return false
}

func artifactStatus(artifact storage.Artifact, chunks []storage.ArtifactChunk) artifactChunkingStatus {
	freshness := artifact.AcceptedVersionID
	if freshness == nil {
		freshness = artifact.CurrentVersionID
	}
	status := artifactChunkingStatus{Status: "none", FailedAddresses: []string{}}
	for _, chunk := range chunks {
		if chunk.SupersededByChunkID != nil {
			status.SupersededChunkCount++
		} else if freshness != nil && chunk.ArtifactVersionID == *freshness {
			status.CurrentChunkCount++
		} else {
			status.StaleChunkCount++
		}
		var metadata map[string]any
		if json.Unmarshal(chunk.Metadata, &metadata) == nil {
			value, ok := metadata["status"]
			if !ok {
				value = metadata["chunking_status"]
			}
			if text, _ := value.(string); text == "failed" {
				status.FailedAddresses = append(status.FailedAddresses, chunk.ChildAddress)
			}
		}
	}
	switch {
	case len(status.FailedAddresses) > 0:
		status.Status = "partial"
	case status.StaleChunkCount > 0:
		status.Status = "stale"
	case status.CurrentChunkCount > 0:
		status.Status = "current"
	}
	return status
}
func statusForVersion(artifact storage.Artifact, version storage.ArtifactVersion, chunks []storage.ArtifactChunk) artifactChunkingStatus {
	return artifactStatus(artifact, onlyVersionChunks(chunks, version.ArtifactVersionID))
}
func onlyVersionChunks(chunks []storage.ArtifactChunk, id string) []storage.ArtifactChunk {
	out := make([]storage.ArtifactChunk, 0)
	for _, c := range chunks {
		if c.ArtifactVersionID == id {
			out = append(out, c)
		}
	}
	return out
}
func noArtifactChunks() artifactChunkingStatus {
	return artifactChunkingStatus{Status: "none", FailedAddresses: []string{}}
}
