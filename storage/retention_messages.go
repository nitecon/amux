package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// migrateRetentionMessages creates the Rust-compatible message retention schema
// and upgrades message tables that predate confirmation and author metadata.
func migrateRetentionMessages(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_ident TEXT NOT NULL REFERENCES projects(ident),
		source TEXT NOT NULL CHECK(source IN ('agent','user','system')),
		external_message_id TEXT,
		content TEXT NOT NULL,
		sent_at INTEGER NOT NULL,
		deliver_to_agents INTEGER NOT NULL DEFAULT 0,
		confirmed_at INTEGER,
		parent_message_id INTEGER,
		agent_id TEXT,
		message_type TEXT NOT NULL DEFAULT 'message',
		subject TEXT,
		hostname TEXT,
		event_at INTEGER,
		author_kind TEXT,
		resolved_at INTEGER,
		resolved_by TEXT
	)`); err != nil {
		return fmt.Errorf("create messages table: %w", err)
	}
	columns, err := retentionTableColumns(ctx, tx, "messages")
	if err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"confirmed_at", "INTEGER"},
		{"parent_message_id", "INTEGER"},
		{"agent_id", "TEXT"},
		{"message_type", "TEXT NOT NULL DEFAULT 'message'"},
		{"subject", "TEXT"},
		{"hostname", "TEXT"},
		{"event_at", "INTEGER"},
		{"deliver_to_agents", "INTEGER NOT NULL DEFAULT 0"},
		{"author_kind", "TEXT"},
		{"resolved_at", "INTEGER"},
		{"resolved_by", "TEXT"},
	} {
		if !columns[column.name] {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE messages ADD COLUMN "+column.name+" "+column.definition); err != nil {
				return fmt.Errorf("add messages.%s: %w", column.name, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cursors (
		project_ident TEXT PRIMARY KEY REFERENCES projects(ident),
		last_read_id INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create cursors table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET confirmed_at=sent_at
		WHERE confirmed_at IS NULL AND id <= (
			SELECT COALESCE(c.last_read_id, 0) FROM cursors c
			WHERE c.project_ident=messages.project_ident
		)`); err != nil {
		return fmt.Errorf("backfill message confirmations from cursors: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET author_kind=source
		WHERE author_kind IS NULL AND source IN ('agent','system')`); err != nil {
		return fmt.Errorf("backfill message author kinds: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS agent_confirmations (
		agent_id TEXT NOT NULL,
		project_ident TEXT NOT NULL,
		message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		confirmed_at INTEGER NOT NULL,
		PRIMARY KEY(agent_id, project_ident, message_id)
	)`); err != nil {
		return fmt.Errorf("create agent_confirmations table: %w", err)
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS idx_messages_project ON messages(project_ident, id)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_unconfirmed ON messages(project_ident, id) WHERE confirmed_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_agent_conf_project ON agent_confirmations(project_ident, message_id)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create retention index: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(23)"); err != nil {
		return fmt.Errorf("record message retention migration: %w", err)
	}
	return nil
}

const retentionMessageBatchSize = 200

// purgeRetentionMessagesIn removes eligible messages using the caller's
// transaction. The caller owns commit and rollback.
func purgeRetentionMessagesIn(ctx context.Context, tx *sql.Tx, cutoffMS, botCutoffMS int64) (int64, error) {
	clearDelegation, err := sqliteTableHasColumn(ctx, tx, "task_delegations", "completion_message_id")
	if err != nil {
		return 0, err
	}
	var deleted int64
	var afterID int64
	for {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		rows, err := tx.QueryContext(ctx, `SELECT m.id FROM messages m
			WHERE m.id > ? AND ((m.sent_at < ? AND (
				m.source IN ('agent','system') OR m.confirmed_at IS NOT NULL OR EXISTS (
					SELECT 1 FROM agent_confirmations ac
					WHERE ac.project_ident=m.project_ident AND ac.message_id=m.id
				)
			)) OR (m.sent_at < ? AND m.author_kind IN ('bot','webhook')))
			ORDER BY m.id LIMIT ?`, afterID, cutoffMS, botCutoffMS, retentionMessageBatchSize)
		if err != nil {
			return deleted, fmt.Errorf("select retention messages: %w", err)
		}
		ids := make([]int64, 0, retentionMessageBatchSize)
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return deleted, fmt.Errorf("scan retention message: %w", err)
			}
			ids = append(ids, id)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil {
			return deleted, fmt.Errorf("read retention messages: %w", rowsErr)
		}
		if closeErr != nil {
			return deleted, fmt.Errorf("close retention messages: %w", closeErr)
		}
		if len(ids) == 0 {
			return deleted, nil
		}
		afterID = ids[len(ids)-1]
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		marks, args := retentionIDArgs(ids)
		if clearDelegation {
			if _, err := tx.ExecContext(ctx, `UPDATE task_delegations SET completion_message_id=NULL WHERE completion_message_id IN (`+marks+")", args...); err != nil {
				return deleted, fmt.Errorf("clear delegation completion messages: %w", err)
			}
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id IN (`+marks+")", args...)
		if err != nil {
			return deleted, fmt.Errorf("delete retention messages: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return deleted, fmt.Errorf("count deleted retention messages: %w", err)
		}
		if count != int64(len(ids)) {
			return deleted, fmt.Errorf("delete retention messages: deleted %d of %d selected rows", count, len(ids))
		}
		deleted += count
	}
}

func retentionTableColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+quoteIdentifier(table)+")")
	if err != nil {
		return nil, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk); err != nil {
			_ = rows.Close()
			return nil, err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	return columns, rows.Close()
}

func retentionIDArgs(ids []int64) (string, []any) {
	marks := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return marks, args
}
