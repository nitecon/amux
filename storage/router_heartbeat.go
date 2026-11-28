package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
)

func migrateRouterHeartbeat(ctx context.Context, tx *sql.Tx) error {
	for _, column := range []struct{ name, definition string }{
		{"heartbeat_uid", "TEXT"},
		{"heartbeat_seen_at", "INTEGER"},
	} {
		found, err := sqliteTableHasColumn(ctx, tx, "amux_presence_sources", column.name)
		if err != nil {
			return fmt.Errorf("inspect presence source column %s: %w", column.name, err)
		}
		if !found {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE amux_presence_sources ADD COLUMN "+column.name+" "+column.definition); err != nil {
				return fmt.Errorf("add presence source column %s: %w", column.name, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS amux_heartbeat_outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		uid TEXT NOT NULL UNIQUE,
		source TEXT NOT NULL,
		payload TEXT NOT NULL,
		created_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create amux_heartbeat_outbox: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(19)"); err != nil {
		return fmt.Errorf("record router heartbeat migration: %w", err)
	}
	return nil
}

func captureNativeHeartbeatIn(ctx context.Context, tx *sql.Tx, report PresenceReport, reportedAt int64) error {
	uid, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate native heartbeat UID: %w", err)
	}
	payload, err := json.Marshal(struct {
		ClientRun      *string                `json:"client_run,omitempty"`
		OS             string                 `json:"os,omitempty"`
		Hostname       *string                `json:"hostname,omitempty"`
		ReportSequence int64                  `json:"report_sequence"`
		ReportedAt     int64                  `json:"reported_at"`
		Agents         []routerHeartbeatAgent `json:"agents"`
	}{
		ClientRun: report.ClientRun, OS: report.OS, Hostname: report.Hostname, ReportSequence: report.Sequence, ReportedAt: reportedAt,
		Agents: flattenHeartbeatAgents(report),
	})
	if err != nil {
		return fmt.Errorf("marshal native heartbeat: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE amux_presence_sources SET heartbeat_uid=?,heartbeat_seen_at=MAX(COALESCE(heartbeat_seen_at,?),?) WHERE source=? AND sequence=?`, uid.String(), reportedAt, reportedAt, report.Source, report.Sequence)
	if err != nil {
		return fmt.Errorf("record native heartbeat source: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count native heartbeat source updates: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("recorded native heartbeat for %d source rows; expected 1", updated)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO amux_heartbeat_outbox(uid,source,payload,created_at) VALUES(?,?,?,?)`, uid.String(), report.Source, string(payload), reportedAt); err != nil {
		return fmt.Errorf("capture native heartbeat outbox row: %w", err)
	}
	return nil
}

type routerHeartbeatAgent struct {
	Project            string  `json:"project"`
	SessionID          string  `json:"session_id"`
	InstanceID         string  `json:"instance_id"`
	Provider           string  `json:"provider"`
	OS                 string  `json:"os"`
	Busy               bool    `json:"busy"`
	Draft              bool    `json:"draft"`
	AutoAssign         bool    `json:"auto_assign"`
	ReviewContextClear bool    `json:"review_context_clear"`
	LastActivityAt     *int64  `json:"last_activity_at,omitempty"`
	Hostname           *string `json:"hostname,omitempty"`
	Goal               *string `json:"goal,omitempty"`
	Generation         *string `json:"generation,omitempty"`
	AgentID            *string `json:"agent_id,omitempty"`
}

func flattenHeartbeatAgents(report PresenceReport) []routerHeartbeatAgent {
	agents := make([]routerHeartbeatAgent, 0, len(report.Agents))
	for _, agent := range report.Agents {
		agents = append(agents, routerHeartbeatAgent{
			Project: agent.ProjectIdent, SessionID: agent.Origin.SessionID, InstanceID: agent.Origin.InstanceID,
			Provider: agent.Origin.Provider, OS: agent.Origin.OS, Busy: agent.Busy, Draft: agent.Draft,
			AutoAssign: agent.AutoAssign, ReviewContextClear: agent.ReviewContextClear,
			LastActivityAt: agent.LastActivityAt, Hostname: agent.Hostname,
			Goal: agent.Goal, Generation: agent.Generation, AgentID: agent.AgentID,
		})
	}
	return agents
}

// DrainRouterHeartbeatOutbox emits and acknowledges one bounded prefix atomically.
func (s *Store) DrainRouterHeartbeatOutbox(ctx context.Context, registry *fism.Registry, limit int) (int, error) {
	if limit < 1 || limit > 200 {
		return 0, fmt.Errorf("router heartbeat outbox limit must be between 1 and 200")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	batch, err := readObservations(ctx, tx, 0, limit, "e.seq IS NULL")
	if err != nil {
		return 0, err
	}
	stored := false
	for _, row := range batch {
		result, err := emitRouterEventIn(ctx, tx, registry, fism.EmitRequest{
			UID: row.Event.UID, ID: "client.heartbeat", Version: 1, Source: row.Event.Source, Payload: row.Event.Payload,
		})
		if err != nil {
			return 0, fmt.Errorf("emit router heartbeat outbox row %d: %w", row.ID, err)
		}
		stored = stored || result.Stored
		if err := settleObservation(ctx, tx, row.ID, row.Event.UID); err != nil {
			return 0, err
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

// routerHeartbeatCurrent verifies the persisted source binding, sequence, and TTL.
func routerHeartbeatCurrent(ctx context.Context, q queryer, event fism.Event, nowMS int64) (bool, error) {
	if event.ID != "client.heartbeat" || event.Version != 1 || !canonicalUUID(event.UID) || !validRouterSource(event.Source) {
		return false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(event.Payload))
	var payload struct {
		ClientRun      *string                 `json:"client_run"`
		OS             string                  `json:"os"`
		Hostname       *string                 `json:"hostname"`
		ReportSequence *int64                  `json:"report_sequence"`
		ReportedAt     *int64                  `json:"reported_at"`
		Agents         *[]routerHeartbeatAgent `json:"agents"`
	}
	if err := decoder.Decode(&payload); err != nil {
		return false, nil
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF {
		return false, nil
	}
	var markerFields map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &markerFields); err != nil || markerFields == nil {
		return false, nil
	}
	// Raw client heartbeats have no native report marker. They use the separate
	// retained event cursor and never participate in the native source branch.
	if _, hasSequence := markerFields["report_sequence"]; !hasSequence {
		if _, hasReportedAt := markerFields["reported_at"]; !hasReportedAt {
			authoritative, err := nativePresenceSourceAuthoritative(ctx, q, event.Source)
			if err != nil {
				return false, err
			}
			if authoritative {
				return false, nil
			}
			var cursor int64
			var uid sql.NullString
			var seen sql.NullInt64
			err = q.QueryRowContext(ctx, `SELECT heartbeat_seq,heartbeat_uid,heartbeat_seen_at
			FROM rawpresence_sources WHERE source=?`, event.Source).Scan(&cursor, &uid, &seen)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return false, nil
				}
				return false, err
			}
			if cursor != event.Seq || !uid.Valid || uid.String != event.UID || !seen.Valid {
				return false, nil
			}
			var storedClock int64
			if err := q.QueryRowContext(ctx, "SELECT observed_at FROM amux_presence_clock WHERE id=1").Scan(&storedClock); err != nil {
				return false, err
			}
			return max(storedClock, nowMS)-seen.Int64 < presenceGoneAfter, nil
		}
	}
	if payload.ReportSequence == nil || *payload.ReportSequence < 0 || payload.ReportedAt == nil || payload.Agents == nil {
		return false, nil
	}
	if payload.ClientRun != nil && !canonicalUUID(*payload.ClientRun) {
		return false, nil
	}
	for _, agent := range *payload.Agents {
		if agent.Project == "" || agent.SessionID == "" || agent.InstanceID == "" || agent.Provider == "" || agent.OS == "" {
			return false, nil
		}
		if agent.Generation != nil && !canonicalUUID(*agent.Generation) {
			return false, nil
		}
		if agent.Goal != nil {
			goal, err := NormalizeTaskGoal(agent.Goal)
			if err != nil || !sameOptionalString(goal, agent.Goal) {
				return false, nil
			}
		}
		if agent.AgentID != nil {
			id := strings.TrimSpace(*agent.AgentID)
			if id == "" || len(id) > 128 || strings.IndexFunc(id, unicode.IsControl) >= 0 || id != *agent.AgentID {
				return false, nil
			}
		}
	}
	var uid sql.NullString
	var seen sql.NullInt64
	var currentSequence int64
	err := q.QueryRowContext(ctx, `SELECT heartbeat_uid,heartbeat_seen_at,sequence FROM amux_presence_sources WHERE source=?`, event.Source).Scan(&uid, &seen, &currentSequence)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	if !uid.Valid || !seen.Valid || uid.String != event.UID || currentSequence != *payload.ReportSequence {
		return false, nil
	}
	var storedClock int64
	if err := q.QueryRowContext(ctx, "SELECT observed_at FROM amux_presence_clock WHERE id=1").Scan(&storedClock); err != nil {
		return false, err
	}
	return max(storedClock, nowMS)-seen.Int64 < presenceGoneAfter, nil
}
