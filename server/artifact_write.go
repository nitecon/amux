package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/storage"
)

const artifactMutationBodyLimit = 2 << 20

type artifactMutationContext struct {
	actor          storage.ArtifactActorIdentity
	idempotencyKey string
	workflowRunID  *string
	requestID      string
}

type artifactMutationActorResponse struct {
	ActorID     string  `json:"actor_id"`
	ActorType   string  `json:"actor_type"`
	AgentSystem *string `json:"agent_system"`
	AgentID     *string `json:"agent_id"`
	Host        *string `json:"host"`
	DisplayName string  `json:"display_name"`
}

type artifactMutationAuthorization struct {
	Boundary       string   `json:"boundary"`
	RequiredScopes []string `json:"required_scopes"`
}

type artifactMutationProvenance struct {
	Actor          artifactMutationActorResponse `json:"actor"`
	WorkflowRunID  *string                       `json:"workflow_run_id"`
	IdempotencyKey string                        `json:"idempotency_key"`
	RequestID      string                        `json:"request_id"`
	CreatedAt      int64                         `json:"created_at"`
	Authorization  artifactMutationAuthorization `json:"authorization"`
	Generated      map[string]any                `json:"generated_resources"`
	Replay         bool                          `json:"replay"`
	Warnings       []string                      `json:"warnings"`
}

type artifactMutationResponse[T any] struct {
	Data       T                          `json:"data"`
	Provenance artifactMutationProvenance `json:"provenance"`
}

// beginArtifactMutation is the shared entry point for artifact writes. It
// establishes project scope, gates an explicit quota override, then parses
// the actor and idempotency envelope. Caller supplied gateway user headers
// never participate in actor resolution.
func (a *api) beginArtifactMutation(w http.ResponseWriter, r *http.Request, scopes ...string) (artifactMutationContext, artifactAuthorization, bool) {
	authorization, ok := a.authorizeArtifactMutation(w, r, scopes...)
	if !ok {
		return artifactMutationContext{}, artifactAuthorization{}, false
	}
	mutation, err := parseArtifactMutationContext(r)
	if err != nil {
		var inputErr *artifactMutationInputError
		if errors.As(err, &inputErr) {
			writeError(w, inputErr.status, inputErr.token)
		} else {
			writeError(w, http.StatusInternalServerError, "artifact mutation context unavailable")
		}
		return artifactMutationContext{}, artifactAuthorization{}, false
	}
	return mutation, authorization, true
}

type artifactMutationInputError struct {
	status int
	token  string
}

func (e *artifactMutationInputError) Error() string { return e.token }

func parseArtifactMutationContext(r *http.Request) (artifactMutationContext, error) {
	if authenticatedPrincipal(r.Context()) == "" {
		return artifactMutationContext{}, &artifactMutationInputError{status: http.StatusUnauthorized, token: "authentication_required"}
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		return artifactMutationContext{}, &artifactMutationInputError{status: http.StatusBadRequest, token: "idempotency_key_required"}
	}
	if len(key) > 255 {
		return artifactMutationContext{}, &artifactMutationInputError{status: http.StatusBadRequest, token: "idempotency_key_too_long"}
	}
	agentID := strings.TrimSpace(r.Header.Get("X-Agent-Id"))
	if agentID == "" || agentID == "_default" {
		return artifactMutationContext{}, &artifactMutationInputError{status: http.StatusBadRequest, token: "x_agent_id_required"}
	}
	actorType := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Actor-Type")))
	if actorType == "" {
		actorType = "agent"
	}
	if actorType != "agent" && actorType != "system" {
		// This server has no authenticated browser-user principal. Never let a
		// caller turn arbitrary actor headers into a user/admin attribution.
		return artifactMutationContext{}, &artifactMutationInputError{status: http.StatusForbidden, token: "untrusted_artifact_actor_type"}
	}
	var agentSystem *string
	if value := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Agent-System"))); value != "" {
		switch value {
		case "claude", "codex", "gemini", "other":
			agentSystem = &value
		default:
			return artifactMutationContext{}, &artifactMutationInputError{status: http.StatusBadRequest, token: "invalid_x_agent_system"}
		}
	}
	if actorType == "agent" && agentSystem == nil {
		return artifactMutationContext{}, &artifactMutationInputError{status: http.StatusBadRequest, token: "x_agent_system_required"}
	}
	agentIDValue := agentID
	var host *string
	if value := strings.TrimSpace(r.Header.Get("X-Host")); value != "" {
		host = &value
	}
	var workflowRunID *string
	if value := strings.TrimSpace(r.Header.Get("X-Workflow-Run-ID")); value != "" {
		workflowRunID = &value
	}
	requestID, err := uuid.NewV7()
	if err != nil {
		return artifactMutationContext{}, fmt.Errorf("create artifact mutation request identity: %w", err)
	}
	return artifactMutationContext{
		actor: storage.ArtifactActorIdentity{
			ActorType: actorType, AgentSystem: agentSystem, AgentID: &agentIDValue,
			Host: host, DisplayName: agentID,
		},
		idempotencyKey: key,
		workflowRunID:  workflowRunID,
		requestID:      requestID.String(),
	}, nil
}

func (a *api) resolveArtifactMutationActor(ctx context.Context, mutation artifactMutationContext, displayName *string) (*storage.ArtifactActor, error) {
	identity := mutation.actor
	if displayName != nil && strings.TrimSpace(*displayName) != "" {
		identity.DisplayName = strings.TrimSpace(*displayName)
	}
	return a.store.ResolveArtifactActor(ctx, identity)
}

func writeArtifactMutation[T any](w http.ResponseWriter, status int, data T, actor *storage.ArtifactActor, mutation artifactMutationContext, authorization artifactAuthorization, generated map[string]any, replayed bool, warnings []storage.ArtifactQuotaWarning) {
	warningTokens := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		warningTokens = append(warningTokens, warning.Token())
	}
	provenance := artifactMutationProvenance{
		Actor: artifactMutationActorResponse{
			ActorID: actor.ActorID, ActorType: actor.ActorType, AgentSystem: actor.AgentSystem,
			AgentID: actor.AgentID, Host: actor.Host, DisplayName: actor.DisplayName,
		},
		WorkflowRunID: mutation.workflowRunID, IdempotencyKey: mutation.idempotencyKey,
		RequestID: mutation.requestID, CreatedAt: time.Now().UnixMilli(),
		Authorization: artifactMutationAuthorization{Boundary: authorization.Boundary, RequiredScopes: authorization.RequiredScopes},
		Generated:     generated, Replay: replayed, Warnings: warningTokens,
	}
	writeJSONStatus(w, status, artifactMutationResponse[T]{Data: data, Provenance: provenance})
}

type createArtifactRequest struct {
	Kind             string   `json:"kind"`
	Subkind          *string  `json:"subkind"`
	Title            string   `json:"title"`
	Labels           []string `json:"labels"`
	ActorDisplayName *string  `json:"actor_display_name"`
}

type createArtifactVersionRequest struct {
	VersionLabel      *string         `json:"version_label"`
	ParentVersionID   *string         `json:"parent_version_id"`
	BodyFormat        *string         `json:"body_format"`
	Body              *string         `json:"body"`
	StructuredPayload json.RawMessage `json:"structured_payload"`
	SourceFormat      *string         `json:"source_format"`
	VersionState      *string         `json:"version_state"`
	ActorDisplayName  *string         `json:"actor_display_name"`
}

func (a *api) registerArtifactWriteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{ident}/artifacts", a.createArtifact)
	mux.HandleFunc("POST /v1/projects/{ident}/artifacts/{artifact_id}/versions", a.createArtifactVersion)
	mux.HandleFunc("POST /v1/projects/{ident}/design-reviews", a.createDesignReview)
	mux.HandleFunc("POST /v1/projects/{ident}/design-reviews/{artifact_id}/rounds", a.createDesignReviewRound)
	mux.HandleFunc("POST /v1/projects/{ident}/design-reviews/{artifact_id}/rounds/{workflow_run_id}/contributions", a.createDesignReviewContribution)
	mux.HandleFunc("POST /v1/projects/{ident}/design-reviews/{artifact_id}/rounds/{workflow_run_id}/synthesis", a.createDesignReviewSynthesis)
	mux.HandleFunc("POST /v1/projects/{ident}/design-reviews/{artifact_id}/state", a.updateDesignReviewState)
	mux.HandleFunc("POST /v1/projects/{ident}/specs", a.createSpec)
	mux.HandleFunc("POST /v1/projects/{ident}/specs/{artifact_id}/versions", a.createSpecVersion)
}

func (a *api) createArtifact(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "artifact.write")
	if !ok {
		return
	}
	if mutation.workflowRunID != nil {
		writeError(w, http.StatusBadRequest, "workflow_run_not_supported_for_artifact_creation")
		return
	}
	var body createArtifactRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	kind := strings.TrimSpace(body.Kind)
	if kind != "spec" && kind != "design_review" {
		writeError(w, http.StatusBadRequest, "invalid_artifact_kind")
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		writeError(w, http.StatusBadRequest, "title_required")
		return
	}
	actor, err := a.resolveArtifactMutationActor(r.Context(), mutation, body.ActorDisplayName)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	write, err := a.store.CreateArtifactWrite(r.Context(), storage.ArtifactInsert{
		ProjectIdent: r.PathValue("ident"), Kind: kind, Subkind: body.Subkind,
		Title: title, Labels: body.Labels, CreatedByActorID: actor.ActorID,
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if write == nil || write.Record == nil {
		writeError(w, http.StatusInternalServerError, "artifact creation was not confirmed")
		return
	}
	writeArtifactMutation(w, http.StatusCreated, write.Record, actor, mutation, authorization,
		map[string]any{"artifact_id": write.Record.ArtifactID}, false, write.Warnings)
}

func (a *api) createArtifactVersion(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "artifact_version.create")
	if !ok {
		return
	}
	var body createArtifactVersionRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
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
	payload := normalizedArtifactPayload(body.StructuredPayload)
	if a.disableArtifactBodySchema && len(payload) != 0 {
		writeError(w, http.StatusServiceUnavailable, "artifact_body_schema_disabled")
		return
	}
	versionState := "draft"
	if body.VersionState != nil {
		versionState = *body.VersionState
	}
	switch versionState {
	case "draft", "under_review", "accepted", "superseded", "rejected":
	default:
		writeError(w, http.StatusBadRequest, "invalid_version_state")
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
	write, err := a.store.CreateArtifactVersionWithWorkflow(r.Context(), projectIdent, artifactID, storage.ArtifactVersionInsert{
		VersionLabel: body.VersionLabel, ParentVersionID: body.ParentVersionID, BodyFormat: bodyFormat,
		Body: body.Body, StructuredPayload: payload, SourceFormat: body.SourceFormat,
		CreatedByActorID: actor.ActorID, CreatedViaWorkflowRunID: mutation.workflowRunID,
		VersionState: versionState, IdempotencyKey: &mutation.idempotencyKey,
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if write == nil || write.Record == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	writeArtifactMutation(w, artifactMutationStatus(write.Replayed), write.Record, actor, mutation, authorization,
		map[string]any{"artifact_version_id": write.Record.ArtifactVersionID, "workflow_run_id": mutation.workflowRunID}, write.Replayed, write.Warnings)
}

func normalizedArtifactPayload(payload json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(payload)) == 0 || bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
		return nil
	}
	return payload
}

func artifactMutationStatus(replayed bool) int {
	if replayed {
		return http.StatusOK
	}
	return http.StatusCreated
}

func writeArtifactWriteError(w http.ResponseWriter, err error) {
	var sizeErr *storage.ArtifactSizeLimitError
	if errors.As(err, &sizeErr) {
		status := http.StatusRequestEntityTooLarge
		if sizeErr.Kind == "labels_too_many" || sizeErr.Kind == "label_too_long" || sizeErr.Kind == "read_set_too_large" {
			status = http.StatusBadRequest
		}
		writeError(w, status, sizeErr.Token())
		return
	}
	var quotaErr *storage.ArtifactQuotaError
	if errors.As(err, &quotaErr) {
		writeError(w, http.StatusTooManyRequests, quotaErr.Token())
		return
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "not found"):
		writeError(w, http.StatusNotFound, err.Error())
	case strings.Contains(message, "invalid"), strings.Contains(message, "required"), strings.Contains(message, "does not belong"), strings.Contains(message, "not retryable"), strings.Contains(message, "cannot"):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "artifact mutation failed")
	}
}

// loadProjectArtifactForMutation centralizes the common project-scoped lookup
// and its not-found/error response. Callers keep their own validation order,
// kind checks, actor resolution, and transaction or workflow fencing.
func (a *api) loadProjectArtifactForMutation(w http.ResponseWriter, r *http.Request, projectIdent, artifactID string) (*storage.Artifact, bool) {
	artifact, err := a.store.Artifact(r.Context(), projectIdent, artifactID)
	if err != nil {
		writeArtifactWriteError(w, err)
		return nil, false
	}
	if artifact == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return nil, false
	}
	return artifact, true
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
