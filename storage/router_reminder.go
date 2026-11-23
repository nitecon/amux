package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

const reminderQuietForMS int64 = 10 * 60_000

const (
	reminderOwnerInstructions  = "You still own this task. Continue it, mark it done if complete, or release it if you cannot continue. If waiting for a user decision, comment with your question then run: amux tasks user-review {task_id} \"<specific user action required>\"."
	reminderPreviousExecution  = "This conversation's previous execution still owns this task. Inspect its prior progress. This is an informational continuity reminder, not an ownership handoff. Use the existing recovery or reassignment flow before attempting task mutations or resuming execution."
	reminderUpdateInstructions = "You still own this task and it has had no update for 10 minutes. Post a progress comment saying where you are, mark it done if complete, or release it if you cannot continue. If waiting for a user decision, comment with your question then run: amux tasks user-review {task_id} \"<specific user action required>\"."
)

func migrateRouterReminders(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS reminder_nudges (
		task_id TEXT PRIMARY KEY,
		sent_at INTEGER NOT NULL
	);
	CREATE TABLE IF NOT EXISTS reminder_restarts (
		source TEXT NOT NULL,
		session_id TEXT NOT NULL,
		instance_id TEXT NOT NULL,
		client_run TEXT NOT NULL,
		PRIMARY KEY(source,session_id,instance_id)
	);`); err != nil {
		return fmt.Errorf("create router reminder tables: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(20)"); err != nil {
		return fmt.Errorf("record router reminder migration: %w", err)
	}
	return nil
}

// ReconcileRouterReminders emits periodic task nudges on clock ticks and
// continuity reminders for accepted client lifetime heartbeats.
func (s *Store) ReconcileRouterReminders(ctx context.Context, registry *router.Registry, event router.Event) error {
	if event.Version != 1 || (event.ID != "clock.tick" && event.ID != "client.heartbeat") || !canonicalUUID(event.UID) {
		return fmt.Errorf("reminder reconciliation requires a clock.tick or client.heartbeat v1 event with a canonical UID")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := event.EmittedAt
	stored := false
	if event.ID == "clock.tick" {
		enabled, err := pluginEnabled(ctx, tx, "reminder")
		if err != nil {
			return err
		}
		if enabled {
			stored, err = s.remindQuietTasks(ctx, tx, registry, event)
			if err != nil {
				return err
			}
		}
	} else {
		now = time.Now().UTC().UnixMilli()
		stored, err = s.remindRestartedExecutions(ctx, tx, registry, event, now)
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if stored {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	}
	return nil
}

func (s *Store) remindQuietTasks(ctx context.Context, tx *sql.Tx, registry *router.Registry, event router.Event) (bool, error) {
	agents, _, err := livePresenceSnapshot(ctx, tx, event.EmittedAt)
	if err != nil {
		return false, err
	}
	connected := make(map[[3]string]struct{}, len(agents))
	for _, agent := range agents {
		connected[[3]string{agent.ProjectIdent, agent.Origin.SessionID, agent.Origin.InstanceID}] = struct{}{}
	}
	query := fmt.Sprintf(`SELECT %s FROM tasks t
		LEFT JOIN reminder_nudges n ON n.task_id=t.id
		WHERE t.status='in_progress' AND ? - MAX(t.updated_at,COALESCE(n.sent_at,0)) >= ?`, taskSnapshotSQL("t"))
	tasks, err := readReminderTasks(ctx, tx, query, event.EmittedAt, reminderQuietForMS)
	if err != nil {
		return false, fmt.Errorf("read quiet in-progress tasks: %w", err)
	}
	stored := false
	for _, task := range tasks {
		var fields struct {
			ID          string          `json:"id"`
			Project     string          `json:"project_ident"`
			OwnerOrigin json.RawMessage `json:"owner_origin"`
		}
		if err := json.Unmarshal(task, &fields); err != nil {
			return false, fmt.Errorf("decode quiet task snapshot: %w", err)
		}
		owner, ok := reminderExecution(fields.OwnerOrigin, fields.Project)
		if !ok || fields.ID == "" {
			continue
		}
		if _, live := connected[owner]; !live {
			continue
		}
		instructions := reminderUpdateInstructions
		// The template has one task-specific placeholder; preserving the task
		// snapshot as RawMessage keeps all SQLite integer values exact.
		instructions = replaceReminderTaskID(instructions, fields.ID)
		payload, err := json.Marshal(map[string]any{
			"target": owner[1], "project": fields.Project, "mode": "assigned", "require_idle": true,
			"reminder": true, "heading": "Task update requested", "instructions": instructions,
			"event": map[string]any{"kind": "task_created", "project_ident": fields.Project, "created_at": event.EmittedAt,
				"task": task, "comment": nil, "origin": nil},
		})
		if err != nil {
			return false, err
		}
		result, err := emitRouterNotificationIn(ctx, tx, registry, "reminder", event.UID, payload)
		if err != nil {
			return false, fmt.Errorf("emit periodic reminder for task %s: %w", fields.ID, err)
		}
		stored = stored || result.Stored
		if _, err := tx.ExecContext(ctx, `INSERT INTO reminder_nudges(task_id,sent_at) VALUES(?,?)
			ON CONFLICT(task_id) DO UPDATE SET sent_at=MAX(reminder_nudges.sent_at,excluded.sent_at)`, fields.ID, event.EmittedAt); err != nil {
			return false, fmt.Errorf("record periodic reminder for task %s: %w", fields.ID, err)
		}
	}
	return stored, nil
}

func (s *Store) remindRestartedExecutions(ctx context.Context, tx *sql.Tx, registry *router.Registry, event router.Event, now int64) (bool, error) {
	var payload struct {
		ClientRun string            `json:"client_run"`
		Agents    []json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil || !canonicalUUID(payload.ClientRun) {
		return false, nil
	}
	current, err := routerHeartbeatCurrent(ctx, tx, event, now)
	if err != nil || !current {
		return false, err
	}
	liveAgents, effectiveNow, err := livePresenceSnapshot(ctx, tx, now)
	if err != nil {
		return false, err
	}
	now = effectiveNow
	listed := make(map[[3]string]struct{}, len(payload.Agents))
	for _, raw := range payload.Agents {
		var identity struct {
			Project    string `json:"project"`
			SessionID  string `json:"session_id"`
			InstanceID string `json:"instance_id"`
		}
		if json.Unmarshal(raw, &identity) != nil || identity.Project == "" || identity.SessionID == "" || identity.InstanceID == "" {
			continue
		}
		listed[[3]string{identity.Project, identity.SessionID, identity.InstanceID}] = struct{}{}
	}
	stored := false
	for _, live := range liveAgents {
		key := [3]string{live.ProjectIdent, live.Origin.SessionID, live.Origin.InstanceID}
		if live.Source != event.Source {
			continue
		}
		if _, ok := listed[key]; !ok {
			continue
		}
		first, err := tx.ExecContext(ctx, `INSERT INTO reminder_restarts(source,session_id,instance_id,client_run)
			VALUES(?,?,?,?) ON CONFLICT(source,session_id,instance_id) DO UPDATE SET client_run=excluded.client_run
			WHERE client_run!=excluded.client_run`, event.Source, key[1], key[2], payload.ClientRun)
		if err != nil {
			return false, fmt.Errorf("record reminder client lifetime: %w", err)
		}
		changed, err := first.RowsAffected()
		if err != nil {
			return false, err
		}
		if changed == 0 {
			continue
		}
		query := fmt.Sprintf(`SELECT %s FROM tasks t WHERE t.project_ident=? AND t.status='in_progress'
			AND json_extract(t.owner_origin_json,'$.session_id')=?`, taskSnapshotSQL("t"))
		tasks, err := readReminderTasks(ctx, tx, query, live.ProjectIdent, live.Origin.SessionID)
		if err != nil {
			return false, fmt.Errorf("read tasks for restarted execution: %w", err)
		}
		for _, task := range tasks {
			var fields struct {
				ID          string          `json:"id"`
				OwnerOrigin json.RawMessage `json:"owner_origin"`
			}
			if err := json.Unmarshal(task, &fields); err != nil {
				return false, fmt.Errorf("decode task for restarted execution: %w", err)
			}
			owner, ok := reminderExecution(fields.OwnerOrigin, live.ProjectIdent)
			if !ok || owner[1] != live.Origin.SessionID || fields.ID == "" {
				continue
			}
			instructions := reminderPreviousExecution
			if owner[2] == live.Origin.InstanceID {
				instructions = replaceReminderTaskID(reminderOwnerInstructions, fields.ID)
			}
			payload, err := json.Marshal(map[string]any{
				"target": live.Origin.SessionID, "project": live.ProjectIdent, "mode": "recipient", "require_idle": true,
				"receiver": map[string]string{"session_id": live.Origin.SessionID, "instance_id": live.Origin.InstanceID, "project": live.ProjectIdent},
				"heading":  "In-progress task reminder", "instructions": instructions,
				"event": map[string]any{"kind": "task_created", "project_ident": live.ProjectIdent, "created_at": now,
					"sender_name": "amux", "task": task, "comment": nil, "origin": nil},
			})
			if err != nil {
				return false, err
			}
			result, err := emitRouterNotificationIn(ctx, tx, registry, "reminder", event.UID, payload)
			if err != nil {
				return false, fmt.Errorf("emit startup reminder for task %s: %w", fields.ID, err)
			}
			stored = stored || result.Stored
			if _, err := tx.ExecContext(ctx, `INSERT INTO reminder_nudges(task_id,sent_at) VALUES(?,?)
				ON CONFLICT(task_id) DO UPDATE SET sent_at=MAX(reminder_nudges.sent_at,excluded.sent_at)`, fields.ID, now); err != nil {
				return false, fmt.Errorf("record startup reminder for task %s: %w", fields.ID, err)
			}
		}
	}
	return stored, nil
}

func readReminderTasks(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]json.RawMessage, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := make([]json.RawMessage, 0)
	for rows.Next() {
		var task []byte
		if err := rows.Scan(&task); err != nil {
			return nil, err
		}
		tasks = append(tasks, append(json.RawMessage(nil), task...))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tasks, nil
}

func reminderExecution(origin json.RawMessage, project string) ([3]string, bool) {
	var identity struct {
		SessionID  string `json:"session_id"`
		InstanceID string `json:"instance_id"`
	}
	if project == "" || json.Unmarshal(origin, &identity) != nil || identity.SessionID == "" || identity.InstanceID == "" {
		return [3]string{}, false
	}
	return [3]string{project, identity.SessionID, identity.InstanceID}, true
}

func replaceReminderTaskID(template, taskID string) string {
	const placeholder = "{task_id}"
	return strings.ReplaceAll(template, placeholder, taskID)
}
