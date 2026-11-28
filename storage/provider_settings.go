package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nitecon/amux/identity/agent"
)

var switchableProviderIDs = [...]string{"antigravity", "claude", "codex"}

var ErrUnknownProvider = errors.New("unknown provider")

const providerDisabledComment = "Task released because its agent provider was disabled in settings. Next agent please verify prior progress before continuing."

type ProviderSetting struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

// SwitchableProviderIDs returns the fixed provider order used by the API.
func SwitchableProviderIDs() []string {
	return append([]string(nil), switchableProviderIDs[:]...)
}

func isSwitchableProvider(id string) bool {
	for _, known := range switchableProviderIDs {
		if id == known {
			return true
		}
	}
	return false
}

// providerEnabled defaults to enabled; only the literal "false" disables one.
// Unknown providers are reported enabled so legacy origins keep their behavior.
func providerEnabled(ctx context.Context, q queryer, id string) (bool, error) {
	if !isSwitchableProvider(id) {
		return true, nil
	}
	var value string
	err := q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", "provider."+id+".enabled").Scan(&value)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return value != "false", nil
}

// disabledProviders returns the set of explicitly disabled providers.
func disabledProviders(ctx context.Context, q queryer) (map[string]bool, error) {
	disabled := make(map[string]bool)
	for _, id := range switchableProviderIDs {
		enabled, err := providerEnabled(ctx, q, id)
		if err != nil {
			return nil, err
		}
		if !enabled {
			disabled[id] = true
		}
	}
	return disabled, nil
}

// ProviderSettings returns every switchable provider in API order.
func (s *Store) ProviderSettings(ctx context.Context) ([]ProviderSetting, error) {
	disabled, err := disabledProviders(ctx, s.db)
	if err != nil {
		return nil, err
	}
	settings := make([]ProviderSetting, 0, len(switchableProviderIDs))
	for _, id := range switchableProviderIDs {
		settings = append(settings, ProviderSetting{ID: id, Enabled: !disabled[id]})
	}
	return settings, nil
}

// SetProviderEnabled persists a provider switch. Disabling a provider also
// releases its active tasks back to the automatic queue in the same
// transaction so enabled agents can pick them up. It returns the number of
// released tasks.
func (s *Store) SetProviderEnabled(ctx context.Context, id string, enabled bool) (int, error) {
	if !isSwitchableProvider(id) {
		return 0, ErrUnknownProvider
	}
	value := "false"
	if enabled {
		value = "true"
	}
	released := 0
	err := s.taskMutation(ctx, nil, func(tx *sql.Tx, _ *agent.Origin) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "provider."+id+".enabled", value); err != nil {
			return err
		}
		if enabled {
			return nil
		}
		var err error
		released, err = releaseProviderTasks(ctx, tx, id, time.Now().UnixMilli())
		return err
	})
	if err != nil {
		return 0, err
	}
	return released, nil
}

// releaseProviderTasks returns active tasks owned by, or routed to, a provider
// to the automatic queue. Dispatch-managed tasks keep their own handoff API.
func releaseProviderTasks(ctx context.Context, tx *sql.Tx, provider string, nowMS int64) (int, error) {
	_, hasDispatches, err := sqliteTableColumns(ctx, tx, "task_dispatches")
	if err != nil {
		return 0, err
	}
	query := `SELECT id FROM tasks
		WHERE status IN ('todo','in_progress')
		AND ((json_valid(owner_origin_json) AND json_extract(owner_origin_json,'$.provider')=?)
			OR (json_valid(routing_json) AND json_extract(routing_json,'$.target.origin.provider')=?))`
	if hasDispatches {
		query += ` AND NOT EXISTS(SELECT 1 FROM task_dispatches d WHERE d.task_id=tasks.id)`
	}
	rows, err := tx.QueryContext(ctx, query, provider, provider)
	if err != nil {
		return 0, fmt.Errorf("read provider tasks: %w", err)
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	for _, id := range ids {
		if _, err := insertTaskComment(ctx, tx, id, "amux", "system", providerDisabledComment, nil, nil); err != nil {
			return 0, fmt.Errorf("add provider release comment for task %s: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='todo',owner_agent_id=NULL,owner_origin_json=NULL,
			routing_json='{"mode":"auto"}',started_at=NULL,updated_at=? WHERE id=?`, nowMS, id); err != nil {
			return 0, fmt.Errorf("release provider task %s: %w", id, err)
		}
	}
	// Review rework pins a source task to one session; drop pins held by
	// sessions of the disabled provider so other agents can take the task.
	if _, err := tx.ExecContext(ctx, `DELETE FROM amux_review_rework WHERE session_id IN
		(SELECT session_id FROM amux_presence_agents WHERE provider=?)`, provider); err != nil {
		return 0, fmt.Errorf("release provider rework pins: %w", err)
	}
	return len(ids), nil
}
