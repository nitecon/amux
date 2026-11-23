package storage

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

const presenceSummaryMigration = 26

// migratePresenceSummary retains machine metadata on the sequence-fenced
// source row so heartbeat delivery and event retention cannot erase it.
func migratePresenceSummary(ctx context.Context, tx *sql.Tx) error {
	for _, column := range []struct{ name, definition string }{
		{"os", "TEXT"},
		{"hostname", "TEXT"},
	} {
		found, err := sqliteTableHasColumn(ctx, tx, "amux_presence_sources", column.name)
		if err != nil {
			return fmt.Errorf("inspect presence summary column %s: %w", column.name, err)
		}
		if !found {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE amux_presence_sources ADD COLUMN "+column.name+" "+column.definition); err != nil {
				return fmt.Errorf("add presence summary column %s: %w", column.name, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)", presenceSummaryMigration); err != nil {
		return fmt.Errorf("record presence summary migration: %w", err)
	}
	return nil
}

type PresenceSummary struct {
	Machines    int                 `json:"machines"`
	OS          []string            `json:"os"`
	Agents      int                 `json:"agents"`
	Active      int                 `json:"active"`
	Idle        int                 `json:"idle"`
	ThisMachine *MachineAgentCounts `json:"this_machine,omitempty"`
}

type MachineAgentCounts struct {
	Agents int `json:"agents"`
	Active int `json:"active"`
	Idle   int `json:"idle"`
}

// PresenceSummary reports current machines and live native agents from one
// read transaction. The source heartbeat cursor, not its drainable event, owns
// the machine lease and its optional host metadata.
func (s *Store) PresenceSummary(ctx context.Context, now time.Time) (PresenceSummary, error) {
	return s.PresenceSummaryForHost(ctx, now, "")
}

func (s *Store) PresenceSummaryForHost(ctx context.Context, now time.Time, hostname string) (PresenceSummary, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PresenceSummary{}, err
	}
	defer tx.Rollback()

	agents, observed, err := livePresenceSnapshot(ctx, tx, now.UnixMilli())
	if err != nil {
		return PresenceSummary{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT source,heartbeat_uid,heartbeat_seen_at,os,hostname
		FROM amux_presence_sources WHERE heartbeat_uid IS NOT NULL
		ORDER BY source`)
	if err != nil {
		return PresenceSummary{}, err
	}
	type sourceInventory struct {
		os, hostname string
	}
	sources := make(map[string]sourceInventory)
	nativeSources := make(map[string]struct{})
	machines := make(map[string]struct{})
	osSet := make(map[string]struct{})
	for rows.Next() {
		var source string
		var uid sql.NullString
		var seen sql.NullInt64
		var osName, hostname sql.NullString
		if err := rows.Scan(&source, &uid, &seen, &osName, &hostname); err != nil {
			rows.Close()
			return PresenceSummary{}, err
		}
		nativeSources[source] = struct{}{}
		if !uid.Valid || !seen.Valid || !canonicalUUID(uid.String) || observed-seen.Int64 >= presenceGoneAfter {
			continue
		}
		inventory := sourceInventory{}
		if osName.Valid {
			inventory.os = osName.String
			if inventory.os != "" {
				osSet[inventory.os] = struct{}{}
			}
		}
		if hostname.Valid {
			inventory.hostname = hostname.String
		}
		sources[source] = inventory
		machine := normalizedMachine(inventory.hostname)
		if machine == "" {
			machine = source
		}
		machines[machine] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return PresenceSummary{}, err
	}
	if err := rows.Close(); err != nil {
		return PresenceSummary{}, err
	}
	// Raw observations contribute machine inventory only when this source has
	// no native cursor. Keep expired native cursors in nativeSources so an old
	// raw heartbeat cannot make a superseded source look current again.
	rawRows, err := tx.QueryContext(ctx, `SELECT source,heartbeat_uid,heartbeat_seen_at,os,hostname
		FROM rawpresence_sources WHERE heartbeat_uid IS NOT NULL AND heartbeat_seen_at IS NOT NULL ORDER BY source`)
	if err != nil {
		return PresenceSummary{}, err
	}
	for rawRows.Next() {
		var source, uid string
		var seen int64
		var osName, hostname sql.NullString
		if err := rawRows.Scan(&source, &uid, &seen, &osName, &hostname); err != nil {
			rawRows.Close()
			return PresenceSummary{}, err
		}
		if _, native := nativeSources[source]; native || !canonicalUUID(uid) || observed-seen >= presenceGoneAfter {
			continue
		}
		inventory := sourceInventory{}
		if osName.Valid {
			inventory.os = osName.String
			if inventory.os != "" {
				osSet[inventory.os] = struct{}{}
			}
		}
		if hostname.Valid {
			inventory.hostname = hostname.String
		}
		sources[source] = inventory
		machine := normalizedMachine(inventory.hostname)
		if machine == "" {
			machine = source
		}
		machines[machine] = struct{}{}
	}
	if err := rawRows.Err(); err != nil {
		rawRows.Close()
		return PresenceSummary{}, err
	}
	if err := rawRows.Close(); err != nil {
		return PresenceSummary{}, err
	}

	active, idle := 0, 0
	host := normalizedMachine(hostname)
	var local *MachineAgentCounts
	if host != "" {
		local = &MachineAgentCounts{}
	}
	for _, agent := range agents {
		osSet[agent.Origin.OS] = struct{}{}
		switch agent.State {
		case "busy":
			active++
		case "idle":
			idle++
		}
		registeredHostname, err := presenceSummaryRegisteredHostname(ctx, tx, agent)
		if err != nil {
			return PresenceSummary{}, err
		}
		machine := ""
		if registeredHostname != nil {
			machine = normalizedMachine(*registeredHostname)
		}
		if machine == "" {
			machine = normalizedMachine(sources[agent.Source].hostname)
		}
		if machine == "" && agent.Hostname != nil {
			machine = normalizedMachine(*agent.Hostname)
		}
		if local != nil && machine == host {
			local.Agents++
			switch agent.State {
			case "busy":
				local.Active++
			case "idle":
				local.Idle++
			}
		}
		if machine == "" {
			machine = agent.Source
		}
		machines[machine] = struct{}{}
	}

	platforms := make([]string, 0, len(osSet))
	for platform := range osSet {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	if err := tx.Commit(); err != nil {
		return PresenceSummary{}, err
	}
	return PresenceSummary{Machines: len(machines), OS: platforms, Agents: len(agents), Active: active, Idle: idle, ThisMachine: local}, nil
}

func presenceSummaryRegisteredHostname(ctx context.Context, q queryer, agent AgentPresence) (*string, error) {
	candidate, err := rankedRegisteredHostCandidate(ctx, q, registeredHostRetainedTable,
		agent.Origin.SessionID, agent.Origin.InstanceID, agent.ProjectIdent)
	if err != nil {
		return nil, err
	}
	if candidate == nil {
		return nil, nil
	}
	return &candidate.host, nil
}

func normalizedMachine(host string) string {
	return strings.ToLower(strings.TrimSpace(host))
}
