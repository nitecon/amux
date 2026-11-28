package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	projectid "github.com/nitecon/amux/identity/project"
)

var ErrArtifactLinkProjectMismatch = errors.New("artifact link is outside the requested project")

// CreateArtifactLinkForProject validates project scope and any associated run
// in the same transaction as link replay/creation and resumed-run completion.
func (s *Store) CreateArtifactLinkForProject(ctx context.Context, projectIdent string, input ArtifactLinkInsert) (*ArtifactLinkWrite, error) {
	projectIdent = projectid.NormalizeIdent(projectIdent)
	if projectIdent == "" {
		return nil, fmt.Errorf("project ident is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	write, err := createArtifactLinkTx(ctx, tx, input, projectIdent)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return write, nil
}

// CreateSpecTaskArtifactLink reuses the unique source item link across runs.
// Rust serializes generation under its database lock and checks this tuple
// before inserting; this transaction preserves that behavior for concurrent Go
// requests with different workflow idempotency keys.
func (s *Store) CreateSpecTaskArtifactLink(ctx context.Context, projectIdent string, input ArtifactLinkInsert) (*ArtifactLinkWrite, error) {
	projectIdent = projectid.NormalizeIdent(projectIdent)
	if projectIdent == "" || input.LinkType != "task_generated_from_spec" || input.SourceKind != "artifact_version" || input.TargetKind != "task" || input.SourceChildAddress == nil {
		return nil, fmt.Errorf("invalid spec task artifact link")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := validateArtifactLinkProject(ctx, tx, input, projectIdent); err != nil {
		return nil, err
	}
	existing, err := scanArtifactLink(tx.QueryRowContext(ctx, artifactLinkSelect+` AS l
WHERE l.link_type='task_generated_from_spec' AND l.source_kind='artifact_version'
AND l.source_id=? AND l.source_child_address=? AND l.target_kind='task'
AND EXISTS (SELECT 1 FROM artifact_versions v JOIN artifacts a ON a.artifact_id=v.artifact_id
 WHERE v.artifact_version_id=l.source_id AND a.project_ident=?)
ORDER BY l.created_at ASC LIMIT 1`, input.SourceID, *input.SourceChildAddress, projectIdent))
	if err != nil {
		return nil, err
	}
	var write *ArtifactLinkWrite
	if existing != nil {
		write = &ArtifactLinkWrite{Record: existing, Replayed: true}
	} else {
		write, err = createArtifactLinkTx(ctx, tx, input, "")
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return write, nil
}

func createArtifactLinkTx(ctx context.Context, tx *sql.Tx, input ArtifactLinkInsert, requestedProject string) (*ArtifactLinkWrite, error) {
	if err := validateArtifactLink(ctx, tx, input); err != nil {
		return nil, err
	}
	var project string
	var err error
	if requestedProject != "" {
		if err := validateArtifactLinkProject(ctx, tx, input, requestedProject); err != nil {
			return nil, err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE ident=?`, requestedProject).Scan(&exists); err != nil {
			if err == sql.ErrNoRows {
				return nil, fmt.Errorf("project not found")
			}
			return nil, err
		}
		project = requestedProject
	}
	var run *WorkflowRun
	if requestedProject != "" && input.CreatedViaWorkflowRunID != nil {
		run, err = scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE workflow_run_id=?`, *input.CreatedViaWorkflowRunID))
		if err != nil {
			return nil, err
		}
		if run == nil {
			return nil, fmt.Errorf("workflow_run not found")
		}
		if err := ensureWorkflowRunRetryable(run); err != nil {
			return nil, err
		}
		if err := validateArtifactLinkEndpointProject(ctx, tx, "artifact", run.ArtifactID, requestedProject); err != nil {
			return nil, err
		}
	}
	if input.CreatedViaWorkflowRunID != nil && input.IdempotencyKey != nil {
		existing, err := scanArtifactLink(tx.QueryRowContext(ctx, artifactLinkSelect+` WHERE created_via_workflow_run_id=? AND idempotency_key=?`, *input.CreatedViaWorkflowRunID, *input.IdempotencyKey))
		if err != nil {
			return nil, err
		}
		if existing != nil {
			if requestedProject != "" {
				existingInput := ArtifactLinkInsert{SourceKind: existing.SourceKind, SourceID: existing.SourceID, SourceVersionID: existing.SourceVersionID, TargetKind: existing.TargetKind, TargetID: existing.TargetID, TargetVersionID: existing.TargetVersionID}
				if err := validateArtifactLinkProject(ctx, tx, existingInput, requestedProject); err != nil {
					return nil, err
				}
			}
			if run != nil {
				if err := validateAndCompleteResumedWorkflowRunTx(ctx, tx, run.ArtifactID, input.CreatedViaWorkflowRunID, nil, nil, &existing.LinkID); err != nil {
					return nil, err
				}
			}
			return &ArtifactLinkWrite{Record: existing, Replayed: true}, nil
		}
	}
	if requestedProject == "" {
		project, err = projectForArtifactLink(ctx, tx, input)
		if err != nil {
			return nil, err
		}
		if project == "" {
			return nil, fmt.Errorf("link project not resolvable")
		}
	}
	limits, err := artifactWriteLimits()
	if err != nil {
		return nil, err
	}
	warnings, err := checkArtifactQuota(ctx, tx, project, "link", limits)
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO artifact_links(link_id,link_type,source_kind,source_id,source_version_id,source_child_address,target_kind,target_id,target_version_id,target_child_address,created_by_actor_id,created_via_workflow_run_id,idempotency_key,supersedes_link_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id.String(), input.LinkType, input.SourceKind, input.SourceID, input.SourceVersionID, input.SourceChildAddress, input.TargetKind, input.TargetID, input.TargetVersionID, input.TargetChildAddress, input.CreatedByActorID, input.CreatedViaWorkflowRunID, input.IdempotencyKey, input.SupersedesLinkID, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	item, err := scanArtifactLink(tx.QueryRowContext(ctx, artifactLinkSelect+` WHERE link_id=?`, id.String()))
	if err != nil {
		return nil, err
	}
	if run != nil {
		if err := validateAndCompleteResumedWorkflowRunTx(ctx, tx, run.ArtifactID, input.CreatedViaWorkflowRunID, nil, nil, &item.LinkID); err != nil {
			return nil, err
		}
	}
	return &ArtifactLinkWrite{Record: item, Warnings: warnings}, nil
}

func validateArtifactLinkProject(ctx context.Context, q queryer, input ArtifactLinkInsert, requestedProject string) error {
	project, err := projectForArtifactLink(ctx, q, input)
	if err != nil {
		return err
	}
	if project == "" {
		return fmt.Errorf("link project not resolvable")
	}
	if project != requestedProject {
		return ErrArtifactLinkProjectMismatch
	}
	for _, endpoint := range []struct{ kind, id string }{{input.SourceKind, input.SourceID}, {input.TargetKind, input.TargetID}} {
		if err := validateArtifactLinkEndpointProject(ctx, q, endpoint.kind, endpoint.id, requestedProject); err != nil {
			return err
		}
	}
	for _, versionID := range []*string{input.SourceVersionID, input.TargetVersionID} {
		if versionID != nil {
			if err := validateArtifactLinkEndpointProject(ctx, q, "artifact_version", *versionID, requestedProject); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateArtifactLinkEndpointProject(ctx context.Context, q queryer, kind, id, project string) error {
	var query string
	switch kind {
	case "artifact":
		query = `SELECT project_ident FROM artifacts WHERE artifact_id=?`
	case "artifact_version":
		query = `SELECT a.project_ident FROM artifact_versions v JOIN artifacts a ON a.artifact_id=v.artifact_id WHERE v.artifact_version_id=?`
	case "contribution":
		query = `SELECT a.project_ident FROM artifact_contributions c JOIN artifacts a ON a.artifact_id=c.artifact_id WHERE c.contribution_id=?`
	case "comment":
		query = `SELECT a.project_ident FROM artifact_comments c JOIN artifacts a ON a.artifact_id=c.artifact_id WHERE c.comment_id=?`
	case "chunk":
		query = `SELECT a.project_ident FROM artifact_chunks c JOIN artifacts a ON a.artifact_id=c.artifact_id WHERE c.chunk_id=?`
	default:
		return nil
	}
	var actual string
	err := q.QueryRowContext(ctx, query, id).Scan(&actual)
	if err == sql.ErrNoRows {
		return ErrArtifactLinkProjectMismatch
	}
	if err != nil {
		return err
	}
	if actual != project {
		return ErrArtifactLinkProjectMismatch
	}
	return nil
}
