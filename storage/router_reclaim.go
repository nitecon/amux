package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/nitecon/amux/router"
)

const (
	reclaimAfterMS = int64(15 * 60_000)
	reclaimComment = "Task became unclaimed after 15 minutes of inactivity. Next agent please verify prior progress before continuing."
)

type reclaimCandidate struct {
	id        string
	ownerJSON sql.NullString
}

// ReconcileRouterReclaim applies the legacy 15-minute reclaim policy on a
// shared clock tick. Native work is retained whenever a valid native owner or
// conversation/execution record exists; old profiles without that evidence
// still receive the Rust-compatible offline reclaim behavior.
func (s *Store) ReconcileRouterReclaim(ctx context.Context, event router.Event) (int, error) {
	if event.ID != "clock.tick" || event.Version != 1 || !canonicalUUID(event.UID) {
		return 0, fmt.Errorf("reclaim reconciliation requires a clock.tick v1 event with a canonical UID")
	}
	count := 0
	err := s.taskMutation(ctx, nil, func(tx *sql.Tx, _ *AgentOrigin) error {
		enabled, err := pluginEnabled(ctx, tx, "reclaim")
		if err != nil {
			return err
		}
		if !enabled {
			return nil
		}

		busySessions := make(map[string]struct{})
		agents, _, err := livePresenceSnapshot(ctx, tx, event.EmittedAt)
		if err != nil {
			return fmt.Errorf("read reclaim presence: %w", err)
		}
		for _, agent := range agents {
			if agent.State == "busy" && agent.ProjectIdent != "" {
				busySessions[agent.Origin.SessionID] = struct{}{}
			}
		}

		hasDispatches, err := reclaimTableExists(ctx, tx, "task_dispatches")
		if err != nil {
			return err
		}
		nativeEvidence := make([]string, 0, 3)
		for _, table := range []string{"amux_conversations", "amux_assignment_runs", "amux_worker_runs"} {
			present, err := sqliteTableHasColumn(ctx, tx, table, "task_id")
			if err != nil {
				return err
			}
			if present {
				nativeEvidence = append(nativeEvidence, table)
			}
		}

		cutoff := event.EmittedAt - reclaimAfterMS
		query := `SELECT id,owner_origin_json FROM tasks
			WHERE status IN ('todo','in_progress')
			AND (owner_agent_id IS NOT NULL OR owner_origin_json IS NOT NULL OR status='in_progress')
			AND updated_at<=?`
		if hasDispatches {
			query += ` AND NOT EXISTS(SELECT 1 FROM task_dispatches d WHERE d.task_id=tasks.id)`
		}
		rows, err := tx.QueryContext(ctx, query, cutoff)
		if err != nil {
			return fmt.Errorf("read stale reclaim candidates: %w", err)
		}
		candidates := make([]reclaimCandidate, 0)
		for rows.Next() {
			var candidate reclaimCandidate
			if err := rows.Scan(&candidate.id, &candidate.ownerJSON); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, candidate)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		for _, candidate := range candidates {
			sessionID := ""
			if candidate.ownerJSON.Valid {
				var nativeOrMalformed bool
				sessionID, nativeOrMalformed = reclaimOwnerSession(candidate.ownerJSON.String)
				if nativeOrMalformed {
					continue
				}
			}
			if candidate.ownerJSON.Valid {
				if _, busy := busySessions[sessionID]; sessionID != "" && busy {
					continue
				}
			}
			native := false
			for _, table := range nativeEvidence {
				query := "SELECT EXISTS(SELECT 1 FROM " + table + " WHERE task_id=?)"
				var exists bool
				if err := tx.QueryRowContext(ctx, query, candidate.id).Scan(&exists); err != nil {
					return fmt.Errorf("check native reclaim evidence in %s: %w", table, err)
				}
				if exists {
					native = true
					break
				}
			}
			if native {
				continue
			}
			if _, err := insertTaskComment(ctx, tx, candidate.id, "system", "system", reclaimComment, nil, nil); err != nil {
				return fmt.Errorf("add reclaim comment for task %s: %w", candidate.id, err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='todo',owner_agent_id=NULL,owner_origin_json=NULL,started_at=NULL,updated_at=? WHERE id=?`, event.EmittedAt, candidate.id); err != nil {
				return fmt.Errorf("release stale task %s: %w", candidate.id, err)
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// reclaimOwnerSession accepts partial legacy source JSON while failing closed
// for malformed values and retaining complete normalized native origins.
func reclaimOwnerSession(raw string) (sessionID string, nativeOrMalformed bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil || fields == nil {
		return "", true
	}
	for _, key := range []string{"session_id", "instance_id", "provider", "os"} {
		if value, present := fields[key]; present {
			var text *string
			if err := json.Unmarshal(value, &text); err != nil || text == nil {
				return "", true
			}
		}
	}
	var origin AgentOrigin
	if err := json.Unmarshal([]byte(raw), &origin); err == nil {
		if normalized, err := NormalizeOrigin(&origin); err == nil && normalized != nil {
			return "", true
		}
	}
	var legacy struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		return "", true
	}
	return legacy.SessionID, false
}

func reclaimTableExists(ctx context.Context, q queryer, table string) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)", table).Scan(&exists)
	return exists, err
}
