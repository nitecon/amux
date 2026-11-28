package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
)

var routerTaskEventKinds = [...]string{"created", "updated", "commented", "assigned", "started", "user_review", "completed", "cancelled", "unclaimed"}

// RouterTaskEventDefinitions declares the task events emitted from the durable
// task outbox. Payloads match the Rust task plugin contract.
func RouterTaskEventDefinitions() []fism.EventDefinition {
	definitions := make([]fism.EventDefinition, 0, len(routerTaskEventKinds))
	for _, kind := range routerTaskEventKinds {
		definitions = append(definitions, fism.EventDefinition{
			ID:         "task." + kind,
			Version:    1,
			RemoteEmit: false,
			Validate:   validateRouterTaskPayload,
		})
	}
	return definitions
}

func validateRouterTaskPayload(payload json.RawMessage) error {
	var object map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&object); err != nil || object == nil {
		return fmt.Errorf("payload needs a project and a task object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("payload needs one JSON object")
	}
	var project *string
	if raw, ok := object["project"]; !ok || json.Unmarshal(raw, &project) != nil || project == nil {
		return fmt.Errorf("payload needs a project and a task object")
	}
	var task map[string]json.RawMessage
	if raw, ok := object["task"]; !ok || json.Unmarshal(raw, &task) != nil || task == nil {
		return fmt.Errorf("payload needs a project and a task object")
	}
	return nil
}

func migrateRouterTaskOutbox(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS router_task_outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,
		project TEXT NOT NULL,
		task_json TEXT NOT NULL,
		comment_json TEXT,
		actor_json TEXT,
		created_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create router_task_outbox: %w", err)
	}

	actor := "(SELECT origin_json FROM task_mutation_context WHERE id=1)"
	now := "CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)"
	task := taskSnapshotSQL("NEW")
	insert := func(kind, taskJSON, commentJSON string) string {
		return fmt.Sprintf(`INSERT INTO router_task_outbox(kind,project,task_json,comment_json,actor_json,created_at)
		VALUES('%s',NEW.project_ident,%s,%s,%s,%s);`, kind, taskJSON, commentJSON, actor, now)
	}
	statusTrigger := func(kind, status string) string {
		return fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS router_task_%s AFTER UPDATE OF status ON tasks
		WHEN NEW.status='%s' AND OLD.status!='%s' BEGIN %s END;`, kind, status, status, insert(kind, task, "NULL"))
	}
	statements := []string{
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS router_task_created AFTER INSERT ON tasks BEGIN %s END;`, insert("created", task, "NULL")),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS router_task_updated AFTER UPDATE ON tasks BEGIN %s END;`, insert("updated", task, "NULL")),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS router_task_commented AFTER INSERT ON task_comments BEGIN
			INSERT INTO router_task_outbox(kind,project,task_json,comment_json,actor_json,created_at)
			SELECT 'commented',t.project_ident,%s,%s,%s,%s FROM tasks t WHERE t.id=NEW.task_id;
		END;`, taskSnapshotSQL("t"), commentSnapshotSQL("NEW"), actor, now),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS router_task_assigned AFTER UPDATE OF owner_agent_id,owner_origin_json ON tasks
		WHEN (NEW.owner_agent_id IS NOT NULL OR NEW.owner_origin_json IS NOT NULL)
		 AND (NEW.owner_agent_id IS NOT OLD.owner_agent_id OR NEW.owner_origin_json IS NOT OLD.owner_origin_json)
		BEGIN %s END;`, insert("assigned", task, "NULL")),
		statusTrigger("started", "in_progress"),
		statusTrigger("user_review", "user_review"),
		statusTrigger("completed", "done"),
		statusTrigger("cancelled", "cancelled"),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS router_task_unclaimed AFTER UPDATE OF status,owner_agent_id,owner_origin_json ON tasks
		WHEN NEW.status='todo' AND NEW.owner_agent_id IS NULL AND NEW.owner_origin_json IS NULL
		 AND (OLD.owner_agent_id IS NOT NULL OR OLD.owner_origin_json IS NOT NULL OR OLD.status='in_progress')
		BEGIN %s END;`, insert("unclaimed", task, "NULL")),
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create router task outbox trigger: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(16)"); err != nil {
		return fmt.Errorf("record router task outbox migration: %w", err)
	}
	return nil
}

type routerTaskOutboxRow struct {
	id             int64
	kind           string
	project        string
	task           []byte
	comment        []byte
	commentPresent bool
	actor          []byte
	actorPresent   bool
}

// DrainRouterTaskOutbox emits and acknowledges one bounded outbox batch in a
// single SQLite transaction. Failed rows leave the entire batch available.
func (s *Store) DrainRouterTaskOutbox(ctx context.Context, registry *fism.Registry, limit int) (int, error) {
	if limit < 1 || limit > 200 {
		return 0, fmt.Errorf("router task outbox limit must be between 1 and 200")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	batch, err := readTaskOutbox(ctx, tx, limit)
	if err != nil {
		return 0, err
	}
	stored := false
	for _, row := range batch {
		request, err := taskChangeRequest(row)
		if err != nil {
			return 0, err
		}
		result, err := emitRouterEventIn(ctx, tx, registry, request)
		if err != nil {
			return 0, fmt.Errorf("emit router task outbox row %d: %w", row.id, err)
		}
		stored = stored || result.Stored
	}
	if len(batch) > 0 {
		result, err := tx.ExecContext(ctx, "DELETE FROM router_task_outbox WHERE id<=?", batch[len(batch)-1].id)
		if err != nil {
			return 0, fmt.Errorf("delete router task outbox batch: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count deleted router task outbox rows: %w", err)
		}
		if deleted != int64(len(batch)) {
			return 0, fmt.Errorf("deleted %d router task outbox rows; expected %d", deleted, len(batch))
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if stored {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	}
	return len(batch), nil
}

func readTaskOutbox(ctx context.Context, q queryer, limit int) ([]routerTaskOutboxRow, error) {
	if limit < 1 || limit > 200 {
		return nil, fmt.Errorf("router task outbox limit must be between 1 and 200")
	}
	rows, err := q.QueryContext(ctx, `SELECT id,kind,project,task_json,comment_json,actor_json
		FROM router_task_outbox ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read router task outbox: %w", err)
	}
	defer rows.Close()
	batch := make([]routerTaskOutboxRow, 0, limit)
	for rows.Next() {
		var row routerTaskOutboxRow
		var comment, actor sql.NullString
		if err := rows.Scan(&row.id, &row.kind, &row.project, &row.task, &comment, &actor); err != nil {
			return nil, fmt.Errorf("scan router task outbox row: %w", err)
		}
		row.commentPresent, row.actorPresent = comment.Valid, actor.Valid
		row.comment, row.actor = []byte(comment.String), []byte(actor.String)
		batch = append(batch, row)
	}
	return batch, rows.Err()
}

func taskChangeRequest(row routerTaskOutboxRow) (fism.EmitRequest, error) {
	id, ok := routerTaskEventID(row.kind)
	if !ok {
		return fism.EmitRequest{}, fmt.Errorf("unknown task change kind %q", row.kind)
	}
	payload, err := marshalRouterTaskPayload(row)
	if err != nil {
		return fism.EmitRequest{}, fmt.Errorf("build task change %d: %w", row.id, err)
	}
	return fism.EmitRequest{ID: id, Version: 1, Payload: payload, Source: "tasks"}, nil
}

func routerTaskEventID(kind string) (string, bool) {
	for _, known := range routerTaskEventKinds {
		if kind == known {
			return "task." + known, true
		}
	}
	return "", false
}

func marshalRouterTaskPayload(row routerTaskOutboxRow) (json.RawMessage, error) {
	if !json.Valid(row.task) {
		return nil, errors.New("task snapshot is malformed JSON")
	}
	comment := json.RawMessage("null")
	if row.commentPresent {
		if !json.Valid(row.comment) {
			return nil, errors.New("comment snapshot is malformed JSON")
		}
		comment = json.RawMessage(row.comment)
	}
	actor := json.RawMessage("null")
	if row.actorPresent {
		if !json.Valid(row.actor) {
			return nil, errors.New("actor snapshot is malformed JSON")
		}
		actor = json.RawMessage(row.actor)
	}
	payload, err := json.Marshal(struct {
		Project string          `json:"project"`
		Task    json.RawMessage `json:"task"`
		Comment json.RawMessage `json:"comment"`
		Actor   json.RawMessage `json:"actor"`
	}{Project: row.project, Task: json.RawMessage(row.task), Comment: comment, Actor: actor})
	return payload, err
}
