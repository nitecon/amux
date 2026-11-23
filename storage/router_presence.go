package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

var routerPresenceKinds = [...]string{"online", "idle", "busy", "gone"}

// RouterPresenceEventDefinitions declares the durable native presence events.
func RouterPresenceEventDefinitions() []router.EventDefinition {
	definitions := make([]router.EventDefinition, 0, len(routerPresenceKinds))
	for _, kind := range routerPresenceKinds {
		definitions = append(definitions, router.EventDefinition{
			ID: "agent." + kind, Version: 1, RemoteEmit: false, Validate: validateRouterPresencePayload,
		})
	}
	return definitions
}

func validateRouterPresencePayload(payload json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil || object == nil {
		return fmt.Errorf("payload needs an agent with project and session_id strings")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("payload needs one JSON object")
	}
	var envelope map[string]json.RawMessage
	if raw, ok := object["agent"]; !ok || json.Unmarshal(raw, &envelope) != nil || envelope == nil {
		return fmt.Errorf("payload needs an agent with project and session_id strings")
	}
	for _, key := range []string{"project", "session_id"} {
		var value *string
		if raw, ok := envelope[key]; !ok || json.Unmarshal(raw, &value) != nil || value == nil {
			return fmt.Errorf("agent.%s must be a string", key)
		}
	}
	return nil
}

func migrateRouterPresence(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS amux_presence_outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,
		payload TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		correlation TEXT
	)`); err != nil {
		return fmt.Errorf("create amux_presence_outbox: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS amux_presence_outbox_correlation
		ON amux_presence_outbox(correlation,id)`); err != nil {
		return fmt.Errorf("index presence transition causes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(17)"); err != nil {
		return fmt.Errorf("record router presence migration: %w", err)
	}
	return nil
}

type routerPresenceOutboxRow struct {
	id          int64
	kind        string
	payload     []byte
	correlation sql.NullString
}

// DrainRouterPresenceOutbox atomically emits and acknowledges one bounded prefix.
func (s *Store) DrainRouterPresenceOutbox(ctx context.Context, registry *router.Registry, limit int) (int, error) {
	if limit < 1 || limit > 200 {
		return 0, fmt.Errorf("router presence outbox limit must be between 1 and 200")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := readRouterPresenceOutbox(ctx, tx, "", limit)
	if err != nil {
		return 0, err
	}
	stored, err := emitRouterPresenceRows(ctx, tx, registry, rows)
	if err != nil {
		return 0, err
	}
	if err := acknowledgeRouterPresenceRows(ctx, tx, rows); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if stored {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	}
	return len(rows), nil
}

func readRouterPresenceOutbox(ctx context.Context, tx *sql.Tx, correlation string, limit int) ([]routerPresenceOutboxRow, error) {
	query := "SELECT id,kind,payload,correlation FROM amux_presence_outbox ORDER BY id LIMIT ?"
	args := []any{limit}
	if correlation != "" {
		query = "SELECT id,kind,payload,correlation FROM amux_presence_outbox WHERE correlation=? ORDER BY id LIMIT ?"
		args = []any{correlation, limit}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read router presence outbox: %w", err)
	}
	defer rows.Close()
	batch := make([]routerPresenceOutboxRow, 0, limit)
	for rows.Next() {
		var row routerPresenceOutboxRow
		if err := rows.Scan(&row.id, &row.kind, &row.payload, &row.correlation); err != nil {
			return nil, fmt.Errorf("scan router presence outbox: %w", err)
		}
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read router presence outbox rows: %w", err)
	}
	return batch, nil
}

func emitRouterPresenceRows(ctx context.Context, tx *sql.Tx, registry *router.Registry, rows []routerPresenceOutboxRow) (bool, error) {
	stored := false
	for _, row := range rows {
		if !validRouterPresenceKind(row.kind) {
			return false, fmt.Errorf("unknown router presence outbox kind %q", row.kind)
		}
		var correlation *string
		if row.correlation.Valid {
			correlation = &row.correlation.String
		}
		result, err := emitRouterEventIn(ctx, tx, registry, router.EmitRequest{
			ID: "agent." + row.kind, Version: 1, Payload: row.payload,
			Source: "plugin:presence", Correlation: correlation,
		})
		if err != nil {
			return false, fmt.Errorf("emit router presence outbox row %d: %w", row.id, err)
		}
		stored = stored || result.Stored
	}
	return stored, nil
}

func acknowledgeRouterPresenceRows(ctx context.Context, tx *sql.Tx, rows []routerPresenceOutboxRow) error {
	for _, row := range rows {
		result, err := tx.ExecContext(ctx, "DELETE FROM amux_presence_outbox WHERE id=?", row.id)
		if err != nil {
			return fmt.Errorf("acknowledge router presence outbox row %d: %w", row.id, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count acknowledged router presence row %d: %w", row.id, err)
		}
		if count != 1 {
			return fmt.Errorf("acknowledged %d router presence rows for id %d", count, row.id)
		}
	}
	return nil
}

func validRouterPresenceKind(kind string) bool {
	for _, known := range routerPresenceKinds {
		if kind == known {
			return true
		}
	}
	return false
}

// ReconcileRouterPresence applies one monotonic clock tick and emits its causal
// transitions in the same transaction. Earlier native rows retain no correlation.
func (s *Store) ReconcileRouterPresence(ctx context.Context, registry *router.Registry, event router.Event) error {
	if (event.ID == "client.heartbeat" || event.ID == "client.activity" || event.ID == "terminal.activity") && event.Version == 1 {
		return s.ReconcilePresenceEvent(ctx, registry, event)
	}
	if event.ID != "clock.tick" || event.Version != 1 || !canonicalUUID(event.UID) {
		return fmt.Errorf("presence reconciliation requires a clock.tick v1 event with a canonical UID")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var observed int64
	if err := tx.QueryRowContext(ctx, "UPDATE amux_presence_clock SET observed_at=MAX(observed_at,?) WHERE id=1 RETURNING observed_at", event.EmittedAt).Scan(&observed); err != nil {
		return err
	}
	correlation := event.UID
	changed, err := reconcilePresenceIn(ctx, tx, observed, &correlation)
	if err != nil {
		return err
	}
	rows, err := readRouterPresenceOutbox(ctx, tx, event.UID, 200)
	if err != nil {
		return err
	}
	stored, err := emitRouterPresenceRows(ctx, tx, registry, rows)
	if err != nil {
		return err
	}
	if err := acknowledgeRouterPresenceRows(ctx, tx, rows); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if stored {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	}
	if changed {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.PresenceChanged})
	}
	return nil
}

// LivePresence returns a global TTL-filtered snapshot without changing the clock.
func (s *Store) LivePresence(ctx context.Context, now time.Time) ([]AgentPresence, error) {
	result, _, err := livePresenceSnapshot(ctx, s.db, now.UnixMilli())
	return result, err
}

func livePresenceSnapshot(ctx context.Context, q queryer, nowMS int64) ([]AgentPresence, int64, error) {
	rows, err := q.QueryContext(ctx, `WITH clock AS (
		SELECT MAX(observed_at,?) AS observed_at FROM amux_presence_clock WHERE id=1
	), live AS (
		SELECT clock.observed_at, a.project_ident, a.instance_id, a.session_id, a.provider, a.os, a.source,
			a.seen_at, a.last_activity_at, a.busy, a.draft, a.auto_assign, a.review_context_clear, a.state, a.hostname, a.goal, a.generation, a.agent_id,
			a.binding_required
		FROM clock LEFT JOIN amux_presence_agents a
			ON a.state!='gone' AND clock.observed_at-a.seen_at<?
			AND (a.binding_required=0 OR EXISTS (
				SELECT 1 FROM projects p WHERE p.ident=lower(trim(a.project_ident))
				AND (p.canonical_remote=COALESCE(a.binding_remote,'') OR
					(p.kind='adhoc' AND p.canonical_remote IS NULL AND COALESCE(a.binding_remote,'')=''))
			))
	)
	SELECT observed_at,project_ident,instance_id,session_id,provider,os,source,seen_at,last_activity_at,busy,draft,auto_assign,review_context_clear,state,hostname,goal,generation,agent_id
	FROM live
	ORDER BY COALESCE(last_activity_at,seen_at),session_id,instance_id,project_ident`, nowMS, presenceGoneAfter)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := make([]AgentPresence, 0)
	observedMS := nowMS
	for rows.Next() {
		var item AgentPresence
		var project, instance, session, provider, osName, source, state sql.NullString
		var seen, activity sql.NullInt64
		var busy, draft, autoAssign, reviewContextClear sql.NullBool
		var hostname, goal, generation, agentID sql.NullString
		if err := rows.Scan(&observedMS, &project, &instance, &session, &provider, &osName, &source, &seen, &activity, &busy, &draft, &autoAssign, &reviewContextClear, &state, &hostname, &goal, &generation, &agentID); err != nil {
			return nil, 0, err
		}
		if !project.Valid {
			continue
		}
		item.ProjectIdent, item.Origin.InstanceID, item.Origin.SessionID = project.String, instance.String, session.String
		item.Origin.Provider, item.Origin.OS, item.Source = provider.String, osName.String, source.String
		item.SeenAt, item.Busy, item.Draft = seen.Int64, busy.Bool, draft.Bool
		item.AutoAssign, item.ReviewContextClear, item.State = autoAssign.Bool, reviewContextClear.Bool, state.String
		if activity.Valid {
			item.LastActivityAt = &activity.Int64
		}
		if hostname.Valid {
			item.Hostname = &hostname.String
		}
		if goal.Valid {
			item.Goal = &goal.String
		}
		if generation.Valid {
			item.Generation = &generation.String
		}
		if agentID.Valid {
			item.AgentID = &agentID.String
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return result, observedMS, nil
}

func capturePresenceTransition(ctx context.Context, tx *sql.Tx, key [3]string, kind string, correlation *string) error {
	if !validRouterPresenceKind(kind) {
		return fmt.Errorf("unknown presence transition %q", kind)
	}
	var agent routerPresenceAgent
	var activity sql.NullInt64
	var hostname, goal, generation, agentID sql.NullString
	var reportKind string
	var eventAgent sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT project_ident,session_id,instance_id,provider,os,source,busy,draft,auto_assign,review_context_clear,last_activity_at,seen_at,state,hostname,goal,generation,agent_id,report_kind,event_agent_json
		FROM amux_presence_agents WHERE project_ident=? AND instance_id=? AND session_id=?`, key[0], key[1], key[2]).Scan(
		&agent.Project, &agent.SessionID, &agent.InstanceID, &agent.Provider, &agent.OS, &agent.Source,
		&agent.Busy, &agent.Draft, &agent.AutoAssign, &agent.ReviewContextClear, &activity, &agent.SeenAt, &agent.State,
		&hostname, &goal, &generation, &agentID, &reportKind, &eventAgent,
	); err != nil {
		return err
	}
	if activity.Valid {
		agent.LastActivityAt = &activity.Int64
	}
	if hostname.Valid {
		agent.Hostname = &hostname.String
	}
	if goal.Valid {
		agent.Goal = &goal.String
	}
	if generation.Valid {
		agent.Generation = &generation.String
	}
	if agentID.Valid {
		agent.AgentID = &agentID.String
	}
	var payload []byte
	var err error
	if reportKind == "event" && eventAgent.Valid {
		payload = []byte(`{"agent":` + eventAgent.String + `}`)
		if !json.Valid(payload) {
			return fmt.Errorf("invalid stored event presence agent JSON")
		}
	} else {
		payload, err = json.Marshal(struct {
			Agent routerPresenceAgent `json:"agent"`
		}{Agent: agent})
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO amux_presence_outbox(kind,payload,created_at,correlation) VALUES(?,?,?,?)", kind, string(payload), time.Now().UTC().UnixMilli(), nullableString(correlation))
	return err
}

type routerPresenceAgent struct {
	Project            string  `json:"project"`
	SessionID          string  `json:"session_id"`
	InstanceID         string  `json:"instance_id"`
	Provider           string  `json:"provider"`
	OS                 string  `json:"os"`
	Source             string  `json:"source"`
	Busy               bool    `json:"busy"`
	Draft              bool    `json:"draft"`
	AutoAssign         bool    `json:"auto_assign"`
	ReviewContextClear bool    `json:"review_context_clear"`
	LastActivityAt     *int64  `json:"last_activity_at"`
	SeenAt             int64   `json:"seen_at"`
	State              string  `json:"state"`
	Hostname           *string `json:"hostname,omitempty"`
	Goal               *string `json:"goal,omitempty"`
	Generation         *string `json:"generation,omitempty"`
	AgentID            *string `json:"agent_id,omitempty"`
}
