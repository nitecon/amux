package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var ErrTaskGuidedQuestion = errors.New("task mutation confirmation required")

var expectedTaskStatuses = [...]string{"todo", "in_progress", "stalled", "user_review", "done", "cancelled"}

type TaskGuidedQuestion struct{ TaskID string }

func (e *TaskGuidedQuestion) Error() string {
	return fmt.Sprintf("Did this task mutate any files?\nTask: %s\nThe task has not moved. If files were mutated, run `amux set-task %s software` (MCP `task_confirm_mutation`), then follow the changeset instruction. If no files were mutated, run `amux set-changeset %s null` (MCP `task_set_changeset` with changeset null).", e.TaskID, e.TaskID, e.TaskID)
}
func (e *TaskGuidedQuestion) Unwrap() error { return ErrTaskGuidedQuestion }

type TaskWorkflowUpdate struct {
	TaskMetadataUpdate
	Status              string               `json:"status,omitempty"`
	ExpectedStatus      *string              `json:"expected_status,omitempty"`
	ReviewAction        *string              `json:"review_action,omitempty"`
	Changeset           json.RawMessage      `json:"changeset,omitempty"`
	SubmissionCommentID string               `json:"submission_comment_id,omitempty"`
	ConfirmationToken   *string              `json:"confirmation_token,omitempty"`
	Policy              TaskTransitionPolicy `json:"-"`
}
type TaskWorkflowConfirmation struct {
	TaskType  *string
	Changeset json.RawMessage
}
type TaskConfirmationResult struct {
	Task           *Task
	NeedsChangeset bool
}

func validateExpectedTaskStatus(expected *string) error {
	if expected == nil {
		return nil
	}
	for _, status := range expectedTaskStatuses {
		if *expected == status {
			return nil
		}
	}
	return fmt.Errorf("%w: expected_status is not a known task status", ErrTaskMetadataInput)
}

func checkExpectedTaskStatus(expected *string, actual string) error {
	if expected != nil && *expected != actual {
		return fmt.Errorf("%w: expected task status %q, current status is %q", ErrTaskConflict, *expected, actual)
	}
	return nil
}

// ValidateChangeset preserves literal path strings; it never resolves files or
// normalizes away significant whitespace. Size is the canonical JSON byte size.
func ValidateChangeset(raw json.RawMessage) ([]string, error) {
	invalid := func() ([]string, error) {
		return nil, fmt.Errorf("%w: changeset must be null or a non-empty array of file paths (at most 64 KiB)", ErrTaskConflict)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var paths []string
	if len(raw) == 0 || json.Unmarshal(raw, &paths) != nil || len(paths) == 0 || len(paths) > 1024 {
		return invalid()
	}
	size := 2 + len(paths) - 1
	for _, path := range paths {
		if strings.TrimSpace(path) == "" || len(path) > 4096 || strings.ContainsRune(path, 0) {
			return invalid()
		}
		size += 2
		for _, r := range path {
			switch r {
			case '"', '\\', '\b', '\f', '\n', '\r', '\t':
				size += 2
			default:
				if r < 32 {
					size += 6
				} else {
					size += utf8.RuneLen(r)
				}
			}
		}
	}
	if size > 65536 {
		return invalid()
	}
	return paths, nil
}

func ordinaryWorkflowTask(ctx context.Context, q queryer, t *Task) error {
	if t.Kind == "delegated" {
		link, err := delegationBySource(ctx, q, t.ProjectIdent, t.ID)
		if err != nil {
			return err
		}
		if link == nil {
			return ErrTaskUnsupported
		}
	} else if t.Kind != "normal" {
		return ErrTaskUnsupported
	}
	var table bool
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='task_dispatches')").Scan(&table); err != nil {
		return err
	}
	if table {
		var managed bool
		if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM task_dispatches WHERE task_id=?)", t.ID).Scan(&managed); err != nil {
			return err
		}
		if managed {
			return ErrTaskUnsupported
		}
	}
	return nil
}

func (s *Store) UpdateTaskWorkflow(ctx context.Context, project, id string, update TaskWorkflowUpdate, origin *AgentOrigin) (*Task, error) {
	if update.Status != "" && update.Status != "todo" && update.Status != "in_progress" && update.Status != "user_review" && update.Status != "done" {
		return nil, fmt.Errorf("%w: invalid task status", ErrTaskConflict)
	}
	if err := validateExpectedTaskStatus(update.ExpectedStatus); err != nil {
		return nil, err
	}
	if err := ValidateTaskMetadata(update.TaskMetadataUpdate); err != nil {
		return nil, err
	}
	if update.Changeset != nil {
		if _, err := ValidateChangeset(update.Changeset); err != nil {
			return nil, err
		}
	}
	var result *Task
	guided := false
	err := s.taskMutation(ctx, origin, func(tx *sql.Tx, actor *AgentOrigin) error {
		t, err := readTask(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if t == nil {
			return ErrTaskNotFound
		}
		if err := checkExpectedTaskStatus(update.ExpectedStatus, t.Status); err != nil {
			return err
		}
		if err := ordinaryWorkflowTask(ctx, tx, t); err != nil {
			return err
		}
		if update.ConfirmationToken != nil {
			pending, err := pendingTaskConfirmation(ctx, tx, t, actor)
			if err != nil {
				return err
			}
			if pending.token != *update.ConfirmationToken {
				return fmt.Errorf("%w: confirmation was replaced; repeat the transition", ErrTaskConflict)
			}
			nonnull := !bytes.Equal(bytes.TrimSpace(update.Changeset), []byte("null"))
			if update.Changeset == nil || (pending.mutated && !nonnull) || (!pending.mutated && nonnull) {
				return fmt.Errorf("%w: confirm file mutation with set-task before submitting files; confirmed files cannot be discarded", ErrTaskConflict)
			}
		}
		confirming := update.Status != "" && update.Status != "in_progress" && (t.Status != update.Status || (update.Status == "todo" && t.OwnerOrigin != nil))
		missing := actor != nil && confirming && update.Changeset == nil && update.SubmissionCommentID == ""
		if missing {
			// Preview the complete transition in a savepoint. Ownership and review-action
			// validation happen normally; no speculative mutation/event escapes it.
			if _, err := tx.ExecContext(ctx, "SAVEPOINT task_confirmation_preview"); err != nil {
				return err
			}
			if _, err := applyTaskWorkflow(ctx, tx, t, update, actor, s.routerReviews.Load()); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "ROLLBACK TO task_confirmation_preview; RELEASE task_confirmation_preview"); err != nil {
				return err
			}
			if update.Status != "done" && update.Status != "user_review" {
				return ErrTaskChangeset
			}
			if update.Status != "done" && (t.OwnerOrigin == nil || actor == nil || *t.OwnerOrigin != *actor) {
				return ErrTaskOwnership
			}
			token, err := uuid.NewV7()
			if err != nil {
				return err
			}
			snapshot, _ := json.Marshal(t)
			request, _ := json.Marshal(taskConfirmationRequest{Update: update, Policy: update.Policy})
			originJSON, _ := json.Marshal(actor)
			if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO task_confirmations(task_id,token,origin_json,snapshot_json,request_json) VALUES(?,?,?,?,?)", id, token.String(), string(originJSON), string(snapshot), string(request)); err != nil {
				return err
			}
			guided = true
			return nil
		}
		result, err = applyTaskWorkflow(ctx, tx, t, update, actor, s.routerReviews.Load())
		if err != nil {
			return err
		}
		if confirming {
			if _, err := tx.ExecContext(ctx, "DELETE FROM task_confirmations WHERE task_id=?", id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if guided {
		return nil, &TaskGuidedQuestion{TaskID: id}
	}
	return result, nil
}

type taskConfirmationRequest struct {
	Update TaskWorkflowUpdate   `json:"update"`
	Policy TaskTransitionPolicy `json:"policy"`
}

func applyTaskWorkflow(ctx context.Context, tx *sql.Tx, t *Task, update TaskWorkflowUpdate, actor *AgentOrigin, routerReviews bool) (*Task, error) {
	if err := checkExpectedTaskStatus(update.ExpectedStatus, t.Status); err != nil {
		return nil, err
	}
	human := update.Policy.HumanRequest
	requestedStatus := update.Status
	status := requestedStatus
	if status == "" {
		status = t.Status
	}
	if human && actor != nil {
		return nil, fmt.Errorf("%w: user controls require a human request", ErrTaskConflict)
	}
	if t.Status == "cancelled" && requestedStatus != "" && update.Status != "done" {
		return nil, fmt.Errorf("%w: cancelled tasks are terminal", ErrTaskConflict)
	}
	if status == "user_review" && (requestedStatus == "user_review" || update.ReviewAction != nil) {
		if update.ReviewAction == nil || len(strings.TrimSpace(*update.ReviewAction)) == 0 || len(strings.TrimSpace(*update.ReviewAction)) > 4096 {
			return nil, fmt.Errorf("%w: user review requires a specific review_action (1 to 4096 bytes)", ErrTaskConflict)
		}
		if !human {
			if t.Status == "user_review" {
				return nil, fmt.Errorf("%w: tasks in user review require a user decision", ErrTaskConflict)
			}
			if (t.OwnerOrigin == nil && (t.OwnerAgentID != nil || (t.Status == "in_progress" || t.Status == "stalled"))) || (t.OwnerOrigin != nil && (actor == nil || actor.SessionID != t.OwnerOrigin.SessionID || actor.InstanceID != t.OwnerOrigin.InstanceID)) {
				return nil, ErrTaskOwnership
			}
		}
	} else if update.ReviewAction != nil {
		return nil, fmt.Errorf("%w: review_action requires status user_review", ErrTaskConflict)
	}
	updated, err := applyTaskMetadata(ctx, tx, t, update.TaskMetadataUpdate, requestedStatus, update.Policy, actor)
	if err != nil {
		return nil, err
	}
	t = updated
	if requestedStatus == "" && t.Status == "user_review" && update.ReviewAction != nil {
		if !human && actor != nil {
			return nil, fmt.Errorf("%w: tasks in user review require a user decision", ErrTaskConflict)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET review_action=?,updated_at=? WHERE id=?", strings.TrimSpace(*update.ReviewAction), time.Now().UnixMilli(), t.ID); err != nil {
			return nil, err
		}
		t, err = readTask(ctx, tx, t.ProjectIdent, t.ID)
		if err != nil {
			return nil, err
		}
	}
	activePolicy := update.Policy
	activePolicy.RequireChangeset = false
	if update.Goal != nil {
		activePolicy.CheckGoal = false // already checked against the pre-update goal.
	}
	confirming := requestedStatus != "" && update.Status != "in_progress" && (t.Status != update.Status || (update.Status == "todo" && t.OwnerOrigin != nil))
	nextType := t.TaskType
	if update.SubmissionCommentID != "" {
		if requestedStatus != "user_review" || (t.Status != "in_progress" && t.Status != "stalled") || actor == nil ||
			t.OwnerOrigin == nil || *t.OwnerOrigin != *actor {
			return nil, fmt.Errorf("%w: a submission comment requires an owned task entering user review", ErrTaskConflict)
		}
		origin, err := json.Marshal(actor)
		if err != nil {
			return nil, err
		}
		var proof bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_comments
			WHERE id=? AND task_id=? AND author_type='agent' AND origin_json=? AND content LIKE 'Work submitted: %')`,
			update.SubmissionCommentID, t.ID, string(origin)).Scan(&proof); err != nil {
			return nil, err
		}
		if !proof {
			return nil, fmt.Errorf("%w: missing submission comment for this task and execution", ErrTaskConflict)
		}
		if !hasLabel(t.Labels, "task-review") {
			nextType = "software"
		}
	}
	if confirming && update.Changeset != nil {
		paths, err := ValidateChangeset(update.Changeset)
		if err != nil {
			return nil, err
		}
		if update.SubmissionCommentID != "" && len(paths) == 0 {
			return nil, fmt.Errorf("%w: a software submission must include changed files or reference its published result", ErrTaskConflict)
		}
		files := append([]string(nil), t.Changeset...)
		for _, path := range paths {
			found := false
			for _, prior := range files {
				if prior == path {
					found = true
					break
				}
			}
			if !found {
				files = append(files, path)
			}
		}
		var raw any
		if len(files) > 0 {
			encoded, _ := json.Marshal(files)
			raw = string(encoded)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET changeset_json=? WHERE id=?", raw, t.ID); err != nil {
			return nil, err
		}
		if !hasLabel(t.Labels, "task-review") {
			nextType = "general"
			if len(files) > 0 {
				nextType = "software"
			}
		}
	}
	now := time.Now().UnixMilli()
	switch requestedStatus {
	case "todo", "in_progress":
		if t.Status == "user_review" {
			if !human || update.Status != "todo" {
				return nil, fmt.Errorf("%w: tasks in user review require a user decision", ErrTaskConflict)
			}
			labels := append([]string(nil), t.Labels...)
			if hasLabel(labels, "autoreview-finding") {
				clean := labels[:0]
				for _, label := range labels {
					if label != "autoreview-finding" {
						clean = append(clean, label)
					}
				}
				labels = clean
			}
			raw, _ := json.Marshal(labels)
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='todo',owner_agent_id=NULL,owner_origin_json=NULL,started_at=NULL,done_at=NULL,review_action=NULL,routing_json='{"mode":"auto"}',labels=?,updated_at=? WHERE id=?`, string(raw), now, t.ID); err != nil {
				return nil, err
			}
		} else if t.Status == "done" {
			if !human {
				return nil, ErrTaskUnsupported
			}
			if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status='todo',done_at=NULL,review_action=NULL,updated_at=? WHERE id=?", now, t.ID); err != nil {
				return nil, err
			}
			if update.Status == "in_progress" {
				if _, err := transitionActiveTask(ctx, tx, t.ProjectIdent, t.ID, update.Status, actor, activePolicy); err != nil {
					return nil, err
				}
			}
		} else {
			if _, err := transitionActiveTask(ctx, tx, t.ProjectIdent, t.ID, update.Status, actor, activePolicy); err != nil {
				return nil, err
			}
		}
	case "user_review":
		action := strings.TrimSpace(*update.ReviewAction)
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status='user_review',review_action=?,owner_agent_id=NULL,owner_origin_json=NULL,started_at=NULL,done_at=NULL,updated_at=? WHERE id=?", action, now, t.ID); err != nil {
			return nil, err
		}
	case "done":
		if actor != nil && (t.Status == "in_progress" || t.Status == "stalled") && nextType == "software" &&
			t.Kind != "delegated" && !hasLabel(t.Labels, "task-review") {
			var assigned bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM amux_assignment_runs WHERE task_id=?)`, t.ID).Scan(&assigned); err != nil {
				return nil, err
			}
			enabled, err := automaticReviewsEnabled(ctx, tx)
			if err != nil {
				return nil, err
			}
			if assigned && enabled {
				return nil, fmt.Errorf("%w: publish software changes with submit_work to start code review", ErrTaskConflict)
			}
		}
		if hasLabel(t.Labels, "task-review") && !update.Policy.submissionReviewVerdict {
			source, err := currentSubmissionReviewSource(ctx, tx, t.ProjectIdent, t.ID)
			if err != nil {
				return nil, err
			}
			if source != nil && source.Status == "user_review" && source.CompletionOwner != nil {
				return nil, fmt.Errorf("%w: finish this code review using task_finish_review", ErrTaskConflict)
			}
			// Preserve the obsolete review without letting a later source completion
			// reuse its ordinary closure as a code-review verdict.
			if _, err := tx.ExecContext(ctx, `UPDATE task_reviews SET stage='obsolete'
				WHERE review_task_id=? AND stage='submission'`, t.ID); err != nil {
				return nil, err
			}
		}
		var done any = t.DoneAt
		if t.Status != "done" {
			done = now
		}
		owner := t.OwnerOrigin
		ownerID := t.OwnerAgentID
		if t.Status == "user_review" {
			if err := finalizeSubmissionReview(ctx, tx, t.ID, now); err != nil {
				return nil, err
			}
			prior := t.CompletionOwner
			if prior == nil || (actor != nil && prior.SessionID == actor.SessionID) {
				owner = actor
				ownerID = nil
				if actor != nil {
					ownerID = ptr(actor.SessionID)
				}
			} else {
				owner = prior
			}
		}
		var raw any
		if owner != nil {
			encoded, _ := json.Marshal(owner)
			raw = string(encoded)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status='done',done_at=?,review_action=NULL,owner_origin_json=?,owner_agent_id=?,updated_at=? WHERE id=?", done, raw, ownerID, now, t.ID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM amux_review_rework WHERE source_task_id=?", t.ID); err != nil {
			return nil, err
		}
	}
	if nextType != t.TaskType {
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET task_type=? WHERE id=?", nextType, t.ID); err != nil {
			return nil, err
		}
		if _, err := insertTaskComment(ctx, tx, t.ID, "amux", "system", fmt.Sprintf("Task type changed from %s to %s.", t.TaskType, nextType), nil, actor); err != nil {
			return nil, err
		}
	}
	result, err := readTask(ctx, tx, t.ProjectIdent, t.ID)
	if err != nil {
		return nil, err
	}
	if update.SubmissionCommentID != "" {
		enabled, err := automaticReviewsEnabled(ctx, tx)
		if err != nil {
			return nil, err
		}
		if enabled {
			if _, err := enqueueReviewStages(ctx, tx, result, result.UpdatedAt, []string{"submission"}); err != nil {
				return nil, err
			}
		}
	}
	if result != nil && requestedStatus != "" {
		if err := completeTaskDelegation(ctx, tx, t, result, actor); err != nil {
			return nil, err
		}
		result, err = readTask(ctx, tx, result.ProjectIdent, result.ID)
		if err != nil {
			return nil, err
		}
	}
	if requestedStatus == "done" && t.Status != "done" && !routerReviews {
		enabled, err := automaticReviewsEnabled(ctx, tx)
		if err != nil {
			return nil, err
		}
		if enabled {
			if err := queueCompletionReviews(ctx, tx, result); err != nil {
				return nil, err
			}
		}
	}

	return result, nil
}

type taskPending struct {
	token, request string
	mutated        bool
}

func pendingTaskConfirmation(ctx context.Context, q queryer, t *Task, actor *AgentOrigin) (taskPending, error) {
	var pending taskPending
	var rawActor, snapshot string
	err := q.QueryRowContext(ctx, "SELECT token,origin_json,snapshot_json,request_json,mutated FROM task_confirmations WHERE task_id=?", t.ID).Scan(&pending.token, &rawActor, &snapshot, &pending.request, &pending.mutated)
	if errors.Is(err, sql.ErrNoRows) {
		return pending, fmt.Errorf("%w: no pending confirmation; repeat the requested task transition", ErrTaskConflict)
	}
	if err != nil {
		return pending, err
	}
	var savedActor *AgentOrigin
	var savedTask Task
	if json.Unmarshal([]byte(rawActor), &savedActor) != nil || json.Unmarshal([]byte(snapshot), &savedTask) != nil {
		return pending, fmt.Errorf("%w: invalid pending confirmation", ErrTaskConflict)
	}
	before, _ := json.Marshal(savedTask)
	current, _ := json.Marshal(t)
	if actor == nil || savedActor == nil || *actor != *savedActor || !bytes.Equal(before, current) {
		return pending, fmt.Errorf("%w: task changed since the question; repeat the requested transition", ErrTaskConflict)
	}
	return pending, nil
}

func (s *Store) ConfirmTaskWorkflow(ctx context.Context, project, id string, input TaskWorkflowConfirmation, origin *AgentOrigin) (TaskConfirmationResult, error) {
	if (input.TaskType != nil) == (input.Changeset != nil) {
		return TaskConfirmationResult{}, fmt.Errorf("%w: supply mutation confirmation or a changeset, not both", ErrTaskConflict)
	}
	if input.TaskType != nil && *input.TaskType != "software" {
		return TaskConfirmationResult{}, fmt.Errorf("%w: set-task confirms software mutation only; for no changes use set-changeset null", ErrTaskConflict)
	}
	if input.Changeset != nil {
		if _, err := ValidateChangeset(input.Changeset); err != nil {
			return TaskConfirmationResult{}, err
		}
	}
	var result TaskConfirmationResult
	err := s.taskMutation(ctx, origin, func(tx *sql.Tx, actor *AgentOrigin) error {
		t, err := readTask(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if t == nil {
			return ErrTaskNotFound
		}
		if err := ordinaryWorkflowTask(ctx, tx, t); err != nil {
			return err
		}
		pending, err := pendingTaskConfirmation(ctx, tx, t, actor)
		if err != nil {
			return err
		}
		var update TaskWorkflowUpdate
		var saved taskConfirmationRequest
		if err := json.Unmarshal([]byte(pending.request), &saved); err == nil && saved.Update.Status != "" {
			update = saved.Update
			update.Policy = saved.Policy
		} else {
			decoder := json.NewDecoder(strings.NewReader(pending.request))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&update); err != nil {
				return ErrTaskUnsupported
			}
		}
		if err := validateExpectedTaskStatus(update.ExpectedStatus); err != nil {
			return err
		}
		if err := checkExpectedTaskStatus(update.ExpectedStatus, t.Status); err != nil {
			return err
		}
		if input.TaskType != nil {
			if t.Status != "user_review" && t.Status != "cancelled" && t.OwnerOrigin != nil && *t.OwnerOrigin == *actor && !hasLabel(t.Labels, "task-review") && t.TaskType != "software" {
				if _, err := tx.ExecContext(ctx, "UPDATE tasks SET task_type='software',updated_at=? WHERE id=?", time.Now().UnixMilli(), id); err != nil {
					return err
				}
				if _, err := insertTaskComment(ctx, tx, id, "amux", "system", fmt.Sprintf("Task type changed from %s to software.", t.TaskType), nil, actor); err != nil {
					return err
				}
				t, err = readTask(ctx, tx, project, id)
				if err != nil {
					return err
				}
			}
			snapshot, _ := json.Marshal(t)
			if _, err := tx.ExecContext(ctx, "UPDATE task_confirmations SET mutated=1,snapshot_json=? WHERE task_id=?", string(snapshot), id); err != nil {
				return err
			}
			result.NeedsChangeset = true
			return nil
		}
		nonnull := !bytes.Equal(bytes.TrimSpace(input.Changeset), []byte("null"))
		if (pending.mutated && !nonnull) || (!pending.mutated && nonnull) {
			return fmt.Errorf("%w: confirm file mutation with set-task before submitting files; confirmed files cannot be discarded", ErrTaskConflict)
		}
		update.Changeset = input.Changeset
		update.ConfirmationToken = &pending.token
		result.Task, err = applyTaskWorkflow(ctx, tx, t, update, actor, s.routerReviews.Load())
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM task_confirmations WHERE task_id=?", id)
		return err
	})
	return result, err
}
