package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrateRetentionMessagesBackfillsLegacyCursorAndPreservesData(t *testing.T) {
	db := retentionTestDB(t)
	_, err := db.Exec(`CREATE TABLE projects(ident TEXT PRIMARY KEY, channel_name TEXT NOT NULL, room_id TEXT NOT NULL, last_msg_id TEXT, created_at INTEGER NOT NULL);
		INSERT INTO projects VALUES('p','chan','room',NULL,1);
		CREATE TABLE messages(id INTEGER PRIMARY KEY AUTOINCREMENT, project_ident TEXT NOT NULL, source TEXT NOT NULL, external_message_id TEXT, content TEXT NOT NULL, sent_at INTEGER NOT NULL);
		CREATE INDEX existing_messages_content ON messages(content);
		CREATE TABLE cursors(project_ident TEXT PRIMARY KEY, last_read_id INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL);
		INSERT INTO cursors VALUES('p',1,9);
		INSERT INTO messages(project_ident,source,external_message_id,content,sent_at) VALUES('p','user','ext-1','read',10),('p','agent',NULL,'agent',20),('p','system',NULL,'system',30);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateRetentionTestDB(t, db); err != nil {
		t.Fatal(err)
	}
	assertRetentionValue(t, db, "SELECT confirmed_at FROM messages WHERE id=1", int64(10))
	assertRetentionNull(t, db, "SELECT confirmed_at FROM messages WHERE id=2")
	assertRetentionValue(t, db, "SELECT author_kind FROM messages WHERE id=2", "agent")
	assertRetentionValue(t, db, "SELECT author_kind FROM messages WHERE id=3", "system")
	assertRetentionValue(t, db, "SELECT content FROM messages WHERE id=1", "read")
	assertRetentionValue(t, db, "SELECT external_message_id FROM messages WHERE id=1", "ext-1")
	assertRetentionValue(t, db, "SELECT last_read_id FROM cursors WHERE project_ident='p'", int64(1))
	assertRetentionValue(t, db, "SELECT count(*) FROM sqlite_master WHERE type='index' AND name='existing_messages_content'", int64(1))
	assertRetentionValue(t, db, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='agents'", int64(0))
	assertRetentionValue(t, db, "SELECT count(*) FROM pragma_foreign_key_list('agent_confirmations') WHERE "+"\"table\"='messages' AND on_delete='CASCADE'", int64(1))
	assertRetentionValue(t, db, "SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_messages_unconfirmed'", int64(1))
	assertRetentionValue(t, db, "SELECT count(*) FROM amux_go_migrations WHERE version=23", int64(1))

	// Acknowledgements are owned by the message and disappear through the FK.
	_, err = db.Exec("INSERT INTO agent_confirmations VALUES('agent-a','p',2,40)")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("DELETE FROM messages WHERE id=2"); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertRetentionValue(t, db, "SELECT count(*) FROM agent_confirmations WHERE message_id=2", int64(0))
}

func TestPurgeRetentionMessagesPolicyAndIndependentCutoffs(t *testing.T) {
	db := retentionFreshDB(t)
	ids := map[string]int64{}
	insert := func(name, source, author string, sentAt int64, globallyConfirmed bool) {
		t.Helper()
		var authorValue any
		if author != "" {
			authorValue = author
		}
		var confirmed any
		if globallyConfirmed {
			confirmed = sentAt + 1
		}
		result, err := db.Exec(`INSERT INTO messages(project_ident,source,content,sent_at,author_kind,confirmed_at)
			VALUES('p',?,'body',?,?,?)`, source, sentAt, authorValue, confirmed)
		if err != nil {
			t.Fatal(err)
		}
		ids[name], err = result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("human_open", "user", "human", 100, false)
	insert("human_ack", "user", "human", 100, false)
	insert("agent", "agent", "agent", 100, false)
	insert("system", "system", "system", 100, false)
	insert("bot", "user", "bot", 199, false)
	insert("webhook", "user", "webhook", 150, false)
	insert("human_boundary", "user", "human", 500, true)
	insert("bot_boundary", "user", "bot", 200, false)
	if _, err := db.Exec("INSERT INTO agent_confirmations VALUES('different-agent','p',?,101)", ids["human_ack"]); err != nil {
		t.Fatal(err)
	}
	deleted, err := runRetentionPurge(t, db, 500, 200)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 5 {
		t.Fatalf("deleted %d, want 5", deleted)
	}
	for _, name := range []string{"human_open", "human_boundary", "bot_boundary"} {
		var count int64
		if err := db.QueryRow("SELECT count(*) FROM messages WHERE id=?", ids[name]).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s retained count=%d err=%v", name, count, err)
		}
	}
	for _, name := range []string{"human_ack", "agent", "system", "bot", "webhook"} {
		var count int64
		if err := db.QueryRow("SELECT count(*) FROM messages WHERE id=?", ids[name]).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s remaining count=%d err=%v", name, count, err)
		}
	}
	var confirmations int64
	if err := db.QueryRow("SELECT count(*) FROM agent_confirmations WHERE message_id=?", ids["human_ack"]).Scan(&confirmations); err != nil || confirmations != 0 {
		t.Fatalf("acknowledgement cascade count=%d err=%v", confirmations, err)
	}
}

func TestPurgeRetentionMessagesClearsLegacyRestrictDelegationReference(t *testing.T) {
	db := retentionFreshDB(t)
	if _, err := db.Exec(`CREATE TABLE task_delegations(
		id TEXT PRIMARY KEY, completed_at INTEGER, completion_message_id INTEGER REFERENCES messages(id));`); err != nil {
		t.Fatal(err)
	}
	result, err := db.Exec("INSERT INTO messages(project_ident,source,content,sent_at) VALUES('p','agent','done',1)")
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO task_delegations VALUES('d',77,?)", messageID); err != nil {
		t.Fatal(err)
	}
	deleted, err := runRetentionPurge(t, db, 2, 2)
	if err != nil || deleted != 1 {
		t.Fatalf("purge deleted=%d err=%v", deleted, err)
	}
	assertRetentionNull(t, db, "SELECT completion_message_id FROM task_delegations WHERE id='d'")
	assertRetentionValue(t, db, "SELECT completed_at FROM task_delegations WHERE id='d'", int64(77))
}

func TestPurgeRetentionMessagesLaterBatchFailureRollsBackWithCaller(t *testing.T) {
	db := retentionFreshDB(t)
	if _, err := db.Exec("INSERT INTO messages(project_ident,source,content,sent_at) VALUES('p','agent','old-0',1)"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < retentionMessageBatchSize+1; i++ {
		if _, err := db.Exec("INSERT INTO messages(project_ident,source,content,sent_at) VALUES('p','agent',?,1)", fmt.Sprintf("old-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_later_message_batch BEFORE DELETE ON messages
		WHEN OLD.id > 200 BEGIN SELECT RAISE(ABORT, 'injected later-batch failure'); END`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := purgeRetentionMessagesIn(context.Background(), tx, 2, 2)
	if err == nil || !strings.Contains(err.Error(), "injected later-batch failure") {
		_ = tx.Rollback()
		t.Fatalf("expected injected failure after first batch, deleted=%d err=%v", deleted, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertRetentionValue(t, db, "SELECT count(*) FROM messages", int64(retentionMessageBatchSize+1))
}

func TestPurgeRetentionMessagesIgnoredDeleteRollsBackWithCaller(t *testing.T) {
	db := retentionFreshDB(t)
	for i := 0; i < retentionMessageBatchSize+1; i++ {
		if _, err := db.Exec("INSERT INTO messages(project_ident,source,content,sent_at) VALUES('p','agent',?,1)", fmt.Sprintf("old-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`CREATE TRIGGER ignore_last_retention_delete BEFORE DELETE ON messages
		WHEN OLD.id=201 BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := purgeRetentionMessagesIn(context.Background(), tx, 2, 2)
	if err == nil || !strings.Contains(err.Error(), "deleted 0 of 1 selected rows") {
		_ = tx.Rollback()
		t.Fatalf("expected ignored-delete mismatch after first batch, deleted=%d err=%v", deleted, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertRetentionValue(t, db, "SELECT count(*) FROM messages", int64(retentionMessageBatchSize+1))
}

func TestPurgeRetentionMessagesHonorsCanceledContext(t *testing.T) {
	db := retentionFreshDB(t)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = purgeRetentionMessagesIn(ctx, tx, 10, 10)
	if !errors.Is(err, context.Canceled) {
		_ = tx.Rollback()
		t.Fatalf("got %v, want context canceled", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func retentionFreshDB(t *testing.T) *sql.DB {
	t.Helper()
	db := retentionTestDB(t)
	_, err := db.Exec(`CREATE TABLE projects(ident TEXT PRIMARY KEY, channel_name TEXT NOT NULL, room_id TEXT NOT NULL, last_msg_id TEXT, created_at INTEGER NOT NULL);
		INSERT INTO projects VALUES('p','chan','room',NULL,1);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateRetentionTestDB(t, db); err != nil {
		t.Fatal(err)
	}
	return db
}

func retentionTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE amux_go_migrations(version INTEGER PRIMARY KEY)"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func migrateRetentionTestDB(t *testing.T, db *sql.DB) error {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if err := migrateRetentionMessages(context.Background(), tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func runRetentionPurge(t *testing.T, db *sql.DB, cutoff, botCutoff int64) (int64, error) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return 0, err
	}
	deleted, err := purgeRetentionMessagesIn(context.Background(), tx, cutoff, botCutoff)
	if err != nil {
		_ = tx.Rollback()
		return deleted, err
	}
	if err := tx.Commit(); err != nil {
		return deleted, err
	}
	return deleted, nil
}

func assertRetentionValue(t *testing.T, db *sql.DB, query string, want any, args ...any) {
	t.Helper()
	var got any
	if err := db.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if got != want {
		t.Fatalf("%s = %#v, want %#v", query, got, want)
	}
}

func assertRetentionNull(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	var value sql.NullInt64
	if err := db.QueryRow(query).Scan(&value); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if value.Valid {
		t.Fatalf("%s = %d, want NULL", query, value.Int64)
	}
}
