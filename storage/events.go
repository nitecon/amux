package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/nitecon/amux/eventbus"
)

// AgentOrigin fences an execution by stable session and ephemeral instance.
type AgentOrigin struct {
	SessionID  string `json:"session_id"`
	InstanceID string `json:"instance_id"`
	Provider   string `json:"provider"`
	OS         string `json:"os"`
}

type TaskEvent struct {
	ID              int64           `json:"id"`
	Kind            string          `json:"kind"`
	ProjectIdent    string          `json:"project_ident"`
	CanonicalRemote *string         `json:"canonical_remote"`
	Origin          *AgentOrigin    `json:"origin"`
	Task            json.RawMessage `json:"task"`
	Comment         json.RawMessage `json:"comment"`
	Delegation      json.RawMessage `json:"delegation"`
	CreatedAt       int64           `json:"created_at"`
	Truncated       bool            `json:"truncated"`
	Deliveries      []EventDelivery `json:"deliveries"`
}

type EventDelivery struct {
	ConsumerID  string  `json:"consumer_id"`
	EventID     int64   `json:"event_id"`
	Status      string  `json:"status"`
	WorkspaceID *string `json:"workspace_id"`
	SurfaceID   *string `json:"surface_id"`
	Message     string  `json:"message"`
	Summary     string  `json:"summary"`
	UpdatedAt   int64   `json:"updated_at"`
}

type EventQuery struct {
	After   *int64
	Project *string
	TaskID  *string
}
type EventSubscription struct {
	Cursor        int64
	LatestEventID int64
}
type EventReceipt struct {
	EventID                int64
	Status                 string
	WorkspaceID, SurfaceID *string
	Message, Summary       string
}

func migrateTaskEvents(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS task_events(id INTEGER PRIMARY KEY AUTOINCREMENT,kind TEXT NOT NULL,project_ident TEXT NOT NULL,canonical_remote TEXT,task_json TEXT NOT NULL,comment_json TEXT,created_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS task_events_project ON task_events(project_ident,id);
 CREATE TABLE IF NOT EXISTS task_stream_consumers(consumer_id TEXT PRIMARY KEY,cursor INTEGER NOT NULL,connection_id TEXT,connected INTEGER NOT NULL DEFAULT 0,last_seen_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS task_event_deliveries(consumer_id TEXT NOT NULL REFERENCES task_stream_consumers(consumer_id),event_id INTEGER NOT NULL REFERENCES task_events(id),status TEXT NOT NULL,workspace_id TEXT,surface_id TEXT,message TEXT NOT NULL DEFAULT '',summary TEXT NOT NULL DEFAULT '',updated_at INTEGER NOT NULL,PRIMARY KEY(consumer_id,event_id));`)
	if err != nil {
		return err
	}
	var present bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info('task_events') WHERE name='origin_json')").Scan(&present); err != nil {
		return err
	}
	if !present {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE task_events ADD COLUMN origin_json TEXT"); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(2)")
	return err
}

// TaskEventChanges is a coalesced wake-up, not an event queue. Consumers replay
// committed events from SQLite. Obtain it before querying to avoid missing a wake.
func (s *Store) TaskEventChanges() <-chan struct{} {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	return s.eventChanges
}
func (s *Store) wakeTaskEvents() {
	s.eventMu.Lock()
	close(s.eventChanges)
	s.eventChanges = make(chan struct{})
	s.eventMu.Unlock()
	_ = s.bus.Publish(eventbus.Event{Topic: eventbus.TaskChanged})
}

// AppendTaskEvent persists a snapshot before waking subscribers. Canonical task
// mutations use SQLite triggers in their own transaction, then wake after commit.
func (s *Store) AppendTaskEvent(ctx context.Context, event TaskEvent) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	id, err := insertTaskEvent(ctx, tx, event)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.wakeTaskEvents()
	return id, nil
}
func insertTaskEvent(ctx context.Context, q queryer, event TaskEvent) (int64, error) {
	if !json.Valid(event.Task) || (event.Comment != nil && !json.Valid(event.Comment)) {
		return 0, fmt.Errorf("invalid event snapshot")
	}
	var origin any
	if event.Origin != nil {
		b, err := json.Marshal(event.Origin)
		if err != nil {
			return 0, err
		}
		origin = string(b)
	}
	var comment any
	if event.Comment != nil {
		comment = string(event.Comment)
	}
	if event.CreatedAt == 0 {
		event.CreatedAt = time.Now().UnixMilli()
	}
	result, err := q.ExecContext(ctx, "INSERT INTO task_events(kind,project_ident,canonical_remote,task_json,comment_json,created_at,origin_json) VALUES(?,?,?,?,?,?,?)", event.Kind, event.ProjectIdent, event.CanonicalRemote, string(event.Task), comment, event.CreatedAt, origin)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) TaskEvents(ctx context.Context, query EventQuery) ([]TaskEvent, error) {
	return readTaskEvents(ctx, s.db, query, 100)
}
func readTaskEvents(ctx context.Context, q queryer, query EventQuery, limit int) ([]TaskEvent, error) {
	project := query.Project
	if project != nil && *project == "" {
		project = nil
	}
	rows, err := q.QueryContext(ctx, `SELECT id,kind,project_ident,canonical_remote,task_json,comment_json,created_at,origin_json FROM task_events
 WHERE (?1 IS NULL OR id>?1) AND (?2 IS NULL OR project_ident=?2) AND (?3 IS NULL OR json_extract(task_json,'$.id')=?3)
 ORDER BY CASE WHEN ?1 IS NOT NULL THEN id END ASC,id DESC LIMIT ?4`, query.After, project, query.TaskID, limit)
	if err != nil {
		return nil, err
	}
	result := []TaskEvent{}
	for rows.Next() {
		e := TaskEvent{Deliveries: []EventDelivery{}}
		var task string
		var comment, origin *string
		if err := rows.Scan(&e.ID, &e.Kind, &e.ProjectIdent, &e.CanonicalRemote, &task, &comment, &e.CreatedAt, &origin); err != nil {
			rows.Close()
			return nil, err
		}
		if !json.Valid([]byte(task)) {
			rows.Close()
			return nil, fmt.Errorf("invalid persisted task event")
		}
		e.Task = json.RawMessage(task)
		if comment != nil {
			if !json.Valid([]byte(*comment)) {
				rows.Close()
				return nil, fmt.Errorf("invalid persisted comment")
			}
			e.Comment = json.RawMessage(*comment)
		}
		if origin != nil {
			if err := json.Unmarshal([]byte(*origin), &e.Origin); err != nil {
				rows.Close()
				return nil, err
			}
		}
		result = append(result, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range result {
		// Rust event reads enrich the immutable event snapshot with the linked
		// delegation record. Readiness remains in Task and is never recomputed here.
		var taskFields struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(result[i].Task, &taskFields); err != nil {
			return nil, err
		}
		link, err := delegationBySource(ctx, q, result[i].ProjectIdent, taskFields.ID)
		if err != nil {
			return nil, err
		}
		if link == nil {
			link, err = delegationByTarget(ctx, q, result[i].ProjectIdent, taskFields.ID)
			if err != nil {
				return nil, err
			}
		}
		if link != nil {
			result[i].Delegation, err = json.Marshal(link)
			if err != nil {
				return nil, err
			}
		}
		receipts, err := q.QueryContext(ctx, "SELECT consumer_id,event_id,status,workspace_id,surface_id,message,summary,updated_at FROM task_event_deliveries WHERE event_id=? ORDER BY updated_at DESC LIMIT 100", result[i].ID)
		if err != nil {
			return nil, err
		}
		for receipts.Next() {
			var d EventDelivery
			if err := receipts.Scan(&d.ConsumerID, &d.EventID, &d.Status, &d.WorkspaceID, &d.SurfaceID, &d.Message, &d.Summary, &d.UpdatedAt); err != nil {
				receipts.Close()
				return nil, err
			}
			result[i].Deliveries = append(result[i].Deliveries, d)
		}
		if err := receipts.Err(); err != nil {
			receipts.Close()
			return nil, err
		}
		if err := receipts.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func validStreamIdentity(s string) bool {
	return s != "" && len(s) <= 128 && strings.TrimSpace(s) == s && strings.IndexFunc(s, unicode.IsControl) < 0
}
func eventTail(ctx context.Context, q queryer) (int64, error) {
	var tail int64
	// Deleting a project can remove the newest event while consumers still
	// hold its cursor. AUTOINCREMENT retains the highest allocated ID.
	err := q.QueryRowContext(ctx, "SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='task_events'),0)").Scan(&tail)
	return tail, err
}

func (s *Store) SubscribeTaskEvents(ctx context.Context, connection, consumer string, after *int64) (EventSubscription, error) {
	if !validStreamIdentity(connection) || !validStreamIdentity(consumer) {
		return EventSubscription{}, fmt.Errorf("invalid identity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EventSubscription{}, err
	}
	defer tx.Rollback()
	end, err := eventTail(ctx, tx)
	if err != nil {
		return EventSubscription{}, err
	}
	var stored, lastSeen int64
	var connected bool
	err = tx.QueryRowContext(ctx, "SELECT cursor,connected,last_seen_at FROM task_stream_consumers WHERE consumer_id=?", consumer).Scan(&stored, &connected, &lastSeen)
	exists := !errors.Is(err, sql.ErrNoRows)
	if err != nil && exists {
		return EventSubscription{}, err
	}
	now := time.Now().UnixMilli()
	if exists && connected && now-lastSeen < 60000 {
		return EventSubscription{}, fmt.Errorf("consumer already connected")
	}
	if !exists {
		stored = end
	}
	cursor := stored
	if after != nil {
		cursor = *after
	}
	if cursor < 0 || cursor > end || (exists && cursor > stored) {
		return EventSubscription{}, fmt.Errorf("invalid replay cursor")
	}
	persisted := stored
	if !exists {
		persisted = cursor
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_stream_consumers(consumer_id,cursor,connection_id,connected,last_seen_at) VALUES(?,?,?,1,?) ON CONFLICT(consumer_id) DO UPDATE SET connection_id=excluded.connection_id,connected=1,last_seen_at=excluded.last_seen_at`, consumer, persisted, connection, now); err != nil {
		return EventSubscription{}, err
	}
	if err := tx.Commit(); err != nil {
		return EventSubscription{}, err
	}
	return EventSubscription{Cursor: cursor, LatestEventID: end}, nil
}

func ownsTaskConsumer(ctx context.Context, q queryer, connection, consumer string) error {
	var owns bool
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM task_stream_consumers WHERE consumer_id=? AND connection_id=? AND connected=1)", consumer, connection).Scan(&owns); err != nil {
		return err
	}
	if !owns {
		return fmt.Errorf("connection no longer owns consumer")
	}
	return nil
}
func (s *Store) TouchTaskConsumer(ctx context.Context, connection, consumer string) (bool, error) {
	result, err := s.db.ExecContext(ctx, "UPDATE task_stream_consumers SET last_seen_at=? WHERE consumer_id=? AND connection_id=? AND connected=1", time.Now().UnixMilli(), consumer, connection)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, fmt.Errorf("connection no longer owns consumer")
	}
	return true, nil
}
func (s *Store) DisconnectTaskConsumer(ctx context.Context, connection string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE task_stream_consumers SET connected=0,connection_id=NULL WHERE connection_id=?", connection)
	return err
}

func (s *Store) OfferTaskEvent(ctx context.Context, connection, consumer string, cursor int64) (*TaskEvent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := ownsTaskConsumer(ctx, tx, connection, consumer); err != nil {
		return nil, err
	}
	events, err := readTaskEvents(ctx, tx, EventQuery{After: &cursor}, 1)
	if err != nil || len(events) == 0 {
		return nil, err
	}
	event := events[0]
	event.Deliveries = []EventDelivery{}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO task_event_deliveries(consumer_id,event_id,status,updated_at) VALUES(?,?,'offered',?)", consumer, event.ID, time.Now().UnixMilli()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &event, nil
}

func terminalReceipt(status string) bool {
	return status == "injected" || status == "skipped" || status == "failed" || status == "uncertain"
}
func (s *Store) AcknowledgeTaskEvent(ctx context.Context, connection, consumer string, receipt EventReceipt) (string, error) {
	if receipt.Status != "received" && receipt.Status != "queued" && !terminalReceipt(receipt.Status) {
		return "", fmt.Errorf("invalid delivery status")
	}
	for _, value := range []*string{receipt.WorkspaceID, receipt.SurfaceID} {
		if value != nil && !validStreamIdentity(*value) {
			return "", fmt.Errorf("invalid identity")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := ownsTaskConsumer(ctx, tx, connection, consumer); err != nil {
		return "", err
	}
	var previous string
	if err := tx.QueryRowContext(ctx, "SELECT status FROM task_event_deliveries WHERE consumer_id=? AND event_id=?", consumer, receipt.EventID).Scan(&previous); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("event was not offered to consumer")
		}
		return "", err
	}
	recorded := receipt.Status
	if terminalReceipt(previous) || (previous == "queued" && receipt.Status == "received") {
		recorded = previous
	}
	if recorded == receipt.Status && !terminalReceipt(previous) {
		if _, err := tx.ExecContext(ctx, "UPDATE task_event_deliveries SET status=?,workspace_id=COALESCE(?,workspace_id),surface_id=COALESCE(?,surface_id),message=?,summary=?,updated_at=? WHERE consumer_id=? AND event_id=?", receipt.Status, receipt.WorkspaceID, receipt.SurfaceID, receipt.Message, receipt.Summary, time.Now().UnixMilli(), consumer, receipt.EventID); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE task_stream_consumers SET cursor=MAX(cursor,?),last_seen_at=? WHERE consumer_id=? AND connection_id=?", receipt.EventID, time.Now().UnixMilli(), consumer, connection); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return recorded, nil
}

type TaskConsumerStatus struct {
	ConsumerID string `json:"consumer_id"`
	Cursor     int64  `json:"cursor"`
	Connected  bool   `json:"connected"`
	LastSeenAt int64  `json:"last_seen_at"`
}

func (s *Store) TaskConsumerStatuses(ctx context.Context) ([]TaskConsumerStatus, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT consumer_id,cursor,connected,last_seen_at FROM task_stream_consumers ORDER BY last_seen_at DESC LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	statuses := make([]TaskConsumerStatus, 0)
	for rows.Next() {
		var status TaskConsumerStatus
		if err := rows.Scan(&status.ConsumerID, &status.Cursor, &status.Connected, &status.LastSeenAt); err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}
	return statuses, rows.Err()
}

// RecoverTaskConsumers retires transport leases without changing durable replay
// cursors. Only the runtime holding the exclusive server profile lock may call it.
func (s *Store) RecoverTaskConsumers(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "UPDATE task_stream_consumers SET connected=0,connection_id=NULL")
	return err
}
