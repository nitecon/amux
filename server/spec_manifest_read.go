package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/nitecon/amux/storage"
)

type specManifestItem struct {
	ManifestItemID     string          `json:"manifest_item_id"`
	Phase              *string         `json:"phase"`
	TaskCode           *string         `json:"task_code"`
	Team               *string         `json:"team"`
	Title              string          `json:"title"`
	Status             *string         `json:"status"`
	Dependencies       []string        `json:"dependencies"`
	Labels             []string        `json:"labels"`
	TouchSurface       []string        `json:"touch_surface"`
	AcceptanceCriteria []string        `json:"acceptance_criteria"`
	ValidationPlan     []string        `json:"validation_plan"`
	GatewayTaskID      *string         `json:"gateway_task_id"`
	SpecFile           *string         `json:"spec_file"`
	SpecBody           *string         `json:"spec_body"`
	Metadata           json.RawMessage `json:"metadata"`
}

type specManifestResponse struct {
	ArtifactID        string             `json:"artifact_id"`
	ArtifactVersionID string             `json:"artifact_version_id"`
	Manifest          json.RawMessage    `json:"manifest"`
	Items             []specManifestItem `json:"items"`
	StabilityPolicy   json.RawMessage    `json:"stability_policy"`
}

type specManifestItemWire struct {
	ManifestItemID     *string         `json:"manifest_item_id"`
	Phase              *string         `json:"phase"`
	TaskCode           *string         `json:"task_code"`
	Team               *string         `json:"team"`
	Title              *string         `json:"title"`
	Status             *string         `json:"status"`
	Dependencies       *[]string       `json:"dependencies"`
	Labels             *[]string       `json:"labels"`
	TouchSurface       *[]string       `json:"touch_surface"`
	AcceptanceCriteria *[]string       `json:"acceptance_criteria"`
	ValidationPlan     *[]string       `json:"validation_plan"`
	GatewayTaskID      *string         `json:"gateway_task_id"`
	SpecFile           *string         `json:"spec_file"`
	SpecBody           *string         `json:"spec_body"`
	Metadata           json.RawMessage `json:"metadata"`
}

type specManifestReadError struct {
	status int
	token  string
}

func (e *specManifestReadError) Error() string { return e.token }

func (a *api) getSpecManifest(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorizeArtifact(w, r, "artifact.read"); !ok {
		return
	}
	manifest, status, ok := a.readSpecManifest(w, r)
	if !ok {
		return
	}
	writeJSON(w, artifactReadEnvelope[specManifestResponse]{Data: manifest, ChunkingStatus: status})
}

func (a *api) getSpecManifestItem(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorizeArtifact(w, r, "artifact.read"); !ok {
		return
	}
	manifest, status, ok := a.readSpecManifest(w, r)
	if !ok {
		return
	}
	wanted := r.PathValue("manifest_item_id")
	for _, item := range manifest.Items {
		if item.ManifestItemID == wanted {
			writeJSON(w, artifactReadEnvelope[specManifestItem]{Data: item, ChunkingStatus: status})
			return
		}
	}
	writeError(w, http.StatusNotFound, "manifest_item not found")
}

func (a *api) readSpecManifest(w http.ResponseWriter, r *http.Request) (specManifestResponse, artifactChunkingStatus, bool) {
	project, artifactID := r.PathValue("ident"), r.PathValue("artifact_id")
	detail, err := a.store.ArtifactDetail(r.Context(), project, artifactID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read artifact")
		return specManifestResponse{}, noArtifactChunks(), false
	}
	if detail == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return specManifestResponse{}, noArtifactChunks(), false
	}
	if detail.Artifact.Kind != "spec" {
		writeError(w, http.StatusBadRequest, "artifact is not a spec")
		return specManifestResponse{}, noArtifactChunks(), false
	}

	versionID := ""
	if values, present := r.URL.Query()["version_id"]; present {
		if len(values) != 1 {
			writeError(w, http.StatusBadRequest, "invalid query")
			return specManifestResponse{}, noArtifactChunks(), false
		}
		versionID = values[0]
	} else if detail.Artifact.AcceptedVersionID != nil {
		versionID = *detail.Artifact.AcceptedVersionID
	} else if detail.Artifact.CurrentVersionID != nil {
		versionID = *detail.Artifact.CurrentVersionID
	} else {
		writeError(w, http.StatusNotFound, "spec version not found")
		return specManifestResponse{}, noArtifactChunks(), false
	}
	artifact, version, chunks, ok := a.readArtifactVersionWithMissingToken(w, r, project, artifactID, versionID, "version not found")
	if !ok {
		return specManifestResponse{}, noArtifactChunks(), false
	}
	manifest, err := decodeSpecManifest(artifactID, *version)
	if err != nil {
		var readErr *specManifestReadError
		if errors.As(err, &readErr) {
			writeError(w, readErr.status, readErr.token)
			return specManifestResponse{}, noArtifactChunks(), false
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return specManifestResponse{}, noArtifactChunks(), false
	}
	return manifest, statusForVersion(*artifact, *version, chunks), true
}

func decodeSpecManifest(artifactID string, version storage.ArtifactVersion) (specManifestResponse, error) {
	if len(bytes.TrimSpace(version.StructuredPayload)) == 0 {
		return specManifestResponse{}, &specManifestReadError{http.StatusBadRequest, "spec_manifest_missing"}
	}
	var payloadObject map[string]json.RawMessage
	if err := json.Unmarshal(version.StructuredPayload, &payloadObject); err != nil {
		return specManifestResponse{}, fmt.Errorf("invalid stored spec manifest payload: %w", err)
	}
	manifest := json.RawMessage(version.StructuredPayload)
	if value, ok := payloadObject["manifest"]; ok {
		manifest = value
	}
	itemsRaw, ok := payloadObject["items"]
	if !ok {
		return specManifestResponse{}, fmt.Errorf("invalid stored spec manifest items: expected array")
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(itemsRaw, &rawItems); err != nil || rawItems == nil {
		if err == nil {
			err = fmt.Errorf("expected array")
		}
		return specManifestResponse{}, fmt.Errorf("invalid stored spec manifest items: %w", err)
	}
	items := make([]specManifestItem, 0, len(rawItems))
	for _, raw := range rawItems {
		var wire specManifestItemWire
		if err := json.Unmarshal(raw, &wire); err != nil {
			return specManifestResponse{}, fmt.Errorf("invalid stored spec manifest items: %w", err)
		}
		if wire.ManifestItemID == nil || wire.Title == nil || wire.Dependencies == nil || wire.Labels == nil || wire.TouchSurface == nil || wire.AcceptanceCriteria == nil || wire.ValidationPlan == nil || len(wire.Metadata) == 0 {
			return specManifestResponse{}, fmt.Errorf("invalid stored spec manifest items: missing required field")
		}
		items = append(items, specManifestItem{
			ManifestItemID: *wire.ManifestItemID, Phase: wire.Phase, TaskCode: wire.TaskCode, Team: wire.Team,
			Title: *wire.Title, Status: wire.Status, Dependencies: *wire.Dependencies, Labels: *wire.Labels,
			TouchSurface: *wire.TouchSurface, AcceptanceCriteria: *wire.AcceptanceCriteria,
			ValidationPlan: *wire.ValidationPlan, GatewayTaskID: wire.GatewayTaskID, SpecFile: wire.SpecFile,
			SpecBody: wire.SpecBody, Metadata: wire.Metadata,
		})
	}
	policy, ok := payloadObject["stability_policy"]
	if !ok {
		policy = specManifestStabilityPolicy()
	}
	return specManifestResponse{ArtifactID: artifactID, ArtifactVersionID: version.ArtifactVersionID, Manifest: manifest, Items: items, StabilityPolicy: policy}, nil
}
