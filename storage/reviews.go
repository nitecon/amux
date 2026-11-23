package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

func oppositeReviewProvider(provider string) *string {
	switch provider {
	case "codex":
		return ptr("claude")
	case "claude":
		return ptr("codex")
	default:
		return nil
	}
}

func enqueueTaskReviews(ctx context.Context, tx *sql.Tx, source *Task) ([]Task, error) {
	if source.Status != "done" || source.DoneAt == nil {
		return nil, fmt.Errorf("%w: source task is not completed", ErrTaskConflict)
	}
	return enqueueReviewStages(ctx, tx, source, *source.DoneAt, []string{"code", "documentation"})
}

// Move the submitted review to the completion cycle so later completion
// delivery can reuse the same child.
func finalizeSubmissionReview(ctx context.Context, tx *sql.Tx, sourceID string, doneAt int64) error {
	var childID, status string
	err := tx.QueryRowContext(ctx, `SELECT r.review_task_id,t.status FROM task_reviews r
		JOIN tasks t ON t.id=r.review_task_id
		WHERE r.source_task_id=? AND r.stage='submission'
		ORDER BY r.source_done_at DESC LIMIT 1`, sourceID).Scan(&childID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status != "done" {
		return fmt.Errorf("%w: complete the code review subtask before closing the submitted work", ErrTaskConflict)
	}
	_, err = tx.ExecContext(ctx, `UPDATE task_reviews SET stage='code',source_done_at=?
		WHERE source_task_id=? AND review_task_id=? AND stage='submission'`, doneAt, sourceID, childID)
	return err
}

func enqueueReviewStages(ctx context.Context, tx *sql.Tx, source *Task, cycle int64, stages []string) ([]Task, error) {
	result := []Task{}
	if source.TaskType != "software" {
		return result, nil
	}
	var followup bool
	if err := tx.QueryRowContext(ctx, `WITH RECURSIVE ancestors(id) AS(SELECT ?1 UNION SELECT s.parent_id FROM task_subtasks s JOIN ancestors a ON s.child_id=a.id)
 SELECT EXISTS(SELECT 1 FROM ancestors a JOIN tasks t ON t.id=a.id WHERE EXISTS(SELECT 1 FROM json_each(CASE WHEN json_valid(t.labels) THEN t.labels ELSE '[]' END) l WHERE l.value IN('task-review','documentation-review','documentation')) OR EXISTS(SELECT 1 FROM task_reviews r WHERE r.review_task_id=a.id))`, source.ID).Scan(&followup); err != nil {
		return nil, err
	}
	if followup || (source.Kind == "delegated" && source.DelegatedToTaskID != nil) {
		return result, nil
	}
	var preferred *string
	implementer := source.OwnerOrigin
	if source.Status == "user_review" {
		implementer = source.CompletionOwner
	}
	if implementer != nil {
		preferred = oppositeReviewProvider(implementer.Provider)
	} else {
		var provider *string
		err := tx.QueryRowContext(ctx, "SELECT json_extract(origin_json,'$.provider') FROM task_comments WHERE task_id=? AND author_type='agent' AND json_extract(origin_json,'$.provider') IS NOT NULL ORDER BY created_at DESC,id DESC LIMIT 1", source.ID).Scan(&provider)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if provider != nil {
			preferred = oppositeReviewProvider(*provider)
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT json_extract(origin_json,'$.session_id') FROM task_comments WHERE task_id=? AND author_type='agent' AND json_extract(origin_json,'$.session_id') IS NOT NULL ORDER BY created_at", source.ID)
	if err != nil {
		return nil, err
	}
	contributors := []string{}
	for rows.Next() {
		var session string
		if err := rows.Scan(&session); err != nil {
			rows.Close()
			return nil, err
		}
		contributors = append(contributors, session)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var savedActor *string
	if err := tx.QueryRowContext(ctx, "SELECT origin_json FROM task_mutation_context WHERE id=1").Scan(&savedActor); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE task_mutation_context SET origin_json=NULL WHERE id=1"); err != nil {
		return nil, err
	}
	for _, stage := range stages {
		prefix, spec := "Review", codeReviewSpecification
		documentation := stage == "documentation"
		if documentation {
			prefix = "Documentation review"
			spec = documentationReviewSpecification
		}
		if stage == "submission" {
			spec = submissionReviewSpecification
		}
		var existing string
		err := tx.QueryRowContext(ctx, "SELECT review_task_id FROM task_reviews WHERE source_task_id=? AND source_done_at=? AND stage=?", source.ID, cycle, stage).Scan(&existing)
		if err == nil {
			t, err := readTask(ctx, tx, source.ProjectIdent, existing)
			if err != nil {
				return nil, err
			}
			if t != nil {
				result = append(result, *t)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		labels := []string{"task-review", "review-of:" + source.ID}
		if documentation {
			labels = append(labels, "documentation-review")
		}
		if preferred != nil {
			labels = append(labels, "review-provider:"+*preferred)
		}
		for _, label := range source.Labels {
			if strings.HasPrefix(label, "os:") || (documentation && strings.HasPrefix(label, "review-exclude:")) {
				labels = append(labels, label)
			}
		}
		addExclude := func(session string) {
			label := "review-exclude:" + session
			if !hasLabel(labels, label) {
				labels = append(labels, label)
			}
		}
		if implementer != nil {
			addExclude(implementer.SessionID)
		} else if source.OwnerAgentID != nil {
			addExclude(*source.OwnerAgentID)
		}
		for _, session := range contributors {
			addExclude(session)
		}
		// Substitute placeholders in one pass: task titles cannot create another
		// template replacement when they themselves contain placeholder text.
		specification := strings.NewReplacer("{source_task_id}", source.ID, "{source_title}", source.Title).Replace(spec)
		title := prefix + ": " + source.Title
		var hostname *string
		if name, err := os.Hostname(); err == nil {
			hostname = &name
		}
		task, err := insertTaskRecord(ctx, tx, TaskInsert{Project: source.ProjectIdent, Title: title, Details: &specification, Labels: labels, Hostname: hostname, Reporter: "amux-autoreview", ParentID: &source.ID, Goal: source.Goal}, nil, "software")
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO task_reviews(source_task_id,source_done_at,review_task_id,stage) VALUES(?,?,?,?)", source.ID, cycle, task.ID, stage); err != nil {
			return nil, err
		}
		result = append(result, *task)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE task_mutation_context SET origin_json=? WHERE id=1", savedActor); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) TaskReviewChildren(ctx context.Context, project, id string) ([]Task, error) {
	result := []Task{}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT r.review_task_id FROM task_reviews r JOIN tasks t ON t.id=r.source_task_id WHERE r.source_task_id=? AND t.project_ident=? ORDER BY r.source_done_at,r.stage", id, strings.ToLower(strings.TrimSpace(project)))
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		t, err := readTask(ctx, tx, project, id)
		if err != nil {
			return nil, err
		}
		if t != nil {
			result = append(result, *t)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// EnqueueTaskReviews is the explicit review endpoint; an operator's request is
// independent of the automatic review plugin's enabled/disabled setting.
func (s *Store) EnqueueTaskReviews(ctx context.Context, project, id string) ([]Task, error) {
	var result []Task
	err := s.taskMutation(ctx, nil, func(tx *sql.Tx, _ *AgentOrigin) error {
		source, err := readTask(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if source == nil {
			return ErrTaskNotFound
		}
		result, err = enqueueTaskReviews(ctx, tx, source)
		return err
	})
	return result, err
}
