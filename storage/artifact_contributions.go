package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	projectid "github.com/nitecon/amux/identity/project"
)

// ArtifactContribution is an immutable artifact discourse record.
type ArtifactContribution struct {
	ContributionID   string          `json:"contribution_id"`
	ArtifactID       string          `json:"artifact_id"`
	TargetKind       string          `json:"target_kind"`
	TargetID         string          `json:"target_id"`
	ContributionKind string          `json:"contribution_kind"`
	Phase            *string         `json:"phase"`
	Role             string          `json:"role"`
	ActorID          string          `json:"actor_id"`
	WorkflowRunID    *string         `json:"workflow_run_id"`
	ReadSet          json.RawMessage `json:"read_set"`
	BodyFormat       string          `json:"body_format"`
	Body             string          `json:"body"`
	IdempotencyKey   *string         `json:"idempotency_key"`
	CreatedAt        int64           `json:"created_at"`
}

type ArtifactContributionInsert struct {
	TargetKind       string
	TargetID         string
	ContributionKind string
	Phase            *string
	Role             string
	ActorID          string
	WorkflowRunID    *string
	ReadSet          json.RawMessage
	BodyFormat       string
	Body             string
	IdempotencyKey   *string
}

const artifactContributionSelect = `SELECT contribution_id,artifact_id,target_kind,target_id,contribution_kind,phase,role,actor_id,workflow_run_id,read_set,body_format,body,idempotency_key,created_at FROM artifact_contributions`

func scanArtifactContribution(row interface{ Scan(...any) error }) (*ArtifactContribution, error) {
	var item ArtifactContribution
	var readSet sql.NullString
	if err := row.Scan(&item.ContributionID, &item.ArtifactID, &item.TargetKind, &item.TargetID, &item.ContributionKind, &item.Phase, &item.Role, &item.ActorID, &item.WorkflowRunID, &readSet, &item.BodyFormat, &item.Body, &item.IdempotencyKey, &item.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if readSet.Valid {
		if !json.Valid([]byte(readSet.String)) {
			return nil, fmt.Errorf("decode artifact contribution read_set: invalid JSON")
		}
		item.ReadSet = json.RawMessage(readSet.String)
	}
	return &item, nil
}

func (s *Store) AddArtifactContribution(ctx context.Context, projectIdent, artifactID string, input ArtifactContributionInsert) (*ArtifactContribution, bool, error) {
	return s.addArtifactContributionWithLimits(ctx, projectIdent, artifactID, input, nil)
}

// AddArtifactContributionWithWorkflow validates and updates a linked workflow
// run in the same transaction as the contribution write.
func (s *Store) AddArtifactContributionWithWorkflow(ctx context.Context, projectIdent, artifactID string, input ArtifactContributionInsert) (*ArtifactContribution, bool, error) {
	return s.addArtifactContributionWithOptions(ctx, projectIdent, artifactID, input, nil, true)
}

func (s *Store) addArtifactContributionWithLimits(ctx context.Context, projectIdent, artifactID string, input ArtifactContributionInsert, suppliedLimits *ArtifactWriteLimits) (*ArtifactContribution, bool, error) {
	return s.addArtifactContributionWithOptions(ctx, projectIdent, artifactID, input, suppliedLimits, false)
}

func (s *Store) addArtifactContributionWithOptions(ctx context.Context, projectIdent, artifactID string, input ArtifactContributionInsert, suppliedLimits *ArtifactWriteLimits, workflowAtomic bool) (*ArtifactContribution, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	item, replayed, err := addArtifactContributionTxWithLimits(ctx, tx, projectIdent, artifactID, input, suppliedLimits)
	if err != nil {
		return nil, false, err
	}
	if workflowAtomic {
		if err := validateAndCompleteResumedWorkflowRunTx(ctx, tx, artifactID, input.WorkflowRunID, &item.ContributionID, nil, nil); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return item, replayed, nil
}

// addArtifactContributionTx is shared by ordinary contribution writes and
// state transitions that must commit their audit record atomically.
func addArtifactContributionTx(ctx context.Context, tx *sql.Tx, projectIdent, artifactID string, input ArtifactContributionInsert) (*ArtifactContribution, bool, error) {
	return addArtifactContributionTxWithLimits(ctx, tx, projectIdent, artifactID, input, nil)
}

func addArtifactContributionTxWithLimits(ctx context.Context, tx *sql.Tx, projectIdent, artifactID string, input ArtifactContributionInsert, suppliedLimits *ArtifactWriteLimits) (*ArtifactContribution, bool, error) {
	projectIdent = projectid.NormalizeIdent(projectIdent)
	if len(input.ReadSet) > 0 && !json.Valid(input.ReadSet) {
		return nil, false, fmt.Errorf("artifact contribution read_set must be valid JSON")
	}
	if err := requireArtifactProjectTx(ctx, tx, projectIdent, artifactID); err != nil {
		return nil, false, err
	}
	existing, err := findArtifactContributionReplayTx(ctx, tx, artifactID, input)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, true, nil
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
	if err := checkArtifactTextSize("contribution_body", input.Body, limits.ContributionBodyMaxBytes); err != nil {
		return nil, false, err
	}
	if err := checkArtifactReadSetSize(input.ReadSet, limits.ReadSetMaxRefs); err != nil {
		return nil, false, err
	}
	if err := validateArtifactTarget(ctx, tx, artifactID, input.TargetKind, input.TargetID, ""); err != nil {
		return nil, false, err
	}
	if err := validateContributionReadSet(ctx, tx, input.ReadSet); err != nil {
		return nil, false, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, false, err
	}
	if input.BodyFormat == "" {
		input.BodyFormat = "markdown"
	}
	now := time.Now().UnixMilli()
	_, err = tx.ExecContext(ctx, `INSERT INTO artifact_contributions(contribution_id,artifact_id,target_kind,target_id,contribution_kind,phase,role,actor_id,workflow_run_id,read_set,body_format,body,idempotency_key,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id.String(), artifactID, input.TargetKind, input.TargetID, input.ContributionKind, input.Phase, input.Role, input.ActorID, input.WorkflowRunID, nullJSON(input.ReadSet), input.BodyFormat, input.Body, input.IdempotencyKey, now)
	if err != nil {
		return nil, false, err
	}
	item, err := scanArtifactContribution(tx.QueryRowContext(ctx, artifactContributionSelect+` WHERE contribution_id=?`, id.String()))
	if err != nil {
		return nil, false, err
	}
	return item, false, nil
}

func findArtifactContributionReplayTx(ctx context.Context, tx *sql.Tx, artifactID string, input ArtifactContributionInsert) (*ArtifactContribution, error) {
	if input.IdempotencyKey == nil {
		return nil, nil
	}
	if input.WorkflowRunID != nil {
		return scanArtifactContribution(tx.QueryRowContext(ctx, artifactContributionSelect+` WHERE workflow_run_id=? AND idempotency_key=?`, *input.WorkflowRunID, *input.IdempotencyKey))
	}
	return scanArtifactContribution(tx.QueryRowContext(ctx, artifactContributionSelect+` WHERE artifact_id=? AND actor_id=? AND idempotency_key=? AND workflow_run_id IS NULL`, artifactID, input.ActorID, *input.IdempotencyKey))
}

func (s *Store) ArtifactContribution(ctx context.Context, projectIdent, artifactID, contributionID string) (*ArtifactContribution, error) {
	return scanArtifactContribution(s.db.QueryRowContext(ctx, artifactContributionSelect+` WHERE artifact_id=? AND contribution_id=? AND EXISTS(SELECT 1 FROM artifacts WHERE artifact_id=? AND project_ident=?)`, artifactID, contributionID, artifactID, projectid.NormalizeIdent(projectIdent)))
}

func (s *Store) ArtifactContributions(ctx context.Context, projectIdent, artifactID string) ([]ArtifactContribution, error) {
	if err := requireArtifactProject(ctx, s.db, projectid.NormalizeIdent(projectIdent), artifactID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, artifactContributionSelect+` WHERE artifact_id=? ORDER BY created_at ASC`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ArtifactContribution{}
	for rows.Next() {
		item, err := scanArtifactContribution(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

func validateContributionReadSet(ctx context.Context, tx *sql.Tx, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var readSet map[string]json.RawMessage
	if err := json.Unmarshal(raw, &readSet); err != nil || readSet == nil {
		return nil
	}
	tables := map[string]struct{ table, column string }{
		"artifact_version": {"artifact_versions", "artifact_version_id"}, "versions": {"artifact_versions", "artifact_version_id"},
		"contribution": {"artifact_contributions", "contribution_id"}, "contributions": {"artifact_contributions", "contribution_id"},
		"chunk": {"artifact_chunks", "chunk_id"}, "chunks": {"artifact_chunks", "chunk_id"},
		"comment": {"artifact_comments", "comment_id"}, "comments": {"artifact_comments", "comment_id"},
		"workflow_run": {"workflow_runs", "workflow_run_id"}, "workflow_runs": {"workflow_runs", "workflow_run_id"},
	}
	for kind, rawIDs := range readSet {
		table, ok := tables[kind]
		if !ok {
			continue
		}
		var ids []json.RawMessage
		if err := json.Unmarshal(rawIDs, &ids); err != nil {
			continue
		}
		for _, rawID := range ids {
			var id string
			if err := json.Unmarshal(rawID, &id); err != nil {
				continue
			}
			var found int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM `+table.table+` WHERE `+table.column+`=?`, id).Scan(&found)
			if err == sql.ErrNoRows {
				return fmt.Errorf("read_set %s reference %q does not resolve", kind, id)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}
