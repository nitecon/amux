package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/projects"
)

// artifactDiscourseMigration follows the workflow-run schema at migration 39.
// The coordinator owns registration in store.go.
const artifactDiscourseMigration = 40

var ErrArtifactNotFound = errors.New("artifact not found")

func migrateArtifactDiscourse(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS artifact_contributions (
 contribution_id TEXT PRIMARY KEY,
 artifact_id TEXT NOT NULL REFERENCES artifacts(artifact_id) ON DELETE CASCADE,
 target_kind TEXT NOT NULL CHECK(target_kind IN ('artifact','artifact_version','contribution')),
 target_id TEXT NOT NULL,
 contribution_kind TEXT NOT NULL CHECK(contribution_kind IN ('review','synthesis','decision','note','completion','state_transition')),
 phase TEXT,
 role TEXT NOT NULL CHECK(role IN ('author','reviewer','analyst','implementer','coordinator','user')),
 actor_id TEXT NOT NULL REFERENCES artifact_actors(actor_id),
 workflow_run_id TEXT REFERENCES workflow_runs(workflow_run_id),
 read_set TEXT,
 body_format TEXT NOT NULL CHECK(body_format='markdown'),
 body TEXT NOT NULL,
 idempotency_key TEXT,
 created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_contributions_artifact ON artifact_contributions(artifact_id,created_at DESC);
CREATE INDEX IF NOT EXISTS idx_contributions_target ON artifact_contributions(target_kind,target_id);
CREATE INDEX IF NOT EXISTS idx_contributions_run ON artifact_contributions(workflow_run_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_contributions_idempotency_run
 ON artifact_contributions(workflow_run_id,idempotency_key)
 WHERE workflow_run_id IS NOT NULL AND idempotency_key IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_contributions_idempotency_runless
 ON artifact_contributions(artifact_id,actor_id,idempotency_key)
 WHERE workflow_run_id IS NULL AND idempotency_key IS NOT NULL;
CREATE TRIGGER IF NOT EXISTS prevent_artifact_contribution_update
 BEFORE UPDATE ON artifact_contributions FOR EACH ROW
 BEGIN SELECT RAISE(ABORT,'artifact_contribution rows are immutable'); END;
CREATE TABLE IF NOT EXISTS artifact_comments (
 comment_id TEXT PRIMARY KEY,
 artifact_id TEXT NOT NULL REFERENCES artifacts(artifact_id) ON DELETE CASCADE,
 target_kind TEXT NOT NULL CHECK(target_kind IN ('artifact','artifact_version','contribution')),
 target_id TEXT NOT NULL,
 child_address TEXT,
 parent_comment_id TEXT REFERENCES artifact_comments(comment_id),
 actor_id TEXT NOT NULL REFERENCES artifact_actors(actor_id),
 body TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'open' CHECK(state IN ('open','resolved')),
 resolved_by_actor_id TEXT REFERENCES artifact_actors(actor_id),
 resolved_by_workflow_run_id TEXT REFERENCES workflow_runs(workflow_run_id),
 resolved_at INTEGER,
 resolution_note TEXT,
 idempotency_key TEXT,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL,
 CHECK(child_address IS NULL OR target_kind='artifact_version')
);
CREATE INDEX IF NOT EXISTS idx_artifact_comments_target ON artifact_comments(target_kind,target_id);
CREATE INDEX IF NOT EXISTS idx_artifact_comments_artifact_state ON artifact_comments(artifact_id,state,created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifact_comments_idempotency
 ON artifact_comments(target_kind,target_id,actor_id,idempotency_key)
 WHERE idempotency_key IS NOT NULL;`)
	if err != nil {
		return fmt.Errorf("migrate artifact comments and contributions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)`, artifactDiscourseMigration); err != nil {
		return fmt.Errorf("record artifact discourse migration: %w", err)
	}
	return nil
}

// ArtifactComment is a discussion entry anchored to an artifact, version, or
// contribution. ChildAddress is stored opaquely and is valid only for versions.
type ArtifactComment struct {
	CommentID             string  `json:"comment_id"`
	ArtifactID            string  `json:"artifact_id"`
	TargetKind            string  `json:"target_kind"`
	TargetID              string  `json:"target_id"`
	ChildAddress          *string `json:"child_address"`
	ParentCommentID       *string `json:"parent_comment_id"`
	ActorID               string  `json:"actor_id"`
	Body                  string  `json:"body"`
	State                 string  `json:"state"`
	ResolvedByActorID     *string `json:"resolved_by_actor_id"`
	ResolvedByWorkflowRun *string `json:"resolved_by_workflow_run_id"`
	ResolvedAt            *int64  `json:"resolved_at"`
	ResolutionNote        *string `json:"resolution_note"`
	IdempotencyKey        *string `json:"idempotency_key"`
	CreatedAt             int64   `json:"created_at"`
	UpdatedAt             int64   `json:"updated_at"`
}

type ArtifactCommentInsert struct {
	TargetKind      string
	TargetID        string
	ChildAddress    *string
	ParentCommentID *string
	ActorID         string
	Body            string
	IdempotencyKey  *string
}

// ArtifactCommentResolve contains the source resolver attribution fields.
type ArtifactCommentResolve struct {
	ActorID       string
	WorkflowRunID *string
	Note          *string
}

const artifactCommentSelect = `SELECT comment_id,artifact_id,target_kind,target_id,child_address,parent_comment_id,actor_id,body,state,resolved_by_actor_id,resolved_by_workflow_run_id,resolved_at,resolution_note,idempotency_key,created_at,updated_at FROM artifact_comments`

func scanArtifactComment(row interface{ Scan(...any) error }) (*ArtifactComment, error) {
	var item ArtifactComment
	if err := row.Scan(&item.CommentID, &item.ArtifactID, &item.TargetKind, &item.TargetID, &item.ChildAddress, &item.ParentCommentID, &item.ActorID, &item.Body, &item.State, &item.ResolvedByActorID, &item.ResolvedByWorkflowRun, &item.ResolvedAt, &item.ResolutionNote, &item.IdempotencyKey, &item.CreatedAt, &item.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

func (s *Store) AddArtifactComment(ctx context.Context, projectIdent, artifactID string, input ArtifactCommentInsert) (*ArtifactComment, bool, error) {
	return s.addArtifactCommentWithLimits(ctx, projectIdent, artifactID, input, nil)
}

func (s *Store) addArtifactCommentWithLimits(ctx context.Context, projectIdent, artifactID string, input ArtifactCommentInsert, suppliedLimits *ArtifactWriteLimits) (*ArtifactComment, bool, error) {
	projectIdent = projects.NormalizeIdent(projectIdent)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err := requireArtifactProjectTx(ctx, tx, projectIdent, artifactID); err != nil {
		return nil, false, err
	}
	if input.ParentCommentID != nil {
		if err := validateArtifactTarget(ctx, tx, artifactID, "contribution", *input.ParentCommentID, "comment"); err != nil {
			return nil, false, fmt.Errorf("parent comment: %w", err)
		}
	}
	if input.IdempotencyKey != nil {
		item, err := scanArtifactComment(tx.QueryRowContext(ctx, artifactCommentSelect+` WHERE target_kind=? AND target_id=? AND actor_id=? AND idempotency_key=?`, input.TargetKind, input.TargetID, input.ActorID, *input.IdempotencyKey))
		if err != nil {
			return nil, false, err
		}
		if item != nil {
			if err := tx.Commit(); err != nil {
				return nil, false, err
			}
			return item, true, nil
		}
	}
	limits := ArtifactWriteLimits{}
	if suppliedLimits != nil {
		limits = *suppliedLimits
	} else {
		limits, err = artifactWriteLimits()
		if err != nil {
			return nil, false, err
		}
	}
	if err := checkArtifactTextSize("comment_body", input.Body, limits.CommentBodyMaxBytes); err != nil {
		return nil, false, err
	}
	if err := validateArtifactTarget(ctx, tx, artifactID, input.TargetKind, input.TargetID, ""); err != nil {
		return nil, false, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UnixMilli()
	_, err = tx.ExecContext(ctx, `INSERT INTO artifact_comments(comment_id,artifact_id,target_kind,target_id,child_address,parent_comment_id,actor_id,body,state,resolved_by_actor_id,resolved_by_workflow_run_id,resolved_at,resolution_note,idempotency_key,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,'open',NULL,NULL,NULL,NULL,?,?,?)`, id.String(), artifactID, input.TargetKind, input.TargetID, input.ChildAddress, input.ParentCommentID, input.ActorID, input.Body, input.IdempotencyKey, now, now)
	if err != nil {
		return nil, false, err
	}
	item, err := scanArtifactComment(tx.QueryRowContext(ctx, artifactCommentSelect+` WHERE comment_id=?`, id.String()))
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return item, false, nil
}

func (s *Store) ArtifactComment(ctx context.Context, projectIdent, artifactID, commentID string) (*ArtifactComment, error) {
	return scanArtifactComment(s.db.QueryRowContext(ctx, artifactCommentSelect+` WHERE artifact_id=? AND comment_id=? AND EXISTS(SELECT 1 FROM artifacts WHERE artifact_id=? AND project_ident=?)`, artifactID, commentID, artifactID, projects.NormalizeIdent(projectIdent)))
}

func (s *Store) ArtifactComments(ctx context.Context, projectIdent, artifactID string) ([]ArtifactComment, error) {
	if err := requireArtifactProject(ctx, s.db, projects.NormalizeIdent(projectIdent), artifactID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, artifactCommentSelect+` WHERE artifact_id=? ORDER BY created_at ASC`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ArtifactComment{}
	for rows.Next() {
		item, err := scanArtifactComment(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

func (s *Store) ResolveArtifactComment(ctx context.Context, projectIdent, artifactID, commentID string, input ArtifactCommentResolve) (*ArtifactComment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requireArtifactProjectTx(ctx, tx, projects.NormalizeIdent(projectIdent), artifactID); err != nil {
		return nil, err
	}
	item, err := scanArtifactComment(tx.QueryRowContext(ctx, artifactCommentSelect+` WHERE artifact_id=? AND comment_id=?`, artifactID, commentID))
	if err != nil || item == nil {
		return item, err
	}
	if item.State == "resolved" {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return item, nil
	}
	now := time.Now().UnixMilli()
	_, err = tx.ExecContext(ctx, `UPDATE artifact_comments SET state='resolved',resolved_by_actor_id=?,resolved_by_workflow_run_id=?,resolved_at=?,resolution_note=?,updated_at=? WHERE artifact_id=? AND comment_id=? AND state='open'`, input.ActorID, input.WorkflowRunID, now, input.Note, now, artifactID, commentID)
	if err != nil {
		return nil, err
	}
	item, err = scanArtifactComment(tx.QueryRowContext(ctx, artifactCommentSelect+` WHERE artifact_id=? AND comment_id=?`, artifactID, commentID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return item, nil
}

func (s *Store) ReopenArtifactComment(ctx context.Context, projectIdent, artifactID, commentID string, actorID, noteBody string, idempotencyKey *string) (*ArtifactComment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requireArtifactProjectTx(ctx, tx, projects.NormalizeIdent(projectIdent), artifactID); err != nil {
		return nil, err
	}
	item, err := scanArtifactComment(tx.QueryRowContext(ctx, artifactCommentSelect+` WHERE artifact_id=? AND comment_id=?`, artifactID, commentID))
	if err != nil || item == nil {
		return item, err
	}
	if item.State != "resolved" {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return item, nil
	}
	if err := validateArtifactTarget(ctx, tx, artifactID, item.TargetKind, item.TargetID, ""); err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE artifact_comments SET state='open',resolved_by_actor_id=NULL,resolved_by_workflow_run_id=NULL,resolved_at=NULL,resolution_note=NULL,updated_at=? WHERE artifact_id=? AND comment_id=?`, now, artifactID, commentID); err != nil {
		return nil, err
	}
	var existingReply *ArtifactComment
	if idempotencyKey != nil {
		existingReply, err = scanArtifactComment(tx.QueryRowContext(ctx, artifactCommentSelect+` WHERE target_kind=? AND target_id=? AND actor_id=? AND idempotency_key=?`, item.TargetKind, item.TargetID, actorID, *idempotencyKey))
		if err != nil {
			return nil, err
		}
	}
	if existingReply == nil {
		replyID, err := uuid.NewV7()
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO artifact_comments(comment_id,artifact_id,target_kind,target_id,child_address,parent_comment_id,actor_id,body,state,resolved_by_actor_id,resolved_by_workflow_run_id,resolved_at,resolution_note,idempotency_key,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,'open',NULL,NULL,NULL,NULL,?,?,?)`, replyID.String(), artifactID, item.TargetKind, item.TargetID, item.ChildAddress, commentID, actorID, noteBody, idempotencyKey, now, now)
		if err != nil {
			return nil, err
		}
	}
	item, err = scanArtifactComment(tx.QueryRowContext(ctx, artifactCommentSelect+` WHERE artifact_id=? AND comment_id=?`, artifactID, commentID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return item, nil
}

func validateArtifactTarget(ctx context.Context, tx *sql.Tx, artifactID, kind, targetID, override string) error {
	if override != "" {
		kind = override
	}
	var table, idColumn string
	switch kind {
	case "artifact":
		table, idColumn = "artifacts", "artifact_id"
	case "artifact_version":
		table, idColumn = "artifact_versions", "artifact_version_id"
	case "contribution":
		table, idColumn = "artifact_contributions", "contribution_id"
	case "comment":
		table, idColumn = "artifact_comments", "comment_id"
	default:
		return fmt.Errorf("unsupported target_kind %q", kind)
	}
	var owner string
	err := tx.QueryRowContext(ctx, `SELECT artifact_id FROM `+table+` WHERE `+idColumn+`=?`, targetID).Scan(&owner)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%s target %q not found", kind, targetID)
	}
	if err != nil {
		return err
	}
	if owner != artifactID {
		return fmt.Errorf("%s target %q belongs to artifact %q, not %q", kind, targetID, owner, artifactID)
	}
	return nil
}

func requireArtifactProject(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, projectIdent, artifactID string) error {
	var exists int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM artifacts WHERE project_ident=? AND artifact_id=?`, projectIdent, artifactID).Scan(&exists)
	if err == sql.ErrNoRows {
		return ErrArtifactNotFound
	}
	return err
}

func requireArtifactProjectTx(ctx context.Context, tx *sql.Tx, projectIdent, artifactID string) error {
	return requireArtifactProject(ctx, tx, projectIdent, artifactID)
}
