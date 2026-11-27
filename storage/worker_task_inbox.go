package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

func migrateWorkerTaskInbox(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS amux_worker_task_consumers(
			server TEXT PRIMARY KEY, consumer TEXT NOT NULL, committed_event INTEGER NOT NULL DEFAULT 0);
		CREATE TABLE IF NOT EXISTS amux_worker_task_inbox(
			server TEXT NOT NULL, project TEXT NOT NULL, task TEXT NOT NULL, owner TEXT NOT NULL,
			event_id INTEGER NOT NULL, PRIMARY KEY(server,project,task));
		CREATE INDEX IF NOT EXISTS amux_worker_task_inbox_owner
			ON amux_worker_task_inbox(server,project,owner,event_id);
	`)
	if err != nil {
		return err
	}
	exists, err := sqliteTableHasColumn(ctx, tx, "amux_worker_task_consumers", "bootstrapped")
	if err != nil || exists {
		return err
	}
	// Retire consumers created by the old replay-from-zero bootstrap. Existing
	// journal entries remain available and are fenced by live ownership checks.
	_, err = tx.ExecContext(ctx, `ALTER TABLE amux_worker_task_consumers ADD COLUMN bootstrapped INTEGER NOT NULL DEFAULT 0;
		DELETE FROM amux_worker_task_consumers;`)
	return err
}

// WorkerTaskConsumer returns a stable gateway-specific subscription identity.
func (s *Store) WorkerTaskConsumer(ctx context.Context, server string) (string, int64, error) {
	id := uuid.NewString()
	if _, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO amux_worker_task_consumers(server,consumer) VALUES(?,?)", server, id); err != nil {
		return "", 0, err
	}
	var committed int64
	err := s.db.QueryRowContext(ctx, "SELECT consumer,CASE WHEN bootstrapped=1 THEN committed_event ELSE -1 END FROM amux_worker_task_consumers WHERE server=?", server).Scan(&id, &committed)
	return id, committed, err
}

// RecordWorkerTaskEvent atomically advances the durable inbox cursor and saves
// work only for an exact, locally journaled worker identity.
func (s *Store) RecordWorkerTaskEvent(ctx context.Context, server string, event TaskEvent) (bool, error) {
	// Historical events may encode unrelated task fields differently. Decode
	// only the routing fields. The gateway checks live ownership before work.
	var task struct {
		ID           string       `json:"id"`
		ProjectIdent string       `json:"project_ident"`
		Status       string       `json:"status"`
		OwnerOrigin  *AgentOrigin `json:"owner_origin"`
	}
	if len(event.Task) > 0 {
		if err := json.Unmarshal(event.Task, &task); err != nil {
			return false, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE amux_worker_task_consumers SET committed_event=? WHERE server=? AND committed_event<?", event.ID, server, event.ID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return false, err
	}
	queued := false
	if task.ID != "" && task.ProjectIdent == event.ProjectIdent {
		if _, err := tx.ExecContext(ctx, "DELETE FROM amux_worker_task_inbox WHERE server=? AND project=? AND task=?", server, event.ProjectIdent, task.ID); err != nil {
			return false, err
		}
		if task.OwnerOrigin != nil && (task.Status == "todo" || (task.Status == "in_progress" || task.Status == "stalled")) {
			queued, err = queueWorkerTask(ctx, tx, server, task.ProjectIdent, task.ID, task.OwnerOrigin, event.ID)
			if err != nil {
				return false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if queued {
		s.wakeWorkerTasks()
	}
	return queued, nil
}

type PendingWorkerTask struct {
	TaskID  string
	EventID int64
}

func (s *Store) PendingWorkerTask(ctx context.Context, server, project string, origin AgentOrigin) (PendingWorkerTask, error) {
	var pending PendingWorkerTask
	owner, err := json.Marshal(origin)
	if err != nil {
		return pending, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT task,event_id FROM amux_worker_task_inbox
		WHERE server=? AND project=? AND owner=? ORDER BY event_id LIMIT 1`, server, project, string(owner)).Scan(&pending.TaskID, &pending.EventID)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return pending, err
}

func (s *Store) ForgetWorkerTask(ctx context.Context, server, project string, pending PendingWorkerTask) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM amux_worker_task_inbox WHERE server=? AND project=? AND task=? AND event_id=?", server, project, pending.TaskID, pending.EventID)
	return err
}

// WorkerTaskChanges signals a newly committed assignment in the local inbox.
func (s *Store) WorkerTaskChanges() <-chan struct{} {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	return s.workerTaskChanges
}

func queueWorkerTask(ctx context.Context, tx *sql.Tx, server string, project, id string, origin *AgentOrigin, cursor int64) (bool, error) {
	owner, err := json.Marshal(origin)
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO amux_worker_task_inbox(server,project,task,owner,event_id)
		SELECT ?,?,?,?,? WHERE EXISTS(
			SELECT 1 FROM amux_worker_pool_slots slot JOIN amux_worker_pools pool ON pool.id=slot.pool_id
			WHERE pool.server=? AND pool.project=? AND slot.owner_json=?)
		ON CONFLICT(server,project,task) DO UPDATE SET owner=excluded.owner,event_id=excluded.event_id`,
		server, project, id, string(owner), cursor, server, project, string(owner))
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted > 0, err
}

func (s *Store) wakeWorkerTasks() {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	close(s.workerTaskChanges)
	s.workerTaskChanges = make(chan struct{})
}

// BootstrapWorkerTasks reconciles current assignments at the first subscription
// boundary. Subsequent reconnects always replay from the recorded cursor.
func (s *Store) BootstrapWorkerTasks(ctx context.Context, server string, cursor int64, tasks []Task) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE amux_worker_task_consumers SET bootstrapped=1,committed_event=? WHERE server=?", cursor, server); err != nil {
		return err
	}
	for _, task := range tasks {
		if _, err := queueWorkerTask(ctx, tx, server, task.ProjectIdent, task.ID, task.OwnerOrigin, cursor); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(tasks) > 0 {
		s.wakeWorkerTasks()
	}
	return nil
}
