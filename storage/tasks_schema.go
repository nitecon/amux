package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func migrateTasks(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS tasks(
 id TEXT PRIMARY KEY,project_ident TEXT NOT NULL REFERENCES projects(ident),title TEXT NOT NULL,description TEXT,details TEXT,
 status TEXT NOT NULL DEFAULT 'todo' CHECK(status IN ('todo','in_progress','user_review','done','cancelled')),rank INTEGER NOT NULL DEFAULT 0,
 labels TEXT,hostname TEXT,owner_agent_id TEXT,reporter TEXT NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,started_at INTEGER,done_at INTEGER);
 CREATE TABLE IF NOT EXISTS task_comments(id TEXT PRIMARY KEY,task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,author TEXT NOT NULL,
 author_type TEXT NOT NULL CHECK(author_type IN ('agent','user','system')),content TEXT NOT NULL,created_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS idx_tasks_project_status ON tasks(project_ident,status);
 CREATE INDEX IF NOT EXISTS idx_tasks_project_rank ON tasks(project_ident,status,rank);
 CREATE INDEX IF NOT EXISTS idx_task_comments_task ON task_comments(task_id,created_at);
 CREATE TABLE IF NOT EXISTS task_subtasks(parent_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,child_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,CHECK(parent_id!=child_id));
 CREATE INDEX IF NOT EXISTS task_subtasks_parent ON task_subtasks(parent_id);
 CREATE TABLE IF NOT EXISTS task_completion_owners(task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,origin_json TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS task_mutation_context(id INTEGER PRIMARY KEY CHECK(id=1),origin_json TEXT);
 INSERT OR IGNORE INTO task_mutation_context(id) VALUES(1);`)
	if err != nil {
		return err
	}
	for _, column := range []struct{ table, name, definition string }{
		{"tasks", "kind", "TEXT NOT NULL DEFAULT 'normal'"}, {"tasks", "delegated_to_project_ident", "TEXT"}, {"tasks", "delegated_to_task_id", "TEXT"},
		{"tasks", "owner_origin_json", "TEXT"}, {"tasks", "goal", "TEXT"}, {"tasks", "routing_json", `TEXT NOT NULL DEFAULT '{"mode":"auto"}'`},
		{"tasks", "requester_json", "TEXT"}, {"tasks", "review_action", "TEXT"}, {"tasks", "task_type", "TEXT NOT NULL DEFAULT 'software' CHECK(task_type IN ('general','software'))"},
		{"tasks", "changeset_json", "TEXT"}, {"task_comments", "origin_json", "TEXT"}, {"task_comments", "hostname", "TEXT"},
	} {
		var present bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name=?)", column.table, column.name).Scan(&present); err != nil {
			return err
		}
		if !present {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE "+column.table+" ADD COLUMN "+column.name+" "+column.definition); err != nil {
				return err
			}
		}
	}
	// Preserve installed Rust triggers. For fresh Go profiles these ordinary-task
	// triggers keep snapshots and actors atomic even if a later statement fails.
	actor := "(SELECT origin_json FROM task_mutation_context WHERE id=1)"
	sql := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS task_event_created AFTER INSERT ON tasks BEGIN
 INSERT INTO task_events(kind,project_ident,canonical_remote,task_json,created_at,origin_json)
 VALUES('task_created',NEW.project_ident,(SELECT canonical_remote FROM projects WHERE ident=NEW.project_ident),%s,NEW.created_at,%s);END;
 CREATE TRIGGER IF NOT EXISTS task_event_commented AFTER INSERT ON task_comments BEGIN
 INSERT INTO task_events(kind,project_ident,canonical_remote,task_json,comment_json,created_at,origin_json)
 SELECT 'task_commented',t.project_ident,p.canonical_remote,%s,%s,NEW.created_at,%s FROM tasks t JOIN projects p ON p.ident=t.project_ident WHERE t.id=NEW.task_id;END;
 CREATE TRIGGER IF NOT EXISTS amux_task_event_user_review AFTER UPDATE OF status ON tasks
 WHEN NEW.status='user_review' AND OLD.status!='user_review' BEGIN
 INSERT INTO task_events(kind,project_ident,canonical_remote,task_json,created_at,origin_json)
 VALUES('task_user_review',NEW.project_ident,(SELECT canonical_remote FROM projects WHERE ident=NEW.project_ident),%s,NEW.updated_at,%s);END;
 CREATE TRIGGER IF NOT EXISTS task_completion_owner_park BEFORE UPDATE OF status ON tasks
 WHEN NEW.status='user_review' AND OLD.status!='user_review' AND OLD.owner_origin_json IS NOT NULL BEGIN
 INSERT OR REPLACE INTO task_completion_owners(task_id,origin_json) VALUES(OLD.id,OLD.owner_origin_json);END;
 CREATE TRIGGER IF NOT EXISTS task_completion_owner_clear AFTER UPDATE OF status ON tasks WHEN NEW.status!='user_review' BEGIN DELETE FROM task_completion_owners WHERE task_id=NEW.id;END;`, taskSnapshotSQL("NEW"), actor, taskSnapshotSQL("t"), commentSnapshotSQL("NEW"), actor, taskSnapshotSQL("NEW"), actor)
	if _, err := tx.ExecContext(ctx, sql); err != nil {
		return err
	}
	// Dispatch-specific unclaim suppression is preserved when importing its Rust
	// trigger. New Go profiles cannot create dispatch tasks until that port lands.
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS task_event_unclaimed AFTER UPDATE OF status,owner_agent_id,owner_origin_json ON tasks
 WHEN NEW.status='todo' AND NEW.owner_agent_id IS NULL AND NEW.owner_origin_json IS NULL
 AND (OLD.owner_agent_id IS NOT NULL OR OLD.owner_origin_json IS NOT NULL OR OLD.status='in_progress') BEGIN
 INSERT INTO task_events(kind,project_ident,canonical_remote,task_json,comment_json,created_at,origin_json)
 VALUES('task_unclaimed',NEW.project_ident,(SELECT canonical_remote FROM projects WHERE ident=NEW.project_ident),%s,
 (SELECT %s FROM task_comments c WHERE c.task_id=NEW.id ORDER BY c.created_at DESC,c.id DESC LIMIT 1),NEW.updated_at,%s);END;
 INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(3);`, taskSnapshotSQL("NEW"), commentSnapshotSQL("c"), actor))
	return err
}

// Aliases are private compile-time identifiers, never request data.
func taskSnapshotSQL(alias string) string {
	fields := []string{"id", "project_ident", "title", "goal", "description", "review_action", "details", "status", "rank", "hostname", "owner_agent_id", "reporter", "created_at", "updated_at", "started_at", "done_at", "kind", "delegated_to_project_ident", "delegated_to_task_id"}
	pairs := make([]string, 0, len(fields)+7)
	for _, field := range fields {
		pairs = append(pairs, "'"+field+"',"+alias+"."+field)
	}
	for _, field := range []struct{ wire, column string }{{"changeset", "changeset_json"}, {"owner_origin", "owner_origin_json"}, {"routing", "routing_json"}, {"requester", "requester_json"}} {
		pairs = append(pairs, "'"+field.wire+"',json("+alias+"."+field.column+")")
	}
	pairs = append(pairs, "'labels',json(CASE WHEN json_valid("+alias+".labels) THEN "+alias+".labels ELSE '[]' END)")
	pairs = append(pairs, "'delegation_ready',json(CASE WHEN "+alias+".kind='delegated' AND EXISTS(SELECT 1 FROM task_delegations d JOIN tasks target ON target.id=d.target_task_id AND target.project_ident=d.target_project_ident WHERE d.source_project_ident="+alias+".project_ident AND d.source_task_id="+alias+".id AND target.status='done') THEN 'true' ELSE 'false' END)")
	pairs = append(pairs, "'parent',(SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title) FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id="+alias+".id)")
	return "json_object(" + strings.Join(pairs, ",") + ")"
}
func commentSnapshotSQL(alias string) string {
	fields := []string{"id", "task_id", "author", "author_type", "content", "created_at"}
	pairs := make([]string, 0, len(fields)+1)
	for _, field := range fields {
		pairs = append(pairs, "'"+field+"',"+alias+"."+field)
	}
	pairs = append(pairs, "'origin',json("+alias+".origin_json)")
	return "json_object(" + strings.Join(pairs, ",") + ")"
}
