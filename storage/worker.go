package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
)

// WorkerRun is local recovery state, not server task authority. Credentials are
// supplied at runtime; the gateway credential is never stored in this journal.
type WorkerRun struct {
	ID                string      `json:"id"`
	Server            string      `json:"server"`
	Project           string      `json:"project"`
	ConversationID    string      `json:"conversation_id"`
	LocalTaskID       string      `json:"-"` // Task ID for a gateway without server-side conversations.
	Directory         string      `json:"directory"`
	Command           []string    `json:"command,omitempty"`
	Owner             AgentOrigin `json:"owner"`
	NativeSessionID   string      `json:"native_session_id"`
	PendingInstanceID string      `json:"pending_instance_id"`
	CommandCursor     int64       `json:"command_cursor"`
	EventCursor       int64       `json:"event_cursor"`
	OutboxCursor      int64       `json:"outbox_cursor"`
	PresenceSequence  int64       `json:"presence_sequence"`
	Goal              *string     `json:"goal,omitempty"`
}
type WorkerOutboxEntry struct {
	Sequence int64
	Kind     string
	ID       string
	Payload  json.RawMessage
}
type WorkerCommand struct {
	Record ConversationCommand
	State  string
}

// AcquireWorkerRun prevents two engines in one host from driving the same run.
// The host must also hold its journal profile's OS lock across processes.
func (s *Store) AcquireWorkerRun(id string) (func(), error) {
	if _, loaded := s.workerRuns.LoadOrStore(id, struct{}{}); loaded {
		return nil, ErrConversationConflict
	}
	var once sync.Once
	return func() { once.Do(func() { s.workerRuns.Delete(id) }) }, nil
}

func migrateWorker(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS amux_worker_runs(
 id TEXT PRIMARY KEY,server TEXT NOT NULL,project TEXT NOT NULL,conversation_id TEXT NOT NULL,
 directory TEXT NOT NULL,command_json TEXT NOT NULL DEFAULT '[]',owner_json TEXT NOT NULL,native_session_id TEXT NOT NULL DEFAULT '',
 pending_instance_id TEXT NOT NULL DEFAULT '',command_cursor INTEGER NOT NULL DEFAULT 0,
 event_cursor INTEGER NOT NULL DEFAULT 0,outbox_cursor INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS amux_worker_outbox(
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,run_id TEXT NOT NULL REFERENCES amux_worker_runs(id),
 kind TEXT NOT NULL,id TEXT NOT NULL,payload TEXT NOT NULL,UNIQUE(run_id,kind,id));
 CREATE INDEX IF NOT EXISTS amux_worker_outbox_run ON amux_worker_outbox(run_id,sequence);
 CREATE TABLE IF NOT EXISTS amux_worker_commands(
 run_id TEXT NOT NULL REFERENCES amux_worker_runs(id),command_id TEXT NOT NULL,
 sequence INTEGER NOT NULL,revision INTEGER NOT NULL,remote_json TEXT NOT NULL,
 local_state TEXT NOT NULL DEFAULT 'new',PRIMARY KEY(run_id,command_id));
 INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(7);`)
	return err
}
func readWorkerRun(ctx context.Context, q queryer, id string) (*WorkerRun, error) {
	var r WorkerRun
	var owner, command string
	var goal sql.NullString
	hasPresenceSequence, err := sqliteTableHasColumn(ctx, q, "amux_worker_runs", "presence_sequence")
	if err != nil {
		return nil, err
	}
	sequence := "0"
	if hasPresenceSequence {
		sequence = "presence_sequence"
	}
	err = q.QueryRowContext(ctx, `SELECT id,server,project,conversation_id,directory,command_json,owner_json,native_session_id,pending_instance_id,command_cursor,event_cursor,outbox_cursor,`+sequence+`,goal FROM amux_worker_runs WHERE id=?`, id).Scan(&r.ID, &r.Server, &r.Project, &r.ConversationID, &r.Directory, &command, &owner, &r.NativeSessionID, &r.PendingInstanceID, &r.CommandCursor, &r.EventCursor, &r.OutboxCursor, &r.PresenceSequence, &goal)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(owner), &r.Owner); err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(command), &r.Command); err != nil {
		return nil, err
	}
	r.Goal = optionalSQLString(goal)
	return &r, nil
}
func (s *Store) WorkerRun(ctx context.Context, id string) (*WorkerRun, error) {
	return readWorkerRun(ctx, s.db, id)
}

func (s *Store) ConversationWorkerRunID(ctx context.Context, server, project, conversation string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM amux_worker_runs WHERE server=? AND project=? AND conversation_id=? ORDER BY rowid DESC LIMIT 1`, server, project, conversation).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
func (s *Store) SaveWorkerRun(ctx context.Context, r WorkerRun) error {
	return insertWorkerRun(ctx, s.db, r)
}
func insertWorkerRun(ctx context.Context, q queryer, r WorkerRun) error {
	goal, err := NormalizeTaskGoal(r.Goal)
	if err != nil {
		return fmt.Errorf("invalid worker run goal: %w", err)
	}
	r.Goal = goal
	actor, err := NormalizeOrigin(&r.Owner)
	if err != nil {
		return err
	}
	if _, err = uuid.Parse(r.ID); err != nil {
		return fmt.Errorf("invalid worker run ID")
	}
	if _, err = uuid.Parse(r.ConversationID); err != nil {
		return fmt.Errorf("invalid worker conversation ID")
	}
	if r.Server == "" || r.Project == "" || !filepath.IsAbs(r.Directory) {
		return fmt.Errorf("worker requires explicit server, project and absolute working directory")
	}
	command, _ := json.Marshal(r.Command)
	if len(r.Command) > 64 || len(command) > 65536 || len(r.Command) > 0 && r.Command[0] == "" {
		return ErrConversationInput
	}
	owner, _ := json.Marshal(actor)
	_, err = q.ExecContext(ctx, `INSERT INTO amux_worker_runs(id,server,project,conversation_id,directory,command_json,owner_json,goal) VALUES(?,?,?,?,?,?,?,?)`, r.ID, r.Server, r.Project, r.ConversationID, r.Directory, string(command), string(owner), nullableString(r.Goal))
	return err
}
func (s *Store) mutateWorkerRun(ctx context.Context, id string, change func(*sql.Tx, *WorkerRun) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := readWorkerRun(ctx, tx, id)
	if err != nil {
		return err
	}
	if err = change(tx, r); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.wakeConversations()
	return nil
}
func (s *Store) QueueWorkerEvent(ctx context.Context, id string, event conversation.Event) (string, error) {
	raw, err := encodeConversationEvent(event)
	if err != nil {
		return "", err
	}
	eventID := uuid.NewString()
	err = s.mutateWorkerRun(ctx, id, func(tx *sql.Tx, r *WorkerRun) error {
		if r.PendingInstanceID != "" {
			return ErrConversationConflict
		}
		if event.Kind == "session_ready" && r.NativeSessionID == "" {
			if event.SessionID == "" || len(event.SessionID) > 4096 || strings.ContainsRune(event.SessionID, 0) {
				return ErrConversationInput
			}
			if _, err := tx.ExecContext(ctx, "UPDATE amux_worker_runs SET native_session_id=? WHERE id=?", event.SessionID, id); err != nil {
				return err
			}
			bind, _ := json.Marshal(map[string]string{"native_session_id": event.SessionID})
			if _, err := tx.ExecContext(ctx, "INSERT INTO amux_worker_outbox(run_id,kind,id,payload) VALUES(?,'bind',?,?)", id, uuid.NewString(), string(bind)); err != nil {
				return err
			}
			r.NativeSessionID = event.SessionID
		}
		if r.NativeSessionID == "" || r.NativeSessionID != event.SessionID {
			return ErrConversationConflict
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO amux_worker_outbox(run_id,kind,id,payload) VALUES(?,'event',?,?)", id, eventID, string(raw))
		return err
	})
	return eventID, err
}
func (s *Store) WorkerOutbox(ctx context.Context, id string, limit int) ([]WorkerOutboxEntry, error) {
	if limit < 1 || limit > 1000 {
		return nil, ErrConversationInput
	}
	rows, err := s.db.QueryContext(ctx, "SELECT sequence,kind,id,payload FROM amux_worker_outbox WHERE run_id=? ORDER BY sequence LIMIT ?", id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []WorkerOutboxEntry{}
	bytes := 0
	for rows.Next() {
		var e WorkerOutboxEntry
		var raw string
		if err = rows.Scan(&e.Sequence, &e.Kind, &e.ID, &raw); err != nil {
			return nil, err
		}
		if bytes+len(raw) > 4<<20 && len(result) > 0 {
			break
		}
		bytes += len(raw)
		e.Payload = json.RawMessage(raw)
		result = append(result, e)
	}
	return result, rows.Err()
}
func (s *Store) AckWorkerOutbox(ctx context.Context, id string, sequence, remoteSequence int64) error {
	return s.mutateWorkerRun(ctx, id, func(tx *sql.Tx, r *WorkerRun) error {
		if sequence <= r.OutboxCursor {
			return nil
		}
		var first int64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MIN(sequence),0) FROM amux_worker_outbox WHERE run_id=?", id).Scan(&first); err != nil {
			return err
		}
		if first != sequence {
			return ErrConversationConflict
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM amux_worker_outbox WHERE run_id=? AND sequence=?", id, sequence); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE amux_worker_runs SET outbox_cursor=?,event_cursor=MAX(event_cursor,?) WHERE id=?", sequence, remoteSequence, id)
		return err
	})
}
func (s *Store) PrepareWorkerInstance(ctx context.Context, id string) (*WorkerRun, error) {
	var prepared *WorkerRun
	err := s.mutateWorkerRun(ctx, id, func(tx *sql.Tx, r *WorkerRun) error {
		var pending bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM amux_worker_outbox WHERE run_id=?)", id).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return ErrConversationConflict
		}
		if r.PendingInstanceID == "" {
			r.PendingInstanceID = uuid.NewString()
			if _, err := tx.ExecContext(ctx, "UPDATE amux_worker_runs SET pending_instance_id=? WHERE id=?", r.PendingInstanceID, id); err != nil {
				return err
			}
		}
		prepared = r
		return nil
	})
	return prepared, err
}
func (s *Store) ActivateWorkerInstance(ctx context.Context, id, instance string) error {
	return s.mutateWorkerRun(ctx, id, func(tx *sql.Tx, r *WorkerRun) error {
		if r.PendingInstanceID != instance || instance == "" {
			return ErrConversationConflict
		}
		r.Owner.InstanceID = instance
		owner, _ := json.Marshal(r.Owner)
		if _, err := tx.ExecContext(ctx, "UPDATE amux_worker_commands SET local_state='interrupted' WHERE run_id=? AND local_state='started'", id); err != nil {
			return err
		}
		// Re-read command revisions after takeover: the server may have changed
		// target instances while the last stream was disconnected.
		_, err := tx.ExecContext(ctx, "UPDATE amux_worker_runs SET owner_json=?,pending_instance_id='',command_cursor=0 WHERE id=?", string(owner), id)
		return err
	})
}
func (s *Store) ObserveWorkerCommand(ctx context.Context, id string, c ConversationCommand) error {
	if c.Revision <= 0 {
		return ErrConversationInput
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.mutateWorkerRun(ctx, id, func(tx *sql.Tx, r *WorkerRun) error {
		if c.ConversationID != r.ConversationID {
			return ErrConversationConflict
		}
		var prior string
		err := tx.QueryRowContext(ctx, "SELECT remote_json FROM amux_worker_commands WHERE run_id=? AND command_id=?", id, c.ID).Scan(&prior)
		if err == nil {
			var previous ConversationCommand
			if err = json.Unmarshal([]byte(prior), &previous); err != nil {
				return err
			}
			if previous.Command != c.Command || previous.RequestID != c.RequestID {
				return ErrConversationConflict
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		state := "new"
		if c.State == "done" || c.State == "rejected" || c.State == "interrupted" {
			state = c.State
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO amux_worker_commands(run_id,command_id,sequence,revision,remote_json,local_state) VALUES(?,?,?,?,?,?) ON CONFLICT(run_id,command_id) DO UPDATE SET remote_json=excluded.remote_json,revision=excluded.revision,local_state=CASE WHEN amux_worker_commands.local_state='new' THEN excluded.local_state ELSE amux_worker_commands.local_state END WHERE excluded.revision>amux_worker_commands.revision`, id, c.ID, c.Sequence, c.Revision, string(raw), state)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE amux_worker_runs SET command_cursor=MAX(command_cursor,?) WHERE id=?", c.Revision, id)
		return err
	})
}
func (s *Store) WorkerCommands(ctx context.Context, id string) ([]WorkerCommand, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT remote_json,local_state FROM amux_worker_commands WHERE run_id=? AND local_state='new' ORDER BY CASE WHEN json_extract(remote_json,'$.command.kind')='prompt' THEN 1 ELSE 0 END,sequence LIMIT 256`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []WorkerCommand{}
	for rows.Next() {
		var c WorkerCommand
		var raw string
		if err = rows.Scan(&raw, &c.State); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &c.Record); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}
func (s *Store) StartWorkerCommand(ctx context.Context, id, commandID string) error {
	return s.mutateWorkerRun(ctx, id, func(tx *sql.Tx, r *WorkerRun) error {
		result, err := tx.ExecContext(ctx, `UPDATE amux_worker_commands SET local_state='started' WHERE run_id=? AND command_id=? AND local_state='new' AND json_extract(remote_json,'$.target_instance_id')=? AND json_extract(remote_json,'$.state') IN ('queued','claimed')`, id, commandID, r.Owner.InstanceID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err == nil && changed != 1 {
			return ErrConversationConflict
		}
		return err
	})
}
func (s *Store) CompleteWorkerCommand(ctx context.Context, id, commandID, state string) error {
	if state != "done" && state != "rejected" && state != "interrupted" {
		return ErrConversationInput
	}
	return s.mutateWorkerRun(ctx, id, func(tx *sql.Tx, r *WorkerRun) error {
		var previous string
		if err := tx.QueryRowContext(ctx, "SELECT local_state FROM amux_worker_commands WHERE run_id=? AND command_id=?", id, commandID).Scan(&previous); err != nil {
			return err
		}
		if previous == state {
			return nil
		}
		if previous != "started" {
			return ErrConversationConflict
		}
		if _, err := tx.ExecContext(ctx, "UPDATE amux_worker_commands SET local_state=? WHERE run_id=? AND command_id=?", state, id, commandID); err != nil {
			return err
		}
		raw, _ := json.Marshal(map[string]string{"command_id": commandID, "state": state})
		_, err := tx.ExecContext(ctx, "INSERT INTO amux_worker_outbox(run_id,kind,id,payload) VALUES(?,'result',?,?)", id, commandID, string(raw))
		return err
	})
}
