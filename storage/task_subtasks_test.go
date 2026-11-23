package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestCreateTaskSubtaskSameAndCrossProjectSnapshots(t *testing.T) {
	store := taskStore(t)
	ctx := context.Background()
	if err := store.InsertProject(ctx, fixtureProject("other")); err != nil {
		t.Fatal(err)
	}
	goal := "release"
	parent, err := store.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Parent", Reporter: "user", Goal: &goal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wake := store.TaskEventChanges()
	same, err := store.CreateTaskSubtask(ctx, TaskSubtaskInsert{
		ParentProject: "tasks", ParentID: parent.ID, Title: " Same child ", Reporter: "user",
	}, nil)
	if err != nil || same.Parent == nil || same.Parent.ID != parent.ID || same.Goal == nil || *same.Goal != goal {
		t.Fatalf("same-project child=%+v err=%v", same, err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("committed subtask did not publish its task event")
	}
	var createdSnapshot string
	if err := store.db.QueryRowContext(ctx, `SELECT task_json FROM task_events WHERE kind='task_created' AND json_extract(task_json,'$.id')=? ORDER BY id DESC LIMIT 1`, same.ID).Scan(&createdSnapshot); err != nil {
		t.Fatal(err)
	}
	var snapshot Task
	if err := json.Unmarshal([]byte(createdSnapshot), &snapshot); err != nil || snapshot.Parent == nil || snapshot.Parent.ProjectIdent != "tasks" {
		t.Fatalf("task event omitted parent at publication: %+v err=%v", snapshot, err)
	}
	var outboxSnapshot string
	if err := store.db.QueryRowContext(ctx, `SELECT task_json FROM router_task_outbox WHERE kind='created' AND json_extract(task_json,'$.id')=? ORDER BY id DESC LIMIT 1`, same.ID).Scan(&outboxSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(outboxSnapshot), &snapshot); err != nil || snapshot.Parent == nil || snapshot.Parent.ID != parent.ID {
		t.Fatalf("router outbox omitted parent at publication: %+v err=%v", snapshot, err)
	}

	target := "other"
	cross, err := store.CreateTaskSubtask(ctx, TaskSubtaskInsert{
		ParentProject: "tasks", ParentID: parent.ID, TargetProject: &target, Title: "Cross child", Reporter: "user",
	}, nil)
	if err != nil || cross.ProjectIdent != target || cross.Goal != nil || cross.Parent == nil || cross.Parent.ProjectIdent != "tasks" {
		t.Fatalf("cross-project child=%+v err=%v", cross, err)
	}
	children, err := store.TaskSubtasks(ctx, "tasks", parent.ID)
	if err != nil || len(children) != 2 {
		t.Fatalf("subtasks=%+v err=%v", children, err)
	}
	if children[0].CreatedAt > children[1].CreatedAt || (children[0].CreatedAt == children[1].CreatedAt && children[0].ID > children[1].ID) {
		t.Fatalf("subtasks are not ordered by creation and ID: %+v", children)
	}
	if _, err := store.TaskSubtasks(ctx, "other", parent.ID); !errors.Is(err, ErrSubtaskParentNotFound) {
		t.Fatalf("parent lookup escaped its project: %v", err)
	}
}

func TestCreateTaskSubtaskRejectsMissingOrArchivedTargetsAtomically(t *testing.T) {
	store := taskStore(t)
	ctx := context.Background()
	parent := makeTask(t, store, nil)
	beforeTasks, beforeEvents := taskCounts(t, store)
	missing := "absent"
	if _, err := store.CreateTaskSubtask(ctx, TaskSubtaskInsert{ParentProject: "tasks", ParentID: parent.ID, TargetProject: &missing, Title: "Missing", Reporter: "user"}, nil); !errors.Is(err, ErrSubtaskTargetNotFound) {
		t.Fatalf("missing target error=%v", err)
	}
	if err := store.InsertProject(ctx, fixtureProject("archived")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetProjectArchived(ctx, "archived", true); err != nil {
		t.Fatal(err)
	}
	archived := "archived"
	if _, err := store.CreateTaskSubtask(ctx, TaskSubtaskInsert{ParentProject: "tasks", ParentID: parent.ID, TargetProject: &archived, Title: "Archived", Reporter: "user"}, nil); !errors.Is(err, ErrSubtaskTargetArchived) {
		t.Fatalf("archived target error=%v", err)
	}
	afterTasks, afterEvents := taskCounts(t, store)
	if afterTasks != beforeTasks || afterEvents != beforeEvents {
		t.Fatalf("failed creation leaked task/event: tasks %d->%d events %d->%d", beforeTasks, afterTasks, beforeEvents, afterEvents)
	}
}

func taskCounts(t *testing.T, store *Store) (int64, int64) {
	t.Helper()
	var tasks, events int64
	if err := store.db.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT COUNT(*) FROM task_events").Scan(&events); err != nil {
		t.Fatal(err)
	}
	return tasks, events
}
