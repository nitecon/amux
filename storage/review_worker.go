package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/nitecon/amux/eventbus"
)

// queueCompletionReviews keeps completion independent from review execution.
// A failed attempt rolls back only generated review work; the durable outbox
// remains in the completion transaction for restart-safe retry.
func queueCompletionReviews(ctx context.Context, tx *sql.Tx, source *Task) error {
	if source.Status != "done" || source.DoneAt == nil || source.TaskType != "software" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO task_review_outbox(source_task_id,source_done_at) VALUES(?,?)", source.ID, *source.DoneAt); err != nil {
		return err
	}
	return attemptQueuedReviews(ctx, tx, source)
}
func attemptQueuedReviews(ctx context.Context, tx *sql.Tx, source *Task) error {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT completion_reviews"); err != nil {
		return err
	}
	_, err := enqueueTaskReviews(ctx, tx, source)
	if err != nil {
		if _, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO completion_reviews; RELEASE completion_reviews"); rollbackErr != nil {
			return rollbackErr
		}
		message := err.Error()
		if len(message) > 4096 {
			message = message[:4096]
		}
		_, err = tx.ExecContext(ctx, "UPDATE task_review_outbox SET last_error=?,attempts=attempts+1 WHERE source_task_id=? AND source_done_at=?", message, source.ID, *source.DoneAt)
		return err
	}
	if _, err := tx.ExecContext(ctx, "RELEASE completion_reviews"); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM task_review_outbox WHERE source_task_id=? AND source_done_at=?", source.ID, *source.DoneAt)
	return err
}

// RetryTaskReviews processes a bounded batch. The bool says a failed obligation
// remains; callers back off on failure rather than repeatedly waking themselves.
func (s *Store) RetryTaskReviews(ctx context.Context) (bool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT o.source_task_id,t.project_ident,o.source_done_at FROM task_review_outbox o JOIN tasks t ON t.id=o.source_task_id ORDER BY o.source_done_at,o.source_task_id LIMIT 100")
	if err != nil {
		return false, err
	}
	type job struct {
		id, project string
		completed   int64
	}
	jobs := []job{}
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.project, &j.completed); err != nil {
			rows.Close()
			return false, err
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	failed := false
	for _, j := range jobs {
		err := s.taskMutation(ctx, nil, func(tx *sql.Tx, _ *AgentOrigin) error {
			enabled, err := automaticReviewsEnabled(ctx, tx)
			if err != nil {
				return err
			}
			if !enabled {
				_, err := tx.ExecContext(ctx, "DELETE FROM task_review_outbox WHERE source_task_id=? AND source_done_at=?", j.id, j.completed)
				return err
			}
			source, err := readTask(ctx, tx, j.project, j.id)
			if err != nil {
				return err
			}
			if source == nil || source.Status != "done" || source.DoneAt == nil || *source.DoneAt != j.completed {
				_, err := tx.ExecContext(ctx, "DELETE FROM task_review_outbox WHERE source_task_id=? AND source_done_at=?", j.id, j.completed)
				return err
			}
			if err := attemptQueuedReviews(ctx, tx, source); err != nil {
				return err
			}
			var remains bool
			if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM task_review_outbox WHERE source_task_id=? AND source_done_at=?)", j.id, j.completed).Scan(&remains); err != nil {
				return err
			}
			failed = failed || remains
			return nil
		})
		if err != nil {
			return false, err
		}
	}
	return failed, nil
}

// RunTaskReviewWorker retries durable obligations on startup, task changes and
// reconciliation ticks. Conversation changes are ignored. Its timer is only
// failure backoff; while backing off, event volume cannot accelerate retries.
func (s *Store) RunTaskReviewWorker(ctx context.Context) error {
	return s.runTaskReviewWorker(ctx, nil)
}

// runTaskReviewWorker optionally reports completed reconciliation passes to
// package tests so they can synchronize without timing assumptions.
func (s *Store) runTaskReviewWorker(ctx context.Context, reconciled chan<- struct{}) error {
	backoff := time.Second
	for {
		sub := s.bus.Subscribe(128)
		failed, err := s.RetryTaskReviews(ctx)
		if err != nil {
			sub.Unsubscribe()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if reconciled != nil {
			select {
			case reconciled <- struct{}{}:
			default:
			}
		}
		if failed {
			sub.Unsubscribe()
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
			if backoff < 30*time.Second {
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
			continue
		}
		backoff = time.Second
		for {
			select {
			case <-ctx.Done():
				sub.Unsubscribe()
				return nil
			case event, ok := <-sub.C:
				if !ok {
					slow := errors.Is(sub.Err(), eventbus.ErrSlowConsumer)
					sub.Unsubscribe()
					if !slow && ctx.Err() == nil {
						return sub.Err()
					}
					break
				}
				if event.Topic == eventbus.TaskChanged || event.Topic == eventbus.Tick {
					sub.Unsubscribe()
					break
				}
				continue
			}
			break
		}
	}
}

func automaticReviewsEnabled(ctx context.Context, q queryer) (bool, error) {
	return pluginEnabled(ctx, q, "review")
}
