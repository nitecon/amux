package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nitecon/amux/projects"
)

var ErrArtifactNotDesignReview = errors.New("artifact is not a design_review")

type DesignReviewContributionFilter struct {
	RoundID, Phase, Role, ReviewedVersionID, ReadSetContains string
}

type DesignReviewSynthesisInsert struct {
	ReviewedVersionID string
	ActorID           string
	WorkflowRunID     string
	IdempotencyKey    string
	ReadSet           json.RawMessage
	BodyFormat        string
	Body              string
	CreateVersion     bool
	VersionLabel      *string
}

type DesignReviewSynthesisWrite struct {
	Contribution *ArtifactContribution
	Version      *ArtifactVersion
	Artifact     *Artifact
	Warnings     []ArtifactQuotaWarning
	Replayed     bool
}

// CreateDesignReviewSynthesis commits the synthesis contribution, optional
// version, workflow outputs, and review-state transition together.
func (s *Store) CreateDesignReviewSynthesis(ctx context.Context, projectIdent, artifactID string, input DesignReviewSynthesisInsert) (*DesignReviewSynthesisWrite, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	projectIdent = projects.NormalizeIdent(projectIdent)
	artifact, err := scanArtifact(tx.QueryRowContext(ctx, artifactSelect+` WHERE project_ident=? AND artifact_id=?`, projectIdent, artifactID))
	if err != nil {
		return nil, err
	}
	if artifact == nil {
		return nil, ErrArtifactNotFound
	}
	if artifact.Kind != "design_review" {
		return nil, ErrArtifactNotDesignReview
	}
	runID := input.WorkflowRunID
	run, err := validateWorkflowRunForMutation(ctx, tx, artifactID, &runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("workflow_run not found")
	}
	if run.WorkflowKind != "design_review_round" {
		return nil, fmt.Errorf("workflow run is not a design_review_round")
	}
	if input.ReviewedVersionID == "" {
		if run.SourceArtifactVersionID == nil {
			return nil, fmt.Errorf("reviewed version required")
		}
		input.ReviewedVersionID = *run.SourceArtifactVersionID
	}
	phase := "synthesis"
	contribution, contributionReplay, err := addArtifactContributionTx(ctx, tx, projectIdent, artifactID, ArtifactContributionInsert{
		TargetKind: "artifact_version", TargetID: input.ReviewedVersionID, ContributionKind: "synthesis", Phase: &phase,
		Role: "analyst", ActorID: input.ActorID, WorkflowRunID: &runID, ReadSet: input.ReadSet,
		BodyFormat: input.BodyFormat, Body: input.Body, IdempotencyKey: &input.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	write := &DesignReviewSynthesisWrite{Contribution: contribution, Warnings: []ArtifactQuotaWarning{}}
	if input.CreateVersion {
		versionKey := input.IdempotencyKey + ":synthesis-version"
		payload, err := json.Marshal(map[string]any{
			"workflow": "design_review_synthesis", "round_id": run.RoundID,
			"source_artifact_version_id": input.ReviewedVersionID,
			"synthesis_contribution_id":  contribution.ContributionID, "read_set": input.ReadSet,
		})
		if err != nil {
			return nil, err
		}
		versionWrite, err := createArtifactVersionTx(ctx, tx, projectIdent, artifactID, ArtifactVersionInsert{
			VersionLabel: input.VersionLabel, ParentVersionID: &input.ReviewedVersionID, BodyFormat: input.BodyFormat,
			Body: &input.Body, StructuredPayload: payload, CreatedByActorID: input.ActorID,
			CreatedViaWorkflowRunID: &runID, VersionState: "draft", IdempotencyKey: &versionKey,
		}, nil, false)
		if err != nil {
			return nil, err
		}
		if versionWrite == nil || versionWrite.Record == nil {
			return nil, ErrArtifactNotFound
		}
		write.Version = versionWrite.Record
		write.Warnings = append(write.Warnings, versionWrite.Warnings...)
		write.Replayed = contributionReplay && versionWrite.Replayed
	}
	var versionID *string
	if write.Version != nil {
		versionID = &write.Version.ArtifactVersionID
	}
	if _, err := appendWorkflowRunOutputsTx(ctx, tx, runID, &contribution.ContributionID, versionID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE artifacts SET review_state='needs_user_decision',updated_at=? WHERE project_ident=? AND artifact_id=? AND kind='design_review'`, time.Now().UnixMilli(), projectIdent, artifactID); err != nil {
		return nil, err
	}
	write.Artifact, err = scanArtifact(tx.QueryRowContext(ctx, artifactSelect+` WHERE project_ident=? AND artifact_id=?`, projectIdent, artifactID))
	if err != nil {
		return nil, err
	}
	if write.Artifact == nil {
		return nil, ErrArtifactNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return write, nil
}

// AddDesignReviewContributionWithWorkflow commits the review contribution and
// its workflow output reference together. Generic contribution writes retain
// their existing workflow semantics.
func (s *Store) AddDesignReviewContributionWithWorkflow(ctx context.Context, projectIdent, artifactID, workflowRunID string, input ArtifactContributionInsert) (*ArtifactContribution, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	projectIdent = projects.NormalizeIdent(projectIdent)
	artifact, err := scanArtifact(tx.QueryRowContext(ctx, artifactSelect+` WHERE project_ident=? AND artifact_id=?`, projectIdent, artifactID))
	if err != nil {
		return nil, false, err
	}
	if artifact == nil {
		return nil, false, ErrArtifactNotFound
	}
	if artifact.Kind != "design_review" {
		return nil, false, ErrArtifactNotDesignReview
	}
	runID := workflowRunID
	run, err := validateWorkflowRunForMutation(ctx, tx, artifactID, &runID)
	if err != nil {
		return nil, false, err
	}
	if run == nil {
		return nil, false, fmt.Errorf("workflow_run not found")
	}
	if run.WorkflowKind != "design_review_round" {
		return nil, false, fmt.Errorf("workflow run is not a design_review_round")
	}
	input.WorkflowRunID = &runID
	contribution, replayed, err := addArtifactContributionTx(ctx, tx, projectIdent, artifactID, input)
	if err != nil {
		return nil, false, err
	}
	if _, err := appendWorkflowRunOutputsTx(ctx, tx, runID, &contribution.ContributionID, nil); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return contribution, replayed, nil
}

// ListDesignReviewContributions returns project-scoped review contributions
// in source order with the Rust route's optional filters.
func (s *Store) ListDesignReviewContributions(ctx context.Context, projectIdent, artifactID string, filter DesignReviewContributionFilter) ([]ArtifactContribution, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	artifact, err := scanArtifact(tx.QueryRowContext(ctx, artifactSelect+` WHERE project_ident=? AND artifact_id=?`, projects.NormalizeIdent(projectIdent), artifactID))
	if err != nil {
		return nil, err
	}
	if artifact == nil {
		return nil, ErrArtifactNotFound
	}
	if artifact.Kind != "design_review" {
		return nil, ErrArtifactNotDesignReview
	}
	query := `SELECT c.contribution_id,c.artifact_id,c.target_kind,c.target_id,c.contribution_kind,c.phase,c.role,c.actor_id,c.workflow_run_id,c.read_set,c.body_format,c.body,c.idempotency_key,c.created_at
FROM artifact_contributions c LEFT JOIN workflow_runs wr ON wr.workflow_run_id=c.workflow_run_id WHERE c.artifact_id=?`
	args := []any{artifactID}
	for _, item := range []struct{ column, value string }{
		{"wr.round_id", filter.RoundID}, {"c.phase", filter.Phase}, {"c.role", filter.Role},
	} {
		if value := strings.TrimSpace(item.value); value != "" {
			query += " AND " + item.column + "=?"
			args = append(args, value)
		}
	}
	if versionID := strings.TrimSpace(filter.ReviewedVersionID); versionID != "" {
		query += ` AND ((c.target_kind='artifact_version' AND c.target_id=?) OR wr.source_artifact_version_id=?)`
		args = append(args, versionID, versionID)
	}
	query += " ORDER BY c.created_at ASC"
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	items := []ArtifactContribution{}
	for rows.Next() {
		item, err := scanArtifactContribution(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if needle := strings.TrimSpace(filter.ReadSetContains); needle != "" {
		filtered := items[:0]
		for _, item := range items {
			if jsonRawContainsString(item.ReadSet, needle) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return items, nil
}

func jsonRawContainsString(raw json.RawMessage, needle string) bool {
	if len(raw) == 0 {
		return false
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return jsonValueContainsString(value, needle)
}

func jsonValueContainsString(value any, needle string) bool {
	switch typed := value.(type) {
	case string:
		return typed == needle
	case []any:
		for _, item := range typed {
			if jsonValueContainsString(item, needle) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if jsonValueContainsString(item, needle) {
				return true
			}
		}
	}
	return false
}
