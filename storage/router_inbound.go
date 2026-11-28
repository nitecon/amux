package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
)

const routerInboundMigration = 28

// migrateRouterInbound adds the native inbound checkpoint without changing any
// existing Rust-owned table or row.
func migrateRouterInbound(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS inbound_cursors (
		project TEXT PRIMARY KEY REFERENCES projects(ident) ON DELETE CASCADE,
		event_seq INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create inbound_cursors: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_messages_project_external_id
		ON messages(project_ident,external_message_id)`); err != nil {
		return fmt.Errorf("index inbound external message lookup: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)", routerInboundMigration); err != nil {
		return fmt.Errorf("record inbound migration: %w", err)
	}
	return nil
}

// IngestRouterInbound applies one inbound event atomically. It returns stop for
// an unknown room, allowing the durable router delivery to become stopped.
func (s *Store) IngestRouterInbound(ctx context.Context, event fism.Event) (stop bool, err error) {
	if event.ID != "channel.message_received" || event.Version != 1 {
		return false, fmt.Errorf("unsupported inbound event %q v%d", event.ID, event.Version)
	}
	payload, err := controllers.DecodeInboundPayload(event.Payload)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var project string
	err = tx.QueryRowContext(ctx, `SELECT ident FROM projects WHERE channel_name=? AND room_id=?`,
		payload.ChannelName, payload.RoomID).Scan(&project)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("resolve inbound room: %w", err)
	}

	// Provider redeliveries carry a new journal sequence. Check the complete
	// retained message history before touching either checkpoint or project head.
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages
		WHERE project_ident=? AND external_message_id=?)`, project, payload.Message.ID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check inbound external message id: %w", err)
	}
	if exists != 0 {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO messages(
		project_ident,source,external_message_id,content,sent_at,confirmed_at,deliver_to_agents,message_type,author_kind
	) VALUES(?,'user',?,?,?,NULL,?,'message',?)`, project, payload.Message.ID, payload.Message.Content,
		event.EmittedAt, false, payload.Message.AuthorKind); err != nil {
		return false, fmt.Errorf("insert inbound message: %w", err)
	}

	var latest sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT event_seq FROM inbound_cursors WHERE project=?", project).Scan(&latest); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read inbound cursor: %w", err)
	}
	if !latest.Valid || event.Seq > latest.Int64 {
		if _, err := tx.ExecContext(ctx, "UPDATE projects SET last_msg_id=? WHERE ident=?", payload.Message.ID, project); err != nil {
			return false, fmt.Errorf("update inbound project message head: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO inbound_cursors(project,event_seq) VALUES(?,?)
			ON CONFLICT(project) DO UPDATE SET event_seq=excluded.event_seq`, project, event.Seq); err != nil {
			return false, fmt.Errorf("advance inbound cursor: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	_ = s.bus.Publish(eventbus.Event{Topic: eventbus.ProjectChanged})
	return false, nil
}
