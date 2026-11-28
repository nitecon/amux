package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	projectid "github.com/nitecon/amux/identity/project"
)

// Artifact links follow workflow runs (39) and artifact discourse (40).
// Registration in store.go is coordinated with those migrations.
const artifactLinksMigration = 41

type ArtifactLink struct {
	LinkID                  string  `json:"link_id"`
	LinkType                string  `json:"link_type"`
	SourceKind              string  `json:"source_kind"`
	SourceID                string  `json:"source_id"`
	SourceVersionID         *string `json:"source_version_id"`
	SourceChildAddress      *string `json:"source_child_address"`
	TargetKind              string  `json:"target_kind"`
	TargetID                string  `json:"target_id"`
	TargetVersionID         *string `json:"target_version_id"`
	TargetChildAddress      *string `json:"target_child_address"`
	CreatedByActorID        string  `json:"created_by_actor_id"`
	CreatedViaWorkflowRunID *string `json:"created_via_workflow_run_id"`
	IdempotencyKey          *string `json:"idempotency_key"`
	SupersedesLinkID        *string `json:"supersedes_link_id"`
	CreatedAt               int64   `json:"created_at"`
}

type ArtifactLinkInsert struct {
	LinkType                string
	SourceKind              string
	SourceID                string
	SourceVersionID         *string
	SourceChildAddress      *string
	TargetKind              string
	TargetID                string
	TargetVersionID         *string
	TargetChildAddress      *string
	CreatedByActorID        string
	CreatedViaWorkflowRunID *string
	IdempotencyKey          *string
	SupersedesLinkID        *string
}

type ArtifactLinkFilter struct {
	LinkType, SourceKind, SourceID, TargetKind, TargetID string
}

type ArtifactLinkWrite struct {
	Record   *ArtifactLink
	Replayed bool
	Warnings []ArtifactQuotaWarning
}

func migrateArtifactLinks(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS artifact_links (
 link_id TEXT PRIMARY KEY,
 link_type TEXT NOT NULL,
 source_kind TEXT NOT NULL CHECK(source_kind IN ('artifact','artifact_version','contribution','comment','task','chunk','pattern','memory','commit','external_url')),
 source_id TEXT NOT NULL,
 source_version_id TEXT REFERENCES artifact_versions(artifact_version_id),
 source_child_address TEXT,
 target_kind TEXT NOT NULL CHECK(target_kind IN ('artifact','artifact_version','contribution','comment','task','chunk','pattern','memory','commit','external_url')),
 target_id TEXT NOT NULL,
 target_version_id TEXT REFERENCES artifact_versions(artifact_version_id),
 target_child_address TEXT,
 created_by_actor_id TEXT NOT NULL REFERENCES artifact_actors(actor_id),
 created_via_workflow_run_id TEXT REFERENCES workflow_runs(workflow_run_id),
 idempotency_key TEXT,
 supersedes_link_id TEXT REFERENCES artifact_links(link_id),
 created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_artifact_links_source ON artifact_links(source_kind,source_id);
CREATE INDEX IF NOT EXISTS idx_artifact_links_target ON artifact_links(target_kind,target_id);
CREATE INDEX IF NOT EXISTS idx_artifact_links_type ON artifact_links(link_type,created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifact_links_idempotency
 ON artifact_links(created_via_workflow_run_id,idempotency_key)
 WHERE created_via_workflow_run_id IS NOT NULL AND idempotency_key IS NOT NULL;`)
	if err != nil {
		return fmt.Errorf("migrate artifact links: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)", artifactLinksMigration); err != nil {
		return fmt.Errorf("record artifact links migration: %w", err)
	}
	return nil
}

const artifactLinkSelect = `SELECT link_id,link_type,source_kind,source_id,source_version_id,source_child_address,target_kind,target_id,target_version_id,target_child_address,created_by_actor_id,created_via_workflow_run_id,idempotency_key,supersedes_link_id,created_at FROM artifact_links`

func scanArtifactLink(row interface{ Scan(...any) error }) (*ArtifactLink, error) {
	var item ArtifactLink
	if err := row.Scan(&item.LinkID, &item.LinkType, &item.SourceKind, &item.SourceID, &item.SourceVersionID, &item.SourceChildAddress,
		&item.TargetKind, &item.TargetID, &item.TargetVersionID, &item.TargetChildAddress, &item.CreatedByActorID,
		&item.CreatedViaWorkflowRunID, &item.IdempotencyKey, &item.SupersedesLinkID, &item.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

func validateArtifactLink(ctx context.Context, q queryer, input ArtifactLinkInsert) error {
	if input.CreatedViaWorkflowRunID != nil && input.IdempotencyKey == nil {
		return fmt.Errorf("workflow-emitted links require idempotency_key")
	}
	sourceRequired, targetRequired := false, false
	switch input.LinkType {
	case "spec_implements_design", "supersedes_version":
		sourceRequired, targetRequired = true, true
	case "task_generated_from_spec", "chunk_of_version", "pattern_applied_to_artifact_version":
		targetRequired = true
	}
	if sourceRequired && input.SourceVersionID == nil {
		return fmt.Errorf("audit-path link requires source_version_id")
	}
	if targetRequired && input.TargetVersionID == nil {
		return fmt.Errorf("audit-path link requires target_version_id")
	}
	if input.LinkType == "supersedes_artifact" {
		sourceProject, err := artifactProjectIdent(ctx, q, input.SourceID)
		if err != nil {
			return err
		}
		targetProject, err := artifactProjectIdent(ctx, q, input.TargetID)
		if err != nil {
			return err
		}
		if sourceProject == "" || sourceProject != targetProject {
			return fmt.Errorf("supersedes_artifact requires artifacts in the same project")
		}
	}
	return nil
}

func artifactProjectIdent(ctx context.Context, q queryer, artifactID string) (string, error) {
	var project string
	err := q.QueryRowContext(ctx, "SELECT project_ident FROM artifacts WHERE artifact_id=?", artifactID).Scan(&project)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return project, err
}

func projectForArtifactLink(ctx context.Context, q queryer, input ArtifactLinkInsert) (string, error) {
	var project string
	var err error
	if input.SourceKind == "artifact" {
		project, err = artifactProjectIdent(ctx, q, input.SourceID)
	} else if input.TargetKind == "artifact" {
		project, err = artifactProjectIdent(ctx, q, input.TargetID)
	} else if input.SourceVersionID != nil || input.TargetVersionID != nil {
		versionID := input.SourceVersionID
		if versionID == nil {
			versionID = input.TargetVersionID
		}
		err = q.QueryRowContext(ctx, `SELECT a.project_ident FROM artifact_versions v
JOIN artifacts a ON a.artifact_id=v.artifact_id WHERE v.artifact_version_id=?`, *versionID).Scan(&project)
		if err == sql.ErrNoRows {
			return "", nil
		}
	} else {
		return "", nil
	}
	return project, err
}

func (s *Store) CreateArtifactLink(ctx context.Context, input ArtifactLinkInsert) (*ArtifactLinkWrite, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	write, err := createArtifactLinkTx(ctx, tx, input, "")
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return write, nil
}

func (s *Store) ArtifactLink(ctx context.Context, linkID string) (*ArtifactLink, error) {
	return scanArtifactLink(s.db.QueryRowContext(ctx, artifactLinkSelect+` WHERE link_id=?`, linkID))
}

// ListArtifactLinks returns links visible to a project through either artifact
// endpoint or either version anchor. Links with no visible artifact are hidden.
const artifactLinkProjectVisibility = `EXISTS (
 SELECT 1 FROM artifacts a WHERE a.project_ident=? AND (
 (l.source_kind='artifact' AND l.source_id=a.artifact_id) OR
 (l.target_kind='artifact' AND l.target_id=a.artifact_id) OR
 l.source_version_id IN (SELECT artifact_version_id FROM artifact_versions WHERE artifact_id=a.artifact_id) OR
 l.target_version_id IN (SELECT artifact_version_id FROM artifact_versions WHERE artifact_id=a.artifact_id)))`

// CountArtifactLinks applies the same project visibility rule as link listing.
func (s *Store) CountArtifactLinks(ctx context.Context, projectIdent string) (int64, error) {
	return countArtifactLinks(ctx, s.db, projectIdent)
}

func countArtifactLinks(ctx context.Context, q queryer, projectIdent string) (int64, error) {
	var count int64
	err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM artifact_links AS l WHERE "+artifactLinkProjectVisibility,
		projectid.NormalizeIdent(projectIdent)).Scan(&count)
	return count, err
}

func (s *Store) ListArtifactLinks(ctx context.Context, projectIdent string, filter ArtifactLinkFilter) ([]ArtifactLink, error) {
	query := artifactLinkSelect + " AS l WHERE " + artifactLinkProjectVisibility
	args := []any{projectid.NormalizeIdent(projectIdent)}
	for _, item := range []struct{ column, value string }{
		{"link_type", filter.LinkType}, {"source_kind", filter.SourceKind}, {"source_id", filter.SourceID},
		{"target_kind", filter.TargetKind}, {"target_id", filter.TargetID},
	} {
		if value := strings.TrimSpace(item.value); value != "" {
			query += ` AND l.` + item.column + `=?`
			args = append(args, value)
		}
	}
	query += ` ORDER BY l.created_at DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ArtifactLink{}
	for rows.Next() {
		item, err := scanArtifactLink(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}
