package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

const presenceObservationMigration = 27

// migratePresenceObservations moves Rust event observations into the canonical
// event store while leaving the retained target directory and Rust archives
// intact. The caller owns the surrounding migration transaction.
func migratePresenceObservations(ctx context.Context, tx *sql.Tx) error {
	for _, column := range []struct{ name, definition string }{
		{"report_kind", "TEXT NOT NULL DEFAULT 'native' CHECK(report_kind IN ('native','event'))"},
		{"event_agent_json", "TEXT"},
		{"identity_seq", "INTEGER NOT NULL DEFAULT 0"},
		{"activity_seq", "INTEGER NOT NULL DEFAULT 0"},
		{"binding_required", "INTEGER NOT NULL DEFAULT 0"},
		{"binding_remote", "TEXT"},
	} {
		found, err := sqliteTableHasColumn(ctx, tx, "amux_presence_agents", column.name)
		if err != nil {
			return fmt.Errorf("inspect canonical presence column %s: %w", column.name, err)
		}
		if !found {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE amux_presence_agents ADD COLUMN "+column.name+" "+column.definition); err != nil {
				return fmt.Errorf("add canonical presence column %s: %w", column.name, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS amux_presence_session
		ON amux_presence_agents(session_id,source,state,seen_at);
		CREATE TRIGGER IF NOT EXISTS amux_presence_project_deleted
		AFTER DELETE ON projects BEGIN
			DELETE FROM amux_presence_agents WHERE project_ident=OLD.ident;
		END`); err != nil {
		return fmt.Errorf("create canonical presence project cleanup trigger: %w", err)
	}

	if err := ensureRawPresenceSources(ctx, tx); err != nil {
		return err
	}
	if err := mergeLegacyPresenceClock(ctx, tx); err != nil {
		return err
	}
	var imported bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM amux_go_migrations WHERE version=?)", presenceObservationMigration).Scan(&imported); err != nil {
		return fmt.Errorf("check presence observation migration ledger: %w", err)
	}
	if !imported {
		if err := copyLegacyPresenceSourceCursors(ctx, tx); err != nil {
			return err
		}
		if err := importRetainedPresenceObservations(ctx, tx); err != nil {
			return err
		}
		if err := backfillImportedHeartbeats(ctx, tx); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)", presenceObservationMigration); err != nil {
		return fmt.Errorf("record presence observation migration: %w", err)
	}
	return nil
}

func ensureRawPresenceSources(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS rawpresence_sources (
		source TEXT PRIMARY KEY, heartbeat_seq INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create raw presence source archive: %w", err)
	}
	for _, column := range []struct{ name, definition string }{
		{"heartbeat_uid", "TEXT"}, {"heartbeat_seen_at", "INTEGER"}, {"os", "TEXT"}, {"hostname", "TEXT"},
	} {
		found, err := sqliteTableHasColumn(ctx, tx, "rawpresence_sources", column.name)
		if err != nil {
			return fmt.Errorf("inspect raw presence source column %s: %w", column.name, err)
		}
		if !found {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE rawpresence_sources ADD COLUMN "+column.name+" "+column.definition); err != nil {
				return fmt.Errorf("add raw presence source column %s: %w", column.name, err)
			}
		}
	}
	return nil
}

func copyLegacyPresenceSourceCursors(ctx context.Context, tx *sql.Tx) error {
	legacy, err := sqliteTableHasColumn(ctx, tx, "presence_sources", "heartbeat_seq")
	if err != nil {
		return fmt.Errorf("inspect retained Rust presence source archive: %w", err)
	}
	if legacy {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO rawpresence_sources(source,heartbeat_seq)
			SELECT source,heartbeat_seq FROM presence_sources`); err != nil {
			return fmt.Errorf("copy retained Rust presence source cursors: %w", err)
		}
	}
	return nil
}

func mergeLegacyPresenceClock(ctx context.Context, tx *sql.Tx) error {
	found, err := sqliteTableHasColumn(ctx, tx, "presence_clock", "observed_at")
	if err != nil {
		return fmt.Errorf("inspect retained Rust presence clock: %w", err)
	}
	if !found {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE amux_presence_clock SET observed_at=MAX(observed_at,
		COALESCE((SELECT MAX(observed_at) FROM presence_clock),0)) WHERE id=1`); err != nil {
		return fmt.Errorf("merge retained Rust presence clock: %w", err)
	}
	return nil
}

type importedPresenceObservation struct {
	source       string
	raw          []byte
	seen         int64
	seq          int64
	activitySeq  int64
	activity     sql.NullInt64
	draft, state string
}

func importRetainedPresenceObservations(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT source,agent_json,report_seen_at,identity_seq,activity_seq,
		output_at,draft,state FROM task_target_presence
		WHERE source IS NOT NULL AND agent_json IS NOT NULL AND report_seen_at IS NOT NULL
		ORDER BY project_ident,instance_id,session_id`)
	if err != nil {
		return fmt.Errorf("read retained directory observations: %w", err)
	}
	observations := make([]importedPresenceObservation, 0)
	for rows.Next() {
		var observation importedPresenceObservation
		var raw string
		if err := rows.Scan(&observation.source, &raw, &observation.seen, &observation.seq, &observation.activitySeq,
			&observation.activity, &observation.draft, &observation.state); err != nil {
			rows.Close()
			return fmt.Errorf("scan retained directory observation: %w", err)
		}
		observation.raw = []byte(raw)
		observations = append(observations, observation)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read retained directory observations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close retained directory observations: %w", err)
	}
	importedKeys := make(map[[3]string]struct{}, len(observations))
	for _, observation := range observations {
		stored, err := storeEventPresenceIn(ctx, tx, observation.source, json.RawMessage(observation.raw), observation.seen, observation.seq)
		if err != nil {
			return fmt.Errorf("import retained presence observation from %q: %w", observation.source, err)
		}
		var legacyKey struct {
			Project  string `json:"project"`
			Instance string `json:"instance_id"`
			Session  string `json:"session_id"`
		}
		if json.Unmarshal(observation.raw, &legacyKey) == nil {
			importedKeys[[3]string{legacyKey.Project, legacyKey.Instance, legacyKey.Session}] = struct{}{}
		}
		if !stored.Accepted {
			continue
		}
		observation.state = retainedObservationState(observation.state)
		if _, err := tx.ExecContext(ctx, `UPDATE amux_presence_agents SET last_activity_at=?,draft=?,state=?,
			identity_seq=?,activity_seq=? WHERE project_ident=? AND instance_id=? AND session_id=? AND report_kind='event'`,
			nullableInt(observation.activity), observation.draft, observation.state, observation.seq, observation.activitySeq,
			stored.Key[0], stored.Key[1], stored.Key[2]); err != nil {
			return fmt.Errorf("restore retained observation snapshot: %w", err)
		}
	}
	return importLegacyPresenceAgents(ctx, tx, importedKeys)
}

func nullableInt(value sql.NullInt64) any {
	if !value.Valid {
		return nil
	}
	return value.Int64
}

func importLegacyPresenceAgents(ctx context.Context, tx *sql.Tx, importedKeys map[[3]string]struct{}) error {
	found, err := sqliteTableHasColumn(ctx, tx, "presence_agents", "agent_json")
	if err != nil {
		return fmt.Errorf("inspect old presence observation archive: %w", err)
	}
	if !found {
		return nil
	}
	columns := map[string]bool{}
	for _, name := range []string{"source", "seen_at", "output_at", "draft", "state", "identity_seq", "activity_seq"} {
		columns[name], err = sqliteTableHasColumn(ctx, tx, "presence_agents", name)
		if err != nil {
			return fmt.Errorf("inspect old presence archive column %s: %w", name, err)
		}
	}
	if !columns["source"] || !columns["seen_at"] {
		return nil
	}
	selectCol := func(name string, fallback string) string {
		if columns[name] {
			return name
		}
		return fallback
	}
	query := `SELECT source,agent_json,seen_at,` + selectCol("output_at", "NULL") + `,` +
		selectCol("draft", "0") + `,` + selectCol("state", "'unknown'") + `,` +
		selectCol("identity_seq", "0") + `,` + selectCol("activity_seq", "0") + ` FROM presence_agents ORDER BY source,seen_at,agent_json`
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("read old presence observations: %w", err)
	}
	defer rows.Close()
	var observations []importedPresenceObservation
	for rows.Next() {
		var item importedPresenceObservation
		var raw string
		if err := rows.Scan(&item.source, &raw, &item.seen, &item.activity, &item.draft, &item.state, &item.seq, &item.activitySeq); err != nil {
			return fmt.Errorf("scan old presence observation: %w", err)
		}
		item.raw = []byte(raw)
		observations = append(observations, item)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read old presence observations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close old presence observations: %w", err)
	}
	for _, observation := range observations {
		var payload struct {
			Project  string `json:"project"`
			Instance string `json:"instance_id"`
			Session  string `json:"session_id"`
		}
		if json.Unmarshal(observation.raw, &payload) != nil {
			continue
		}
		key := [3]string{payload.Project, payload.Instance, payload.Session}
		if _, exists := importedKeys[key]; exists {
			continue
		}
		stored, err := storeEventPresenceIn(ctx, tx, observation.source, json.RawMessage(observation.raw), observation.seen, observation.seq)
		if err != nil {
			return fmt.Errorf("import old presence observation from %q: %w", observation.source, err)
		}
		if !stored.Accepted {
			continue
		}
		observation.state = retainedObservationState(observation.state)
		if _, err := tx.ExecContext(ctx, `UPDATE amux_presence_agents SET last_activity_at=?,draft=?,state=?,identity_seq=?,activity_seq=?
			WHERE project_ident=? AND instance_id=? AND session_id=? AND report_kind='event'`, nullableInt(observation.activity), observation.draft,
			observation.state, observation.seq, observation.activitySeq, stored.Key[0], stored.Key[1], stored.Key[2]); err != nil {
			return fmt.Errorf("restore old observation snapshot: %w", err)
		}
		importedKeys[[3]string{payload.Project, payload.Instance, payload.Session}] = struct{}{}
	}
	return nil
}

func retainedObservationState(state string) string {
	if state == "unknown" || state == "busy" || state == "idle" || state == "gone" {
		return state
	}
	return "unknown"
}

func backfillImportedHeartbeats(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT source,heartbeat_seq FROM rawpresence_sources ORDER BY source`)
	if err != nil {
		return fmt.Errorf("read imported heartbeat cursors: %w", err)
	}
	type sourceCursor struct {
		source string
		seq    int64
	}
	var cursors []sourceCursor
	for rows.Next() {
		var item sourceCursor
		if err := rows.Scan(&item.source, &item.seq); err != nil {
			rows.Close()
			return err
		}
		cursors = append(cursors, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, cursor := range cursors {
		var uid, payload string
		var emitted int64
		err := tx.QueryRowContext(ctx, `SELECT uid,payload,emitted_at FROM router_events
			WHERE seq=? AND source=? AND id='client.heartbeat'`, cursor.seq, cursor.source).Scan(&uid, &payload, &emitted)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return fmt.Errorf("read current imported heartbeat %q: %w", cursor.source, err)
		}
		if !canonicalUUID(uid) {
			continue
		}
		var summary struct {
			OS       string  `json:"os"`
			Hostname *string `json:"hostname"`
		}
		if json.Unmarshal([]byte(payload), &summary) != nil {
			continue
		}
		osName := ""
		switch summary.OS {
		case "linux", "macos", "windows":
			osName = summary.OS
		}
		hostname, hostErr := normalizePresenceHostname(summary.Hostname)
		if hostErr != nil {
			hostname = nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rawpresence_sources SET
			heartbeat_uid=COALESCE(heartbeat_uid,?),
			heartbeat_seen_at=COALESCE(heartbeat_seen_at,?),
			os=CASE WHEN os IS NULL OR os='' THEN ? ELSE os END,
			hostname=CASE WHEN hostname IS NULL OR hostname='' THEN ? ELSE hostname END
			WHERE source=? AND heartbeat_seq=?`, uid, emitted, nullableObservationString(osName), hostname, cursor.source, cursor.seq); err != nil {
			return fmt.Errorf("backfill retained raw heartbeat inventory for %q: %w", cursor.source, err)
		}
	}
	return nil
}

func nullableObservationString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
