package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/nitecon/amux/identity/agent"
)

func TestObsoleteSubmissionReviewCanCloseWithoutChangingSource(t *testing.T) {
	for _, state := range []string{"awaiting review", "reopened", "superseded", "completed"} {
		for _, human := range []bool{false, true} {
			name := state + "/agent"
			if human {
				name = state + "/human"
			}
			t.Run(name, func(t *testing.T) {
				s := taskStore(t)
				ctx := context.Background()
				implementer := taskActor()
				reviewer := *implementer
				reviewer.SessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
				reviewer.Provider = "claude"
				source := makeTask(t, s, implementer)
				submit := func(cycle int64) Task {
					t.Helper()
					if _, err := s.TransitionTask(ctx, "tasks", source.ID, "in_progress", implementer); err != nil {
						t.Fatal(err)
					}
					parked, err := s.UpdateTaskWorkflow(ctx, "tasks", source.ID, TaskWorkflowUpdate{
						Status: "user_review", ReviewAction: ptr("Review the published change"), Changeset: json.RawMessage(`["main.go"]`),
					}, implementer)
					if err != nil {
						t.Fatal(err)
					}
					var reviews []Task
					err = s.taskMutation(ctx, nil, func(tx *sql.Tx, _ *agent.Origin) error {
						var err error
						reviews, err = enqueueReviewStages(ctx, tx, parked, cycle, []string{"submission"})
						return err
					})
					if err != nil || len(reviews) != 1 {
						t.Fatalf("submission review: %+v %v", reviews, err)
					}
					return reviews[0]
				}
				review := submit(1)
				if _, err := s.TransitionTask(ctx, "tasks", review.ID, "in_progress", &reviewer); err != nil {
					t.Fatal(err)
				}
				if state != "awaiting review" {
					if _, err := s.UpdateTaskWorkflow(ctx, "tasks", source.ID, TaskWorkflowUpdate{
						Status: "todo", Policy: TaskTransitionPolicy{HumanRequest: true},
					}, nil); err != nil {
						t.Fatal(err)
					}
				}
				if state == "superseded" || state == "completed" {
					current := submit(2)
					if state == "completed" {
						if _, err := s.TransitionTask(ctx, "tasks", current.ID, "in_progress", &reviewer); err != nil {
							t.Fatal(err)
						}
						if _, err := s.FinishSubmissionReview(ctx, "tasks", current.ID, "approved", "Reviewed the replacement submission", nil, &reviewer); err != nil {
							t.Fatal(err)
						}
					}
				}
				before, err := s.Task(ctx, "tasks", source.ID)
				if err != nil {
					t.Fatal(err)
				}
				if state != "awaiting review" {
					if _, err := s.FinishSubmissionReview(ctx, "tasks", review.ID, "approved", "Stale approval must be rejected", nil, &reviewer); !errors.Is(err, ErrTaskConflict) {
						t.Fatalf("stale verdict returned %v", err)
					}
				}
				actor := &reviewer
				if human {
					actor = nil
				}
				done, err := s.UpdateTaskWorkflow(ctx, "tasks", review.ID, TaskWorkflowUpdate{
					Status: "done", Changeset: json.RawMessage(`null`), Policy: TaskTransitionPolicy{HumanRequest: human},
				}, actor)
				if state == "awaiting review" {
					if !errors.Is(err, ErrTaskConflict) {
						t.Fatalf("active review closed without a verdict: %+v %v", done, err)
					}
				} else if err != nil || done.Status != "done" {
					t.Fatalf("obsolete review remained stuck: %+v %v", done, err)
				}
				after, err := s.Task(ctx, "tasks", source.ID)
				if err != nil {
					t.Fatal(err)
				}
				beforeJSON, _ := json.Marshal(before)
				afterJSON, _ := json.Marshal(after)
				if string(beforeJSON) != string(afterJSON) {
					t.Fatal("closing the obsolete review changed its source or recorded an approval")
				}
				if state == "reopened" {
					if _, err := s.TransitionTask(ctx, "tasks", source.ID, "in_progress", implementer); err != nil {
						t.Fatal(err)
					}
					if _, err := s.UpdateTaskWorkflow(ctx, "tasks", source.ID, TaskWorkflowUpdate{
						Status: "user_review", ReviewAction: ptr("Confirm the ordinary decision"), Changeset: json.RawMessage(`null`),
					}, implementer); err != nil {
						t.Fatal(err)
					}
					if _, err := s.UpdateTaskWorkflow(ctx, "tasks", source.ID, TaskWorkflowUpdate{
						Status: "done", Policy: TaskTransitionPolicy{HumanRequest: true},
					}, nil); err != nil {
						t.Fatal(err)
					}
					var stage string
					if err := s.db.QueryRowContext(ctx, "SELECT stage FROM task_reviews WHERE review_task_id=?", review.ID).Scan(&stage); err != nil || stage != "obsolete" {
						t.Fatalf("obsolete review was promoted on later source completion: stage=%q error=%v", stage, err)
					}
					var freshReview string
					if err := s.db.QueryRowContext(ctx, "SELECT review_task_id FROM task_reviews WHERE source_task_id=? AND stage='code'", source.ID).Scan(&freshReview); err != nil || freshReview == review.ID {
						t.Fatalf("later completion reused an obsolete review: review=%q error=%v", freshReview, err)
					}
				}
			})
		}
	}
}
