package storage

import (
	"context"
	"database/sql"
	"fmt"
)

func migrateTaskWorkflow(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS task_confirmations(task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,token TEXT NOT NULL,origin_json TEXT NOT NULL,snapshot_json TEXT NOT NULL,request_json TEXT NOT NULL,mutated INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS task_reviews(source_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,source_done_at INTEGER NOT NULL,review_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,stage TEXT NOT NULL DEFAULT 'code',PRIMARY KEY(source_task_id,source_done_at,stage));
 CREATE TABLE IF NOT EXISTS task_review_outbox(source_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,source_done_at INTEGER NOT NULL,last_error TEXT NOT NULL DEFAULT '',attempts INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(source_task_id,source_done_at));`); err != nil {
		return err
	}
	var hasStage bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info('task_reviews') WHERE name='stage')").Scan(&hasStage); err != nil {
		return err
	}
	if !hasStage {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE task_reviews RENAME TO task_reviews_old;
  CREATE TABLE task_reviews(source_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,source_done_at INTEGER NOT NULL,review_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,stage TEXT NOT NULL DEFAULT 'code',PRIMARY KEY(source_task_id,source_done_at,stage));
  INSERT INTO task_reviews SELECT COALESCE(parent.source_task_id,r.source_task_id),COALESCE(parent.source_done_at,r.source_done_at),r.review_task_id,CASE WHEN EXISTS(SELECT 1 FROM json_each(t.labels) WHERE value='documentation-review') THEN 'documentation' ELSE 'code' END FROM task_reviews_old r JOIN tasks t ON t.id=r.review_task_id LEFT JOIN task_reviews_old parent ON parent.review_task_id=r.source_task_id;
  DROP TABLE task_reviews_old;`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS amux_review_rework(
		source_task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE, session_id TEXT NOT NULL)`); err != nil {
		return err
	}
	actor := "(SELECT origin_json FROM task_mutation_context WHERE id=1)"
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS task_event_completed AFTER UPDATE OF status ON tasks WHEN NEW.status='done' AND OLD.status!='done' BEGIN
 INSERT INTO task_events(kind,project_ident,canonical_remote,task_json,comment_json,created_at,origin_json)
 VALUES('task_completed',NEW.project_ident,(SELECT canonical_remote FROM projects WHERE ident=NEW.project_ident),%s,
 (SELECT %s FROM task_comments c WHERE c.task_id=NEW.id AND c.author_type!='system' ORDER BY c.created_at DESC,c.id DESC LIMIT 1),NEW.updated_at,%s);END;
 CREATE TRIGGER IF NOT EXISTS task_event_cancelled AFTER UPDATE OF status ON tasks WHEN NEW.status='cancelled' AND OLD.status!='cancelled' BEGIN
 INSERT INTO task_events(kind,project_ident,canonical_remote,task_json,created_at,origin_json) VALUES('task_cancelled',NEW.project_ident,(SELECT canonical_remote FROM projects WHERE ident=NEW.project_ident),%s,NEW.updated_at,%s);END;
 INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(4);`, taskSnapshotSQL("NEW"), commentSnapshotSQL("c"), actor, taskSnapshotSQL("NEW"), actor))
	return err
}
