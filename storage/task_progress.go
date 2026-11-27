package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

const taskQuietMS int64 = 5 * 60_000

// ReconcileRouterProgress tracks exact-execution activity, rather than a busy
// flag or client heartbeat. Pings and their durable counters commit together.
func (s *Store) ReconcileRouterProgress(ctx context.Context, registry *router.Registry, event router.Event) error {
	if event.ID != "clock.tick" || event.Version != 1 || !canonicalUUID(event.UID) {
		return fmt.Errorf("task progress requires a clock.tick v1 event")
	}
	err := s.taskMutation(ctx, nil, func(tx *sql.Tx, _ *AgentOrigin) error {
		enabled, err := pluginEnabled(ctx, tx, "reclaim")
		if err != nil || !enabled {
			return err
		}
		agents, now, err := livePresenceSnapshot(ctx, tx, event.EmittedAt)
		if err != nil {
			return err
		}
		activity := make(map[[3]string]int64)
		for _, agent := range agents {
			if agent.LastActivityAt != nil {
				activity[[3]string{agent.ProjectIdent, agent.Origin.SessionID, agent.Origin.InstanceID}] = min(now, *agent.LastActivityAt)
			}
		}
		rows, err := tx.QueryContext(ctx, "SELECT "+taskSelect+" FROM tasks t WHERE t.status IN ('in_progress','stalled') OR (t.status='todo' AND (t.owner_origin_json IS NOT NULL OR t.owner_agent_id IS NOT NULL))")
		if err != nil {
			return err
		}
		var tasks []*Task
		for rows.Next() {
			task, err := scanTask(rows)
			if err != nil {
				rows.Close()
				return err
			}
			tasks = append(tasks, task)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, task := range tasks {
			owner, originErr := NormalizeOrigin(task.OwnerOrigin)
			nativeOwner := originErr == nil && owner != nil
			if !nativeOwner {
				owner = task.OwnerOrigin
				if owner == nil {
					owner = &AgentOrigin{}
					if task.OwnerAgentID != nil {
						owner.SessionID = *task.OwnerAgentID
					}
				}
			}
			if err := ensureTaskNotManagedDispatch(ctx, tx, task.ID); err != nil {
				if errors.Is(err, ErrTaskConflict) {
					continue
				}
				return err
			}
			ownerJSON, err := json.Marshal(owner)
			if err != nil {
				return err
			}
			last := task.UpdatedAt
			if task.Status != "todo" {
				last = max(last, activity[[3]string{task.ProjectIdent, owner.SessionID, owner.InstanceID}])
			}
			// Server receipt time is authoritative. Only events from this exact
			// execution count; old conversations and replayed events cannot renew it.
			var providerAt int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(e.created_at),0)
			 FROM amux_conversation_events e JOIN amux_conversations c ON c.id=e.conversation_id
			 WHERE c.project_ident=? AND c.task_id=? AND json_extract(c.owner_json,'$.session_id')=?
			 AND e.instance_id=? AND json_extract(e.event_json,'$.kind') NOT IN ('user_message','session_ready','closed')`,
				task.ProjectIdent, task.ID, owner.SessionID, owner.InstanceID).Scan(&providerAt); err != nil {
				return err
			}
			if task.Status != "todo" {
				last = max(last, providerAt)
			}
			last = min(now, last)
			// An open question means the owner is waiting on the user, not
			// stalled. Keep the window fresh so an answer starts a new one.
			if task.WaitingOnUser {
				last = now
			}
			var previousOwner string
			var previousActivity, pingAt int64
			var pings int
			err = tx.QueryRowContext(ctx, "SELECT owner_json,activity_at,ping_count,ping_at FROM task_progress WHERE task_id=?", task.ID).Scan(&previousOwner, &previousActivity, &pings, &pingAt)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if previousOwner != string(ownerJSON) || last > previousActivity {
				pings, pingAt = 0, 0
			}
			if now-last < taskQuietMS {
				if task.Status == "stalled" {
					if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status='in_progress' WHERE id=?", task.ID); err != nil {
						return err
					}
				}
			} else if pingAt == 0 || now-pingAt >= taskQuietMS {
				if pings >= 2 {
					if err := requeueStalledTask(ctx, tx, task, *owner, now); err != nil {
						return err
					}
					continue
				}
				if task.Status != "todo" {
					if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status='stalled' WHERE id=?", task.ID); err != nil {
						return err
					}
				}
				current, err := readTask(ctx, tx, task.ProjectIdent, task.ID)
				if err != nil {
					return err
				}
				heading := "Stalled task: progress requested"
				instructions := fmt.Sprintf("No agent message or tool activity has been observed for five minutes. Recovery ping %d of 2. Explain why task %s is not completing and continue the work. If you need a user decision, ask with task_ask. Without new activity this task will be reassigned after two pings.", pings+1, task.ID)
				if task.Status == "todo" {
					heading = "Queued task reservation"
					instructions = fmt.Sprintf("Task %s is reserved to you but has not started. Reservation reminder %d of 2. Claim it when available, or release the reservation if you are busy with other work. An unstarted reservation is released after two reminders so an available agent can take it.", task.ID, pings+1)
				}
				payload, err := json.Marshal(map[string]any{
					"target": owner.SessionID, "project": task.ProjectIdent, "mode": "assigned", "require_idle": false,
					"reminder": true, "heading": heading,
					"instructions": instructions,
					"event":        map[string]any{"kind": "task_created", "project_ident": task.ProjectIdent, "created_at": now, "task": current, "comment": nil, "origin": nil},
				})
				if err != nil {
					return err
				}
				if nativeOwner {
					if _, err := emitRouterNotificationIn(ctx, tx, registry, "reclaim", event.UID, payload); err != nil {
						return err
					}
				} else {
					if _, err := insertTaskComment(ctx, tx, task.ID, "amux", "system", fmt.Sprintf("Stalled task recovery ping %d of 2: no recent activity. The prior owner has no routable execution identity; restore execution or this task will be requeued for another agent.", pings+1), nil, nil); err != nil {
						return err
					}
					// The system's own recovery message is not agent progress.
					if _, err := tx.ExecContext(ctx, "UPDATE tasks SET updated_at=? WHERE id=?", task.UpdatedAt, task.ID); err != nil {
						return err
					}
				}
				pings++
				pingAt = now
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO task_progress(task_id,owner_json,activity_at,ping_count,ping_at) VALUES(?,?,?,?,?)
			 ON CONFLICT(task_id) DO UPDATE SET owner_json=excluded.owner_json,activity_at=excluded.activity_at,ping_count=excluded.ping_count,ping_at=excluded.ping_at`, task.ID, string(ownerJSON), last, pings, pingAt); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM task_progress WHERE task_id IN (SELECT id FROM tasks WHERE status NOT IN ('todo','in_progress','stalled') OR (status='todo' AND owner_origin_json IS NULL AND owner_agent_id IS NULL))")
		return err
	})
	if err == nil {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	}
	return err
}

func requeueStalledTask(ctx context.Context, tx *sql.Tx, task *Task, owner AgentOrigin, now int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO task_revoked_conversations(conversation_id,revoked_at)
	 SELECT id,? FROM amux_conversations WHERE task_id=? AND json_extract(owner_json,'$.session_id')=?
	 AND json_extract(owner_json,'$.instance_id')=?`, now, task.ID, owner.SessionID, owner.InstanceID); err != nil {
		return err
	}
	// Changing lifecycle invalidates assignment/adoption mappings. Native pools
	// observe the revoked claim and stop/join the old run before freeing its slot.
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='todo',owner_agent_id=NULL,owner_origin_json=NULL,
	 routing_json='{"mode":"auto"}',started_at=NULL,updated_at=? WHERE id=?`, now, task.ID); err != nil {
		return err
	}
	reason := "Task stalled without new agent activity after two recovery pings. Requeued for another agent; inspect prior progress before continuing."
	if task.Status == "todo" {
		reason = "Unstarted task reservation released after two reminders. Any eligible idle agent, including the previous reserver, can take this queued task."
	}
	if _, err := insertTaskComment(ctx, tx, task.ID, "amux", "system", reason, nil, nil); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE tasks SET updated_at=MAX(updated_at,?) WHERE id=?", now, task.ID); err != nil {
		return err
	}
	// Insert after the routing edit/comment, whose triggers reset release guards.
	if IsActiveTaskStatus(task.Status) && owner.SessionID != "" && owner.InstanceID != "" {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO assignment_releases(task_id,session_id,instance_id) VALUES(?,?,?)", task.ID, owner.SessionID, owner.InstanceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO task_recovery_sessions(task_id,session_id,instance_id) VALUES(?,?,?)", task.ID, owner.SessionID, owner.InstanceID); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM task_progress WHERE task_id=?", task.ID)
	return err
}

func conversationRevoked(ctx context.Context, q queryer, id string) (bool, error) {
	var revoked bool
	err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM task_revoked_conversations WHERE conversation_id=?)", id).Scan(&revoked)
	return revoked, err
}

// Real owner messages resume work immediately, including submission result
// comments; a stalled status must not break the submission's active-state CAS.
func resumeStalledTask(ctx context.Context, q queryer, id string, owner *AgentOrigin) (bool, error) {
	if owner == nil || id == "" {
		return false, nil
	}
	result, err := q.ExecContext(ctx, `UPDATE tasks SET status='in_progress' WHERE id=? AND status='stalled'
 AND json_extract(owner_origin_json,'$.session_id')=? AND json_extract(owner_origin_json,'$.instance_id')=?
 AND json_extract(owner_origin_json,'$.provider')=? AND json_extract(owner_origin_json,'$.os')=?`, id, owner.SessionID, owner.InstanceID, owner.Provider, owner.OS)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}
