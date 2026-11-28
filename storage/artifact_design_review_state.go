package storage

import (
	"context"
	"fmt"
	"time"

	projectid "github.com/nitecon/amux/identity/project"
)

type DesignReviewStateWrite struct {
	Artifact     *Artifact
	Contribution *ArtifactContribution
	Replayed     bool
}

// UpdateDesignReviewState records the transition and updates the artifact in
// one transaction, including an optional workflow output reference.
func (s *Store) UpdateDesignReviewState(ctx context.Context, projectIdent, artifactID string, lifecycleState, reviewState *string, actorID, note string, workflowRunID *string, idempotencyKey string) (*DesignReviewStateWrite, error) {
	if lifecycleState == nil && reviewState == nil {
		return nil, fmt.Errorf("design review state required")
	}
	if lifecycleState != nil && !validArtifactLifecycleState(*lifecycleState) {
		return nil, fmt.Errorf("invalid artifact lifecycle state")
	}
	if reviewState != nil && !validArtifactReviewState(*reviewState) {
		return nil, fmt.Errorf("invalid artifact review state")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	projectIdent = projectid.NormalizeIdent(projectIdent)
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
	if _, err := validateWorkflowRunForMutation(ctx, tx, artifactID, workflowRunID); err != nil {
		return nil, err
	}
	phase := "state_transition"
	contribution, replayed, err := addArtifactContributionTx(ctx, tx, projectIdent, artifactID, ArtifactContributionInsert{
		TargetKind: "artifact", TargetID: artifactID, ContributionKind: "state_transition", Phase: &phase,
		Role: "coordinator", ActorID: actorID, WorkflowRunID: workflowRunID,
		BodyFormat: "markdown", Body: note, IdempotencyKey: &idempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	if workflowRunID != nil {
		if _, err := appendWorkflowRunOutputsTx(ctx, tx, *workflowRunID, &contribution.ContributionID, nil); err != nil {
			return nil, err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE artifacts
		SET lifecycle_state=COALESCE(?,lifecycle_state),review_state=COALESCE(?,review_state),updated_at=?
		WHERE project_ident=? AND artifact_id=? AND kind='design_review'`, lifecycleState, reviewState, time.Now().UnixMilli(), projectIdent, artifactID)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed == 0 {
		return nil, ErrArtifactNotFound
	}
	updated, err := scanArtifact(tx.QueryRowContext(ctx, artifactSelect+` WHERE project_ident=? AND artifact_id=?`, projectIdent, artifactID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &DesignReviewStateWrite{Artifact: updated, Contribution: contribution, Replayed: replayed}, nil
}

func validArtifactLifecycleState(state string) bool {
	switch state {
	case "draft", "active", "superseded", "archived":
		return true
	default:
		return false
	}
}

func validArtifactReviewState(state string) bool {
	switch state {
	case "none", "collecting_reviews", "synthesizing", "needs_user_decision", "accepted", "rejected":
		return true
	default:
		return false
	}
}
