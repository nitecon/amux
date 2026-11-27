package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type ReviewFinding struct {
	Title   string `json:"title"`
	Details string `json:"details"`
}

// currentSubmissionReviewSource resolves the submission cycle shared by verdicts
// and ordinary closure. Historical reviews must not require an unusable verdict.
func currentSubmissionReviewSource(ctx context.Context, tx *sql.Tx, project, reviewID string) (*Task, error) {
	var sourceID string
	err := tx.QueryRowContext(ctx, `SELECT r.source_task_id FROM task_reviews r
		WHERE r.review_task_id=? AND r.stage='submission' AND r.source_done_at=(
			SELECT MAX(current.source_done_at) FROM task_reviews current
			WHERE current.source_task_id=r.source_task_id AND current.stage='submission')`, reviewID).Scan(&sourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return readTask(ctx, tx, project, sourceID)
}

// FinishSubmissionReview applies the review and its effect on the source task
// together. The owned review conversation supplies the execution fence.
func (s *Store) FinishSubmissionReview(ctx context.Context, project, id, outcome, summary string, findings []ReviewFinding, actor *AgentOrigin) (*Task, error) {
	summary = strings.TrimSpace(summary)
	if (outcome != "approved" && outcome != "changes_requested") || summary == "" || len(summary) > 8000 || len(findings) > 20 {
		return nil, fmt.Errorf("%w: expected an approval or correction request with a review summary and at most 20 findings", ErrTaskConflict)
	}
	for _, finding := range findings {
		if strings.TrimSpace(finding.Title) == "" || len(finding.Title) > 255 ||
			strings.TrimSpace(finding.Details) == "" || len(finding.Details) > 8000 {
			return nil, fmt.Errorf("%w: findings need a title and concrete details", ErrTaskConflict)
		}
	}
	var result *Task
	err := s.taskMutation(ctx, actor, func(tx *sql.Tx, actor *AgentOrigin) error {
		review, err := readTask(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if review == nil {
			return ErrTaskNotFound
		}
		if actor == nil || (review.Status != "in_progress" && review.Status != "stalled") || review.OwnerOrigin == nil || *review.OwnerOrigin != *actor {
			return ErrTaskOwnership
		}
		source, err := currentSubmissionReviewSource(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if source == nil {
			return fmt.Errorf("%w: review is not the current software submission review", ErrTaskConflict)
		}
		if source.Status != "user_review" || source.CompletionOwner == nil {
			return fmt.Errorf("%w: source is no longer awaiting code review", ErrTaskConflict)
		}
		for _, taskID := range []string{source.ID, review.ID} {
			if _, err := insertTaskComment(ctx, tx, taskID, actor.SessionID, "agent", summary, nil, actor); err != nil {
				return err
			}
		}
		for _, finding := range findings {
			if _, err := insertTaskRecord(ctx, tx, TaskInsert{
				Project: project, ParentID: &source.ID, Goal: source.Goal, Title: finding.Title,
				Details: &finding.Details, Reporter: actor.SessionID,
			}, nil, "general"); err != nil {
				return err
			}
		}
		result, err = applyTaskWorkflow(ctx, tx, review, TaskWorkflowUpdate{
			Status: "done", Changeset: json.RawMessage(`null`), Policy: TaskTransitionPolicy{submissionReviewVerdict: true},
		}, actor, s.routerReviews.Load())
		if err != nil {
			return err
		}
		switch outcome {
		case "approved":
			_, err = applyTaskWorkflow(ctx, tx, source, TaskWorkflowUpdate{Status: "done"}, nil, s.routerReviews.Load())
			return err
		case "changes_requested":
			if _, err := tx.ExecContext(ctx, `UPDATE task_reviews SET stage='changes_requested'
				WHERE review_task_id=? AND stage='submission'`, review.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO amux_review_rework(source_task_id,session_id) VALUES(?,?)
				ON CONFLICT(source_task_id) DO UPDATE SET session_id=excluded.session_id`, source.ID, source.CompletionOwner.SessionID); err != nil {
				return err
			}
			_, err = applyTaskWorkflow(ctx, tx, source, TaskWorkflowUpdate{
				Status: "todo", Policy: TaskTransitionPolicy{HumanRequest: true},
			}, nil, s.routerReviews.Load())
			return err
		}
		return nil
	})
	return result, err
}
