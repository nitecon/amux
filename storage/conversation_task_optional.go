package storage

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
)

const conversationTaskOptionalMigration = userAccountsMigration + 1

var conversationTaskNotNull = regexp.MustCompile(`(?i)(\btask_id\s+TEXT)\s+NOT\s+NULL(\s+REFERENCES\s+tasks\s*\(\s*id\s*\))`)

// migrateLegacyConversationTaskOptional rebuilds only the retained legacy
// conversation table. It runs before Store.migrate because SQLite cannot alter
// a column's nullability in place.
func migrateLegacyConversationTaskOptional(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	var ddl sql.NullString
	err = conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='amux_conversations'`).Scan(&ddl)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if !ddl.Valid {
		return fmt.Errorf("amux_conversations has no stored schema")
	}
	notNull, err := conversationTaskIDNotNull(ctx, conn)
	if err != nil {
		return err
	}
	if !notNull {
		return nil
	}
	updated, count := replaceConversationTaskNullability(ddl.String)
	if count != 1 {
		return fmt.Errorf("unrecognized amux_conversations task_id declaration; refusing rebuild")
	}
	updated, err = replaceCreateTableName(updated, "amux_conversations", "amux_conversations_task_optional_rebuild")
	if err != nil {
		return err
	}
	return rebuildLegacyTable(ctx, conn, "amux_conversations", "amux_conversations_task_optional_rebuild", updated)
}

func replaceConversationTaskNullability(ddl string) (string, int) {
	indices := conversationTaskNotNull.FindAllStringSubmatchIndex(ddl, -1)
	if len(indices) != 1 {
		return ddl, len(indices)
	}
	match := indices[0]
	return ddl[:match[0]] + ddl[match[2]:match[3]] + ddl[match[4]:], 1
}

func conversationTaskIDNotNull(ctx context.Context, q queryer) (bool, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA table_info(amux_conversations)")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primary int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primary); err != nil {
			return false, err
		}
		if name == "task_id" {
			return notNull != 0, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, fmt.Errorf("amux_conversations is missing task_id")
}

// recordConversationTaskOptionalMigration validates both fresh and upgraded
// schemas before recording the migration in the normal startup transaction.
func recordConversationTaskOptionalMigration(ctx context.Context, tx *sql.Tx) error {
	notNull, err := conversationTaskIDNotNull(ctx, tx)
	if err != nil {
		return err
	}
	if notNull {
		return fmt.Errorf("amux_conversations.task_id is still NOT NULL")
	}
	_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)", conversationTaskOptionalMigration)
	return err
}
