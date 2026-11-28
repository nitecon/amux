package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	projectid "github.com/nitecon/amux/identity/project"
)

const artifactBaseMigration = 37

// ArtifactActor is the durable attribution identity used by artifact records.
type ArtifactActor struct {
	ActorID          string          `json:"actor_id"`
	ActorType        string          `json:"actor_type"`
	AgentSystem      *string         `json:"agent_system"`
	AgentSystemLabel *string         `json:"agent_system_label"`
	AgentID          *string         `json:"agent_id"`
	Host             *string         `json:"host"`
	DisplayName      string          `json:"display_name"`
	RuntimeMetadata  json.RawMessage `json:"runtime_metadata"`
	CreatedAt        int64           `json:"created_at"`
	UpdatedAt        int64           `json:"updated_at"`
}

type ArtifactActorIdentity struct {
	ActorType        string
	AgentSystem      *string
	AgentSystemLabel *string
	AgentID          *string
	Host             *string
	DisplayName      string
	RuntimeMetadata  json.RawMessage
}

// Artifact is the mutable, project-owned container. Versions hold immutable bodies.
type Artifact struct {
	ArtifactID          string   `json:"artifact_id"`
	ProjectIdent        string   `json:"project_ident"`
	Kind                string   `json:"kind"`
	Subkind             *string  `json:"subkind"`
	Title               string   `json:"title"`
	Labels              []string `json:"labels"`
	LifecycleState      string   `json:"lifecycle_state"`
	ReviewState         string   `json:"review_state"`
	ImplementationState string   `json:"implementation_state"`
	CurrentVersionID    *string  `json:"current_version_id"`
	AcceptedVersionID   *string  `json:"accepted_version_id"`
	CreatedByActorID    string   `json:"created_by_actor_id"`
	CreatedAt           int64    `json:"created_at"`
	UpdatedAt           int64    `json:"updated_at"`
}

type ArtifactInsert struct {
	ProjectIdent     string
	Kind             string
	Subkind          *string
	Title            string
	Labels           []string
	CreatedByActorID string
}

type ArtifactDetail struct {
	Artifact        Artifact         `json:"artifact"`
	CurrentVersion  *ArtifactVersion `json:"current_version"`
	AcceptedVersion *ArtifactVersion `json:"accepted_version"`
}

// ArtifactVersion is an immutable snapshot with its Rust-compatible creator
// and optional workflow-run attribution fields.
type ArtifactVersion struct {
	ArtifactVersionID       string          `json:"artifact_version_id"`
	ArtifactID              string          `json:"artifact_id"`
	VersionLabel            *string         `json:"version_label"`
	ParentVersionID         *string         `json:"parent_version_id"`
	BodyFormat              string          `json:"body_format"`
	Body                    *string         `json:"body"`
	StructuredPayload       json.RawMessage `json:"structured_payload"`
	SourceFormat            *string         `json:"source_format"`
	CreatedByActorID        string          `json:"created_by_actor_id"`
	CreatedViaWorkflowRunID *string         `json:"created_via_workflow_run_id"`
	VersionState            string          `json:"version_state"`
	IdempotencyKey          *string         `json:"idempotency_key"`
	BodyPurgedAt            *int64          `json:"body_purged_at"`
	CreatedAt               int64           `json:"created_at"`
}

type ArtifactVersionInsert struct {
	VersionLabel            *string
	ParentVersionID         *string
	BodyFormat              string
	Body                    *string
	StructuredPayload       json.RawMessage
	SourceFormat            *string
	CreatedByActorID        string
	CreatedViaWorkflowRunID *string
	VersionState            string
	IdempotencyKey          *string
}

type ArtifactVersionWrite struct {
	Record   *ArtifactVersion
	Replayed bool
	Warnings []ArtifactQuotaWarning
}

// ArtifactWrite carries quota warnings alongside a created artifact.
type ArtifactWrite struct {
	Record   *Artifact
	Warnings []ArtifactQuotaWarning
}

func migrateArtifactBase(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS artifact_actors (
 actor_id TEXT PRIMARY KEY,
 actor_type TEXT NOT NULL CHECK(actor_type IN ('user','agent','system')),
 agent_system TEXT CHECK(agent_system IS NULL OR agent_system IN ('claude','codex','gemini','other')),
 agent_system_label TEXT,
 agent_id TEXT,
 host TEXT,
 display_name TEXT NOT NULL,
 runtime_metadata TEXT,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifact_actors_identity
 ON artifact_actors(actor_type,COALESCE(agent_system,''),COALESCE(agent_id,''),COALESCE(host,''));
CREATE TABLE IF NOT EXISTS artifacts (
 artifact_id TEXT PRIMARY KEY,
 project_ident TEXT NOT NULL REFERENCES projects(ident),
 kind TEXT NOT NULL CHECK(kind IN ('design_review','spec')),
 subkind TEXT,
 title TEXT NOT NULL,
 labels TEXT,
 lifecycle_state TEXT NOT NULL DEFAULT 'draft' CHECK(lifecycle_state IN ('draft','active','superseded','archived')),
 review_state TEXT NOT NULL DEFAULT 'none' CHECK(review_state IN ('none','collecting_reviews','synthesizing','needs_user_decision','accepted','rejected')),
 implementation_state TEXT NOT NULL DEFAULT 'not_applicable' CHECK(implementation_state IN ('not_applicable','not_started','in_progress','complete','blocked')),
 current_version_id TEXT REFERENCES artifact_versions(artifact_version_id) DEFERRABLE INITIALLY DEFERRED,
 accepted_version_id TEXT REFERENCES artifact_versions(artifact_version_id) DEFERRABLE INITIALLY DEFERRED,
 created_by_actor_id TEXT NOT NULL REFERENCES artifact_actors(actor_id),
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_artifacts_project_kind ON artifacts(project_ident,kind,updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_artifacts_project_lifecycle ON artifacts(project_ident,lifecycle_state,updated_at DESC);
CREATE TABLE IF NOT EXISTS artifact_versions (
 artifact_version_id TEXT PRIMARY KEY,
 artifact_id TEXT NOT NULL REFERENCES artifacts(artifact_id) ON DELETE CASCADE,
 version_label TEXT,
 parent_version_id TEXT REFERENCES artifact_versions(artifact_version_id),
 body_format TEXT NOT NULL CHECK(body_format='markdown'),
 body TEXT,
 structured_payload TEXT,
 source_format TEXT,
 created_by_actor_id TEXT NOT NULL REFERENCES artifact_actors(actor_id),
	created_via_workflow_run_id TEXT REFERENCES workflow_runs(workflow_run_id) DEFERRABLE INITIALLY DEFERRED,
 version_state TEXT NOT NULL DEFAULT 'draft' CHECK(version_state IN ('draft','under_review','accepted','superseded','rejected')),
 idempotency_key TEXT,
 body_purged_at INTEGER,
 created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_artifact_versions_by_artifact ON artifact_versions(artifact_id,created_at DESC);
CREATE INDEX IF NOT EXISTS idx_artifact_versions_state ON artifact_versions(artifact_id,version_state);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifact_versions_idempotency
 ON artifact_versions(artifact_id,created_via_workflow_run_id,idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE TRIGGER IF NOT EXISTS prevent_artifact_version_body_update
 BEFORE UPDATE OF body ON artifact_versions FOR EACH ROW
 WHEN OLD.body_purged_at IS NULL AND NEW.body IS NOT OLD.body
 AND NOT (NEW.body IS NULL AND NEW.body_purged_at IS NOT NULL)
 BEGIN SELECT RAISE(ABORT,'artifact_version body is immutable'); END;
CREATE TRIGGER IF NOT EXISTS prevent_artifact_version_body_repopulate
 BEFORE UPDATE OF body ON artifact_versions FOR EACH ROW
 WHEN OLD.body_purged_at IS NOT NULL AND NEW.body IS NOT NULL
 BEGIN SELECT RAISE(ABORT,'artifact_version body cannot be repopulated after purge'); END;
CREATE TRIGGER IF NOT EXISTS prevent_artifact_version_payload_update
 BEFORE UPDATE OF structured_payload ON artifact_versions FOR EACH ROW
 WHEN OLD.body_purged_at IS NULL AND NEW.structured_payload IS NOT OLD.structured_payload
 BEGIN SELECT RAISE(ABORT,'artifact_version structured_payload is immutable'); END;
CREATE TRIGGER IF NOT EXISTS prevent_artifact_version_meta_update
 BEFORE UPDATE OF body_format,source_format,parent_version_id,artifact_id,idempotency_key,created_via_workflow_run_id ON artifact_versions FOR EACH ROW
 WHEN NEW.body_format IS NOT OLD.body_format OR NEW.source_format IS NOT OLD.source_format
 OR NEW.parent_version_id IS NOT OLD.parent_version_id OR NEW.artifact_id IS NOT OLD.artifact_id
 OR NEW.idempotency_key IS NOT OLD.idempotency_key
 OR NEW.created_via_workflow_run_id IS NOT OLD.created_via_workflow_run_id
 BEGIN SELECT RAISE(ABORT,'artifact_version metadata is immutable'); END;
`)
	if err != nil {
		return fmt.Errorf("migrate artifact base schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)", artifactBaseMigration); err != nil {
		return fmt.Errorf("record artifact base migration: %w", err)
	}
	return nil
}

func (s *Store) ResolveArtifactActor(ctx context.Context, identity ArtifactActorIdentity) (*ArtifactActor, error) {
	if strings.TrimSpace(identity.DisplayName) == "" {
		return nil, fmt.Errorf("artifact actor display name is required")
	}
	if len(identity.RuntimeMetadata) > 0 && !json.Valid(identity.RuntimeMetadata) {
		return nil, fmt.Errorf("artifact actor runtime metadata must be valid JSON")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT actor_id FROM artifact_actors WHERE actor_type=? AND COALESCE(agent_system,'')=COALESCE(?,'') AND COALESCE(agent_id,'')=COALESCE(?,'') AND COALESCE(host,'')=COALESCE(?,'')`, identity.ActorType, identity.AgentSystem, identity.AgentID, identity.Host).Scan(&id)
	now := time.Now().UnixMilli()
	if err == sql.ErrNoRows {
		u, err := uuid.NewV7()
		if err != nil {
			return nil, err
		}
		id = u.String()
		_, err = tx.ExecContext(ctx, `INSERT INTO artifact_actors(actor_id,actor_type,agent_system,agent_system_label,agent_id,host,display_name,runtime_metadata,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, id, identity.ActorType, identity.AgentSystem, identity.AgentSystemLabel, identity.AgentID, identity.Host, identity.DisplayName, nullJSON(identity.RuntimeMetadata), now, now)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE artifact_actors SET agent_system_label=?,display_name=?,runtime_metadata=?,updated_at=? WHERE actor_id=?`, identity.AgentSystemLabel, identity.DisplayName, nullJSON(identity.RuntimeMetadata), now, id); err != nil {
			return nil, err
		}
	}
	actor, err := scanArtifactActor(tx.QueryRowContext(ctx, `SELECT actor_id,actor_type,agent_system,agent_system_label,agent_id,host,display_name,runtime_metadata,created_at,updated_at FROM artifact_actors WHERE actor_id=?`, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return actor, nil
}

func nullJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	var out bytes.Buffer
	if err := json.Compact(&out, value); err != nil {
		return nil
	}
	return out.String()
}

func (s *Store) CreateArtifact(ctx context.Context, input ArtifactInsert) (*Artifact, error) {
	write, err := s.CreateArtifactWrite(ctx, input)
	if err != nil || write == nil {
		return nil, err
	}
	return write.Record, nil
}

// CreateArtifactWrite creates an artifact and returns any soft quota warning.
// The quota count and insert share one write transaction.
func (s *Store) CreateArtifactWrite(ctx context.Context, input ArtifactInsert) (*ArtifactWrite, error) {
	limits, err := artifactWriteLimits()
	if err != nil {
		return nil, err
	}
	return s.createArtifactWriteWithLimits(ctx, input, limits)
}

func (s *Store) createArtifactWriteWithLimits(ctx context.Context, input ArtifactInsert, limits ArtifactWriteLimits) (*ArtifactWrite, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	write, err := createArtifactWriteTx(ctx, tx, input, limits)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return write, nil
}

func createArtifactWriteTx(ctx context.Context, tx *sql.Tx, input ArtifactInsert, limits ArtifactWriteLimits) (*ArtifactWrite, error) {
	if strings.TrimSpace(input.Title) == "" {
		return nil, fmt.Errorf("artifact title is required")
	}
	if len(input.Labels) == 0 {
		input.Labels = []string{}
	}
	if err := checkArtifactLabels(input.Labels, limits); err != nil {
		return nil, err
	}
	labels, err := json.Marshal(input.Labels)
	if err != nil {
		return nil, err
	}
	projectIdent := projectid.NormalizeIdent(input.ProjectIdent)
	if projectIdent == "" {
		return nil, fmt.Errorf("project ident is required")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	implementation := "not_applicable"
	if input.Kind == "spec" {
		implementation = "not_started"
	}
	warnings, err := checkArtifactQuota(ctx, tx, projectIdent, "artifact", limits)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO artifacts(artifact_id,project_ident,kind,subkind,title,labels,lifecycle_state,review_state,implementation_state,current_version_id,accepted_version_id,created_by_actor_id,created_at,updated_at) VALUES(?,?,?,?,?,?,'draft','none',?,NULL,NULL,?,?,?)`, id.String(), projectIdent, input.Kind, input.Subkind, input.Title, string(labels), implementation, input.CreatedByActorID, now, now)
	if err != nil {
		return nil, err
	}
	record, err := scanArtifact(tx.QueryRowContext(ctx, artifactSelect+` WHERE project_ident=? AND artifact_id=?`, projectIdent, id.String()))
	if err != nil {
		return nil, err
	}
	return &ArtifactWrite{Record: record, Warnings: warnings}, nil
}

func (s *Store) Artifact(ctx context.Context, projectIdent, artifactID string) (*Artifact, error) {
	return scanArtifact(s.db.QueryRowContext(ctx, artifactSelect+` WHERE project_ident=? AND artifact_id=?`, projectid.NormalizeIdent(projectIdent), artifactID))
}

func (s *Store) ArtifactDetail(ctx context.Context, projectIdent, artifactID string) (*ArtifactDetail, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	artifact, err := scanArtifact(tx.QueryRowContext(ctx, artifactSelect+` WHERE project_ident=? AND artifact_id=?`, projectid.NormalizeIdent(projectIdent), artifactID))
	if err != nil || artifact == nil {
		return nil, err
	}
	detail := &ArtifactDetail{Artifact: *artifact}
	if artifact.CurrentVersionID != nil {
		detail.CurrentVersion, err = readArtifactVersion(ctx, tx, artifactID, *artifact.CurrentVersionID)
		if err != nil {
			return nil, err
		}
	}
	if artifact.AcceptedVersionID != nil {
		detail.AcceptedVersion, err = readArtifactVersion(ctx, tx, artifactID, *artifact.AcceptedVersionID)
		if err != nil {
			return nil, err
		}
	}
	return detail, nil
}

const artifactSelect = `SELECT artifact_id,project_ident,kind,subkind,title,labels,lifecycle_state,review_state,implementation_state,current_version_id,accepted_version_id,created_by_actor_id,created_at,updated_at FROM artifacts`

func scanArtifact(row interface{ Scan(...any) error }) (*Artifact, error) {
	var item Artifact
	var labels sql.NullString
	if err := row.Scan(&item.ArtifactID, &item.ProjectIdent, &item.Kind, &item.Subkind, &item.Title, &labels, &item.LifecycleState, &item.ReviewState, &item.ImplementationState, &item.CurrentVersionID, &item.AcceptedVersionID, &item.CreatedByActorID, &item.CreatedAt, &item.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	item.Labels = []string{}
	if labels.Valid && labels.String != "" {
		if err := json.Unmarshal([]byte(labels.String), &item.Labels); err != nil {
			return nil, fmt.Errorf("decode artifact labels: %w", err)
		}
	}
	if item.Labels == nil {
		item.Labels = []string{}
	}
	return &item, nil
}

func scanArtifactActor(row interface{ Scan(...any) error }) (*ArtifactActor, error) {
	var item ArtifactActor
	var metadata sql.NullString
	if err := row.Scan(&item.ActorID, &item.ActorType, &item.AgentSystem, &item.AgentSystemLabel, &item.AgentID, &item.Host, &item.DisplayName, &metadata, &item.CreatedAt, &item.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if metadata.Valid {
		item.RuntimeMetadata = json.RawMessage(metadata.String)
	}
	return &item, nil
}

func (s *Store) CreateArtifactVersion(ctx context.Context, projectIdent, artifactID string, input ArtifactVersionInsert) (*ArtifactVersionWrite, error) {
	return s.createArtifactVersionWithLimits(ctx, projectIdent, artifactID, input, nil, false)
}

// CreateArtifactVersionWithWorkflow validates and completes an associated
// resumable run in the same transaction as the version write.
func (s *Store) CreateArtifactVersionWithWorkflow(ctx context.Context, projectIdent, artifactID string, input ArtifactVersionInsert) (*ArtifactVersionWrite, error) {
	return s.createArtifactVersionWithLimits(ctx, projectIdent, artifactID, input, nil, true)
}

func (s *Store) createArtifactVersionWithLimits(ctx context.Context, projectIdent, artifactID string, input ArtifactVersionInsert, suppliedLimits *ArtifactWriteLimits, workflowAtomic bool) (*ArtifactVersionWrite, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	write, err := createArtifactVersionTx(ctx, tx, projectIdent, artifactID, input, suppliedLimits, workflowAtomic)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return write, nil
}

func createArtifactVersionTx(ctx context.Context, tx *sql.Tx, projectIdent, artifactID string, input ArtifactVersionInsert, suppliedLimits *ArtifactWriteLimits, workflowAtomic bool) (*ArtifactVersionWrite, error) {
	projectIdent = projectid.NormalizeIdent(projectIdent)
	var err error
	if input.BodyFormat == "" {
		input.BodyFormat = "markdown"
	}
	if input.VersionState == "" {
		input.VersionState = "draft"
	}
	if input.BodyFormat != "markdown" {
		return nil, fmt.Errorf("invalid artifact version body format")
	}
	if input.VersionState != "draft" && input.VersionState != "under_review" && input.VersionState != "accepted" && input.VersionState != "superseded" && input.VersionState != "rejected" {
		return nil, fmt.Errorf("invalid artifact version state")
	}
	if len(input.StructuredPayload) > 0 && !json.Valid(input.StructuredPayload) {
		return nil, fmt.Errorf("structured payload must be valid JSON")
	}
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM artifacts WHERE project_ident=? AND artifact_id=?", projectIdent, artifactID).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if input.ParentVersionID != nil {
		var parent string
		if err := tx.QueryRowContext(ctx, "SELECT artifact_id FROM artifact_versions WHERE artifact_version_id=?", *input.ParentVersionID).Scan(&parent); err != nil || parent != artifactID {
			return nil, fmt.Errorf("parent version does not belong to artifact")
		}
	}
	if input.CreatedViaWorkflowRunID != nil && input.IdempotencyKey != nil {
		existing, err := scanArtifactVersion(tx.QueryRowContext(ctx, artifactVersionSelect+` WHERE artifact_id=? AND created_via_workflow_run_id=? AND idempotency_key=?`, artifactID, *input.CreatedViaWorkflowRunID, *input.IdempotencyKey))
		if err != nil {
			return nil, err
		}
		if existing != nil {
			if err := repairArtifactCurrentPointerForReplay(ctx, tx, artifactID, existing.ArtifactVersionID); err != nil {
				return nil, err
			}
			if workflowAtomic {
				if err := validateAndCompleteResumedWorkflowRunTx(ctx, tx, artifactID, input.CreatedViaWorkflowRunID, nil, &existing.ArtifactVersionID, nil); err != nil {
					return nil, err
				}
			}
			return &ArtifactVersionWrite{Record: existing, Replayed: true}, nil
		}
	}
	limits := ArtifactWriteLimits{}
	if suppliedLimits != nil {
		limits = *suppliedLimits
	} else {
		limits, err = artifactWriteLimits()
		if err != nil {
			return nil, err
		}
	}
	if err := checkArtifactVersionSize(input.Body, input.StructuredPayload, limits); err != nil {
		return nil, err
	}
	warnings, err := checkArtifactQuota(ctx, tx, projectIdent, "version", limits)
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	_, err = tx.ExecContext(ctx, `INSERT INTO artifact_versions(artifact_version_id,artifact_id,version_label,parent_version_id,body_format,body,structured_payload,source_format,created_by_actor_id,created_via_workflow_run_id,version_state,idempotency_key,body_purged_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,NULL,?)`, id.String(), artifactID, input.VersionLabel, input.ParentVersionID, input.BodyFormat, input.Body, nullJSON(input.StructuredPayload), input.SourceFormat, input.CreatedByActorID, input.CreatedViaWorkflowRunID, input.VersionState, input.IdempotencyKey, now)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE artifacts SET current_version_id=?,updated_at=? WHERE project_ident=? AND artifact_id=?", id.String(), now, projectIdent, artifactID); err != nil {
		return nil, err
	}
	item, err := readArtifactVersion(ctx, tx, artifactID, id.String())
	if err != nil {
		return nil, err
	}
	if workflowAtomic {
		if err := validateAndCompleteResumedWorkflowRunTx(ctx, tx, artifactID, input.CreatedViaWorkflowRunID, nil, &item.ArtifactVersionID, nil); err != nil {
			return nil, err
		}
	}
	return &ArtifactVersionWrite{Record: item, Warnings: warnings}, nil
}

func repairArtifactCurrentPointerForReplay(ctx context.Context, tx *sql.Tx, artifactID, replayedVersionID string) error {
	result, err := tx.ExecContext(ctx, `UPDATE artifacts SET current_version_id=?,updated_at=? WHERE artifact_id=? AND current_version_id IS NULL`, replayedVersionID, time.Now().UnixMilli(), artifactID)
	if err != nil {
		return err
	}
	_, err = result.RowsAffected()
	return err
}

const artifactVersionSelect = `SELECT artifact_version_id,artifact_id,version_label,parent_version_id,body_format,body,structured_payload,source_format,created_by_actor_id,created_via_workflow_run_id,version_state,idempotency_key,body_purged_at,created_at FROM artifact_versions`

func scanArtifactVersion(row interface{ Scan(...any) error }) (*ArtifactVersion, error) {
	var item ArtifactVersion
	var payload sql.NullString
	if err := row.Scan(&item.ArtifactVersionID, &item.ArtifactID, &item.VersionLabel, &item.ParentVersionID, &item.BodyFormat, &item.Body, &payload, &item.SourceFormat, &item.CreatedByActorID, &item.CreatedViaWorkflowRunID, &item.VersionState, &item.IdempotencyKey, &item.BodyPurgedAt, &item.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if payload.Valid {
		if !json.Valid([]byte(payload.String)) {
			return nil, fmt.Errorf("decode artifact version payload: invalid JSON")
		}
		item.StructuredPayload = json.RawMessage(payload.String)
	}
	return &item, nil
}

func (s *Store) ArtifactVersion(ctx context.Context, projectIdent, artifactID, versionID string) (*ArtifactVersion, error) {
	return scanArtifactVersion(s.db.QueryRowContext(ctx, artifactVersionSelect+`
 WHERE artifact_id=? AND artifact_version_id=?
 AND EXISTS(SELECT 1 FROM artifacts WHERE project_ident=? AND artifacts.artifact_id=artifact_versions.artifact_id)`,
		artifactID, versionID, projectid.NormalizeIdent(projectIdent)))
}

func readArtifactVersion(ctx context.Context, q queryer, artifactID, versionID string) (*ArtifactVersion, error) {
	return scanArtifactVersion(q.QueryRowContext(ctx, artifactVersionSelect+` WHERE artifact_id=? AND artifact_version_id=?`, artifactID, versionID))
}

func (s *Store) ArtifactVersions(ctx context.Context, projectIdent, artifactID string) ([]ArtifactVersion, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM artifacts WHERE project_ident=? AND artifact_id=?", projectid.NormalizeIdent(projectIdent), artifactID).Scan(&exists)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, artifactVersionSelect+` WHERE artifact_id=? ORDER BY created_at DESC`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ArtifactVersion{}
	for rows.Next() {
		item, err := scanArtifactVersion(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *item)
	}
	return result, rows.Err()
}
