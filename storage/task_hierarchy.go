package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
)

var createdTaskSnapshotTables = []struct {
	table string
	kind  string
}{
	{table: "task_events", kind: "task_created"},
	{table: "router_task_outbox", kind: "created"},
}

func attachTaskParent(ctx context.Context, tx *sql.Tx, parent *Task, childID string) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO task_subtasks(parent_id,child_id) VALUES(?,?)", parent.ID, childID); err != nil {
		return err
	}
	return patchLatestCreatedTaskSnapshots(ctx, tx, childID, map[string]any{
		"parent": TaskParent{ID: parent.ID, ProjectIdent: parent.ProjectIdent, Title: parent.Title},
	})
}

func patchLatestCreatedTaskSnapshots(ctx context.Context, tx *sql.Tx, taskID string, fields map[string]any) error {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var set strings.Builder
	set.WriteString("task_json=json_set(task_json")
	args := make([]any, 0, len(keys)*2+2)
	for _, key := range keys {
		value, err := json.Marshal(fields[key])
		if err != nil {
			return err
		}
		set.WriteString(",?,json(?)")
		args = append(args, "$."+key, string(value))
	}
	set.WriteByte(')')
	for _, snapshot := range createdTaskSnapshotTables {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)", snapshot.table).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			continue
		}
		query := "UPDATE " + snapshot.table + " SET " + set.String() + " WHERE id=(SELECT id FROM " + snapshot.table + " WHERE kind=? AND json_extract(task_json,'$.id')=? ORDER BY id DESC LIMIT 1)"
		updateArgs := append(append([]any(nil), args...), snapshot.kind, taskID)
		if _, err := tx.ExecContext(ctx, query, updateArgs...); err != nil {
			return err
		}
	}
	return nil
}
