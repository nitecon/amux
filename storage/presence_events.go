package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/router"
)

// eventPresenceStored describes the canonical row changed by a raw event.
// Changed means the event changed the stored identity payload.
type eventPresenceStored struct {
	Key      [3]string
	Accepted bool
	Changed  bool
}

func nativePresenceSourceAuthoritative(ctx context.Context, q queryer, source string) (bool, error) {
	var uid sql.NullString
	err := q.QueryRowContext(ctx, "SELECT heartbeat_uid FROM amux_presence_sources WHERE source=?", source).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return uid.Valid, nil
}

// storeEventPresenceIn stores one untrusted event observation without emitting,
// advancing a source cursor, or changing the shared clock. Callers own those
// surrounding transaction effects.
func storeEventPresenceIn(ctx context.Context, tx *sql.Tx, source string, raw json.RawMessage, seenAt, seq int64) (eventPresenceStored, error) {
	var stored eventPresenceStored
	if !validRouterSource(source) || seenAt < 0 || seq < 0 {
		return stored, nil
	}
	authoritative, err := nativePresenceSourceAuthoritative(ctx, tx, source)
	if err != nil {
		return stored, err
	}
	if authoritative {
		return stored, nil
	}
	var agent map[string]json.RawMessage
	if err := json.Unmarshal(raw, &agent); err != nil || agent == nil {
		return stored, nil
	}
	for _, name := range []string{"project", "instance_id", "session_id"} {
		var value *string
		if field, ok := agent[name]; !ok || json.Unmarshal(field, &value) != nil || value == nil {
			return stored, nil
		}
	}
	for _, name := range []string{"provider", "os"} {
		var value *string
		if field, ok := agent[name]; !ok || json.Unmarshal(field, &value) != nil || value == nil {
			return stored, nil
		}
	}
	getString := func(name string) string {
		var value string
		_ = json.Unmarshal(agent[name], &value)
		return value
	}
	getOptionalString := func(name string) *string {
		raw, ok := agent[name]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil
		}
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return nil
		}
		return &value
	}
	project := getString("project")
	instance, session := getString("instance_id"), getString("session_id")
	stored.Key = [3]string{project, instance, session}

	var bindingRequired bool
	var bindingRemote any
	if marker, ok := agent["canonical_remote"]; ok {
		var remote *string
		if !bytes.Equal(bytes.TrimSpace(marker), []byte("null")) {
			var value string
			if json.Unmarshal(marker, &value) != nil {
				return stored, nil
			}
			if value = strings.TrimSpace(value); value != "" {
				repo, err := projects.ParseRemote(value)
				if err != nil {
					return stored, nil
				}
				remote = &repo.Canonical
			}
		}
		p, err := getProject(ctx, tx, project)
		if err != nil {
			return stored, err
		}
		if p == nil || p.ArchivedAt != nil {
			return stored, nil
		}
		canonical, err := validateTargetProjectBinding(p, remote)
		if err != nil {
			return stored, nil
		}
		bindingRequired = true
		if canonical != nil {
			bindingRemote = *canonical
		} else {
			bindingRemote = ""
		}
	}

	// Inject the authenticated router source while preserving every other field
	// and number token exactly as supplied.
	agent["source"], _ = json.Marshal(source)
	encoded, err := json.Marshal(agent)
	if err != nil {
		return stored, err
	}
	var priorKind, priorSource, priorState string
	var priorAgent sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT report_kind,source,state,event_agent_json
		FROM amux_presence_agents WHERE project_ident=? AND instance_id=? AND session_id=?`, project, instance, session).
		Scan(&priorKind, &priorSource, &priorState, &priorAgent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return stored, err
	}
	if err == nil && priorKind != "event" {
		return stored, nil
	}
	var foreign int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM amux_presence_agents
		WHERE session_id=? AND source<>? AND state!='gone')`, session, source).Scan(&foreign); err != nil {
		return stored, err
	}
	if foreign != 0 {
		return stored, nil
	}

	changed := true
	if priorAgent.Valid {
		previous, previousErr := semanticRouterJSON(json.RawMessage(priorAgent.String))
		current, currentErr := semanticRouterJSON(encoded)
		changed = previousErr != nil || currentErr != nil || !reflect.DeepEqual(previous, current)
	}
	if err == nil && (priorState == "gone" || priorSource != source) {
		changed = true
	}
	if _, err := tx.ExecContext(ctx, `UPDATE amux_presence_agents SET state='gone',busy=0,draft=0,auto_assign=0,review_context_clear=0
		WHERE session_id=? AND source=? AND report_kind='event' AND NOT(project_ident=? AND instance_id=? AND session_id=?)`,
		session, source, project, instance, session); err != nil {
		return stored, err
	}
	identitySeq := int64(0)
	if err == nil && !changed {
		var priorIdentity int64
		if err := tx.QueryRowContext(ctx, `SELECT identity_seq FROM amux_presence_agents
			WHERE project_ident=? AND instance_id=? AND session_id=?`, project, instance, session).Scan(&priorIdentity); err != nil {
			return stored, err
		}
		identitySeq = priorIdentity
	} else {
		identitySeq = seq
	}
	goal, generation, agentID := getOptionalString("goal"), getOptionalString("generation"), getOptionalString("agent_id")
	if _, err := tx.ExecContext(ctx, `INSERT INTO amux_presence_agents
		(project_ident,instance_id,session_id,provider,os,source,sequence,seen_at,last_activity_at,busy,draft,auto_assign,review_context_clear,state,hostname,goal,generation,agent_id,report_kind,event_agent_json,identity_seq,activity_seq,binding_required,binding_remote)
		VALUES(?,?,?,?,?,?,?, ?,NULL,0,0,0,0,'unknown',?,?,?,?,'event',?,?,?, ?,?)
		ON CONFLICT(project_ident,instance_id,session_id) DO UPDATE SET
		provider=excluded.provider,os=excluded.os,source=excluded.source,sequence=excluded.sequence,
		seen_at=MAX(amux_presence_agents.seen_at,excluded.seen_at),busy=0,draft=CASE WHEN ? THEN 0 ELSE amux_presence_agents.draft END,auto_assign=0,review_context_clear=0,
		state=CASE WHEN ? THEN 'unknown' ELSE amux_presence_agents.state END,hostname=excluded.hostname,goal=excluded.goal,generation=excluded.generation,agent_id=excluded.agent_id,
		last_activity_at=CASE WHEN ? THEN NULL ELSE amux_presence_agents.last_activity_at END,
		report_kind='event',event_agent_json=excluded.event_agent_json,
		identity_seq=excluded.identity_seq,
		activity_seq=CASE WHEN amux_presence_agents.report_kind='event' AND amux_presence_agents.identity_seq=excluded.identity_seq THEN amux_presence_agents.activity_seq ELSE 0 END,
		binding_required=excluded.binding_required,binding_remote=excluded.binding_remote`,
		project, instance, session, getString("provider"), getString("os"), source, seq, seenAt,
		nil, nullableString(goal), nullableString(generation), nullableString(agentID),
		string(encoded), identitySeq, 0, bindingRequired, bindingRemote, changed, changed, changed); err != nil {
		return stored, fmt.Errorf("store raw presence observation: %w", err)
	}
	stored.Accepted, stored.Changed = true, changed
	return stored, nil
}

// ReconcilePresenceEvent applies one raw heartbeat or activity event and emits
// its derived transitions in the same transaction as the router checkpoint.
func (s *Store) ReconcilePresenceEvent(ctx context.Context, registry *router.Registry, event router.Event) error {
	if event.Version != 1 || !validRouterSource(event.Source) || event.Seq < 0 || event.EmittedAt < 0 || !canonicalUUID(event.UID) {
		return nil
	}
	if event.ID != "client.heartbeat" && event.ID != "client.activity" && event.ID != "terminal.activity" {
		return fmt.Errorf("unsupported raw presence event %q", event.ID)
	}
	if _, err := semanticRouterJSON(event.Payload); err != nil {
		return nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &payload); err != nil || payload == nil {
		return nil
	}
	if event.ID == "client.heartbeat" {
		if _, reserved := payload["report_sequence"]; reserved {
			return nil
		}
		if _, reserved := payload["reported_at"]; reserved {
			return nil
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	authoritative, err := nativePresenceSourceAuthoritative(ctx, tx, event.Source)
	if err != nil {
		return err
	}
	if authoritative {
		return nil
	}
	var observed int64
	if err := tx.QueryRowContext(ctx, "UPDATE amux_presence_clock SET observed_at=MAX(observed_at,?) WHERE id=1 RETURNING observed_at", event.EmittedAt).Scan(&observed); err != nil {
		return err
	}
	changed := false
	if event.ID == "client.heartbeat" {
		var agents []json.RawMessage
		if raw, ok := payload["agents"]; !ok || json.Unmarshal(raw, &agents) != nil || agents == nil {
			return nil
		}
		var cursor sql.NullInt64
		err := tx.QueryRowContext(ctx, "SELECT heartbeat_seq FROM rawpresence_sources WHERE source=?", event.Source).Scan(&cursor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if cursor.Valid && event.Seq <= cursor.Int64 {
			if err := tx.Commit(); err != nil {
				return err
			}
			_ = s.bus.Publish(eventbus.Event{Topic: eventbus.PresenceChanged})
			return nil
		}
		osName := ""
		_ = json.Unmarshal(payload["os"], &osName)
		if osName != "linux" && osName != "macos" && osName != "windows" {
			osName = ""
		}
		var hostname *string
		_ = json.Unmarshal(payload["hostname"], &hostname)
		if _, err := tx.ExecContext(ctx, `INSERT INTO rawpresence_sources(source,heartbeat_seq,heartbeat_uid,heartbeat_seen_at,os,hostname)
			VALUES(?,?,?,?,?,?) ON CONFLICT(source) DO UPDATE SET heartbeat_seq=excluded.heartbeat_seq,
			heartbeat_uid=excluded.heartbeat_uid,heartbeat_seen_at=excluded.heartbeat_seen_at,os=excluded.os,hostname=excluded.hostname`,
			event.Source, event.Seq, event.UID, event.EmittedAt, nullableString(&osName), nullableString(hostname)); err != nil {
			return err
		}
		changed = true
		if observed-event.EmittedAt >= presenceGoneAfter {
			if err := tx.Commit(); err != nil {
				return err
			}
			_ = s.bus.Publish(eventbus.Event{Topic: eventbus.PresenceChanged})
			return nil
		}
		listed := make(map[[3]string]struct{}, len(agents))
		for _, raw := range agents {
			var agent map[string]json.RawMessage
			if json.Unmarshal(raw, &agent) != nil || agent == nil {
				continue
			}
			stored, err := storeEventPresenceIn(ctx, tx, event.Source, raw, event.EmittedAt, event.Seq)
			if err != nil {
				return err
			}
			if !stored.Accepted {
				continue
			}
			listed[stored.Key] = struct{}{}
			if stored.Changed {
				if err := capturePresenceTransition(ctx, tx, stored.Key, "online", &event.UID); err != nil {
					return err
				}
				changed = true
			}
			var busy, draft bool
			var activity sql.NullInt64
			if err := tx.QueryRowContext(ctx, `SELECT busy,draft,last_activity_at FROM amux_presence_agents
				WHERE project_ident=? AND instance_id=? AND session_id=?`, stored.Key[0], stored.Key[1], stored.Key[2]).Scan(&busy, &draft, &activity); err != nil {
				return err
			}
			if activity.Valid {
				if err := evaluateStoredEventPresence(ctx, tx, stored.Key, observed, &event.UID, draft); err != nil {
					return err
				}
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT project_ident,instance_id,session_id FROM amux_presence_agents
			WHERE source=? AND report_kind='event' AND state!='gone'`, event.Source)
		if err != nil {
			return err
		}
		var departed [][3]string
		for rows.Next() {
			var key [3]string
			if err := rows.Scan(&key[0], &key[1], &key[2]); err != nil {
				rows.Close()
				return err
			}
			if _, ok := listed[key]; !ok {
				departed = append(departed, key)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, key := range departed {
			if _, err := tx.ExecContext(ctx, `UPDATE amux_presence_agents SET state='gone',busy=0,draft=0,auto_assign=0,review_context_clear=0
				WHERE project_ident=? AND instance_id=? AND session_id=? AND source=? AND report_kind='event'`, key[0], key[1], key[2], event.Source); err != nil {
				return err
			}
			if err := capturePresenceTransition(ctx, tx, key, "gone", &event.UID); err != nil {
				return err
			}
			changed = true
		}
	} else {
		var session, instance string
		_ = json.Unmarshal(payload["session_id"], &session)
		_ = json.Unmarshal(payload["instance_id"], &instance)
		var outputAt *int64
		if raw, ok := payload["output_at"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			var value int64
			if json.Unmarshal(raw, &value) != nil {
				return nil
			}
			outputAt = &value
		}
		var draft bool
		if raw, ok := payload["draft"]; !ok || json.Unmarshal(raw, &draft) != nil {
			return nil
		}
		rows, err := tx.QueryContext(ctx, `SELECT project_ident,instance_id,session_id FROM amux_presence_agents
			WHERE session_id=? AND instance_id=? AND source=? AND report_kind='event' AND state!='gone' AND identity_seq<? AND activity_seq<?`,
			session, instance, event.Source, event.Seq, event.Seq)
		if err != nil {
			return err
		}
		var keys [][3]string
		for rows.Next() {
			var key [3]string
			if err := rows.Scan(&key[0], &key[1], &key[2]); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, key)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, key := range keys {
			if _, err := tx.ExecContext(ctx, `UPDATE amux_presence_agents SET last_activity_at=CASE WHEN ? IS NULL THEN last_activity_at WHEN last_activity_at IS NULL THEN ? ELSE MAX(last_activity_at,?) END,
				draft=?,activity_seq=? WHERE project_ident=? AND instance_id=? AND session_id=? AND source=? AND report_kind='event'`,
				nullInt64(outputAt), nullInt64(outputAt), nullInt64(outputAt), draft, event.Seq, key[0], key[1], key[2], event.Source); err != nil {
				return err
			}
			if err := evaluateStoredEventPresence(ctx, tx, key, observed, &event.UID, draft); err != nil {
				return err
			}
			changed = true
		}
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

func evaluateStoredEventPresence(ctx context.Context, tx *sql.Tx, key [3]string, now int64, correlation *string, draft bool) error {
	var activity sql.NullInt64
	var state, agentJSON string
	if err := tx.QueryRowContext(ctx, `SELECT last_activity_at,state,event_agent_json FROM amux_presence_agents
		WHERE project_ident=? AND instance_id=? AND session_id=? AND report_kind='event'`, key[0], key[1], key[2]).Scan(&activity, &state, &agentJSON); err != nil {
		return err
	}
	if !activity.Valid {
		return nil
	}
	next := "busy"
	if !draft && now-activity.Int64 >= presenceIdleAfter {
		next = "idle"
	}
	if state == next {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE amux_presence_agents SET state=? WHERE project_ident=? AND instance_id=? AND session_id=?`, next, key[0], key[1], key[2]); err != nil {
		return err
	}
	return capturePresenceTransition(ctx, tx, key, next, correlation)
}
