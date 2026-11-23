package storage

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func taskStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	if err := s.InsertProject(context.Background(), fixtureProject("tasks")); err != nil {
		t.Fatal(err)
	}
	return s
}
func taskActor() *AgentOrigin {
	return &AgentOrigin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", OS: "linux"}
}
func makeTask(t *testing.T, s *Store, origin *AgentOrigin) *Task {
	t.Helper()
	task, err := s.InsertTask(context.Background(), TaskInsert{Project: "tasks", Title: "Work", Reporter: "fixture"}, origin)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestTaskCreationReservationAndSnapshots(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	actor := taskActor()
	wake := s.TaskEventChanges()
	task := makeTask(t, s, actor)
	if task.Status != "todo" || task.Rank != 1 || task.TaskType != "general" || task.OwnerOrigin == nil || *task.OwnerOrigin != *actor || task.StartedAt != nil {
		t.Fatalf("creation %+v", task)
	}
	select {
	case <-wake:
	default:
		t.Fatal("task commit did not announce event")
	}
	events, err := s.TaskEvents(ctx, EventQuery{})
	if err != nil || len(events) != 1 || events[0].Kind != "task_created" || events[0].Origin == nil || *events[0].Origin != *actor {
		t.Fatalf("events %+v, %v", events, err)
	}
	var snapshot Task
	if err := json.Unmarshal(events[0].Task, &snapshot); err != nil || snapshot.ID != task.ID || snapshot.OwnerOrigin == nil {
		t.Fatalf("snapshot %+v, %v", snapshot, err)
	}
	opposite, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Windows only", Reporter: "fixture", Labels: []string{"os:windows"}}, actor)
	if err != nil || opposite.OwnerOrigin != nil || opposite.OwnerAgentID != nil || opposite.Rank != 2 {
		t.Fatalf("wrong-OS reservation %+v, %v", opposite, err)
	}
}

// Claim/release portion of Rust task_events.rs::exact_owner_session_and_instance_fence_shared_machine_and_legacy.
func TestExactTaskClaimAndRelease(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	task := makeTask(t, s, nil)
	actor := taskActor()
	claimed, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", actor)
	if err != nil || claimed.Status != "in_progress" || claimed.StartedAt == nil || claimed.OwnerOrigin == nil || *claimed.OwnerOrigin != *actor {
		t.Fatalf("claim %+v, %v", claimed, err)
	}
	for _, change := range []string{"session", "instance"} {
		other := *actor
		if change == "session" {
			other.SessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		} else {
			other.InstanceID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		}
		for _, status := range []string{"in_progress", "todo"} {
			if _, err := s.TransitionTask(ctx, "tasks", task.ID, status, &other); !errors.Is(err, ErrTaskOwnership) {
				t.Fatalf("%s changed %s: %v", change, status, err)
			}
		}
	}
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "todo", nil); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("anonymous release: %v", err)
	}
	released, err := s.TransitionTask(ctx, "tasks", task.ID, "todo", actor)
	if err != nil || released.OwnerOrigin != nil || released.OwnerAgentID != nil || released.StartedAt != nil {
		t.Fatalf("release %+v, %v", released, err)
	}
	events, err := s.TaskEvents(ctx, EventQuery{})
	if err != nil || len(events) != 2 || events[0].Kind != "task_unclaimed" {
		t.Fatalf("release event %+v, %v", events, err)
	}
}

func TestTaskCommentsKeepExactActorAndCapUnicode(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	task := makeTask(t, s, nil)
	actor := taskActor()
	comment, err := s.AddTaskComment(ctx, "tasks", task.ID, "worker", "agent", "Updated", nil, actor)
	if err != nil || comment.Origin == nil || *comment.Origin != *actor {
		t.Fatalf("comment %+v, %v", comment, err)
	}
	detail, err := s.Task(ctx, "TASKS", task.ID)
	if err != nil || detail == nil || len(detail.Comments) != 1 || detail.Task.OwnerOrigin != nil {
		t.Fatalf("detail %+v, %v", detail, err)
	}
	events, err := s.TaskEvents(ctx, EventQuery{})
	if err != nil || events[0].Kind != "task_commented" || events[0].Comment == nil {
		t.Fatalf("comment event %+v, %v", events, err)
	}
	if _, err := s.AddTaskComment(ctx, "other", task.ID, "worker", "agent", "cross-project", nil, actor); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("cross-project comment %v", err)
	}
	if _, err := s.AddTaskComment(ctx, "tasks", task.ID, "worker", "robot", "bad", nil, actor); err == nil {
		t.Fatal("invalid author accepted")
	}
}

func TestTaskMutationRollsBackJournalAndOrigin(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	actor := taskActor()
	wake := s.TaskEventChanges()
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fixture_fail_task AFTER INSERT ON tasks BEGIN SELECT RAISE(ABORT,'fixture'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Rolled back", Reporter: "fixture"}, actor); err == nil {
		t.Fatal("failing insertion committed")
	}
	var events, tasks int
	var origin *string
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_events").Scan(&events)
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks").Scan(&tasks)
	if err := s.db.QueryRowContext(ctx, "SELECT origin_json FROM task_mutation_context WHERE id=1").Scan(&origin); err != nil || origin != nil || events != 0 || tasks != 0 {
		t.Fatalf("rollback leaked actor/state: %v, %d, %d, %v", origin, events, tasks, err)
	}
	select {
	case <-wake:
		t.Fatal("rollback woke subscribers")
	default:
	}
}

func TestTaskGoalAndReleaseConfirmationAreTransactional(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	goal := "release"
	task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Goal work", Reporter: "fixture", Goal: &goal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := taskActor()
	if _, err := s.ApplyTaskTransition(ctx, "tasks", task.ID, "in_progress", actor, TaskTransitionPolicy{CheckGoal: true, SelectedGoal: "other"}); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("wrong goal claimed: %v", err)
	}
	if _, err := s.ApplyTaskTransition(ctx, "tasks", task.ID, "in_progress", actor, TaskTransitionPolicy{CheckGoal: true, SelectedGoal: goal}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyTaskTransition(ctx, "tasks", task.ID, "todo", actor, TaskTransitionPolicy{RequireChangeset: true}); !errors.Is(err, ErrTaskChangeset) {
		t.Fatalf("missing declaration released: %v", err)
	}
	detail, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || detail.Task.Status != "in_progress" {
		t.Fatalf("failed declaration changed state: %+v, %v", detail, err)
	}
	if _, err := s.ApplyTaskTransition(ctx, "tasks", task.ID, "todo", actor, TaskTransitionPolicy{RequireChangeset: true, ConfirmedNoChanges: true}); err != nil {
		t.Fatal(err)
	}
}

// Rust db.rs::task_goal_names_normalize_and_fit_session_context.
func TestTaskGoalNames(t *testing.T) {
	for _, value := range []string{"  release  ", "  ", strings.Repeat("é", 64)} {
		goal, err := NormalizeTaskGoal(&value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(value) == "" && goal != nil {
			t.Fatal("blank goal retained")
		}
	}
	for _, value := range []string{strings.Repeat("é", 65), "release\nother"} {
		if _, err := NormalizeTaskGoal(&value); err == nil {
			t.Fatal("invalid goal accepted")
		}
	}
}

func TestConcurrentTaskClaimsHaveOneOwner(t *testing.T) {
	s := taskStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	task := makeTask(t, s, nil)
	first := taskActor()
	second := *first
	second.SessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, actor := range []*AgentOrigin{first, &second} {
		go func(actor *AgentOrigin) {
			<-start
			_, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", actor)
			results <- err
		}(actor)
	}
	close(start)
	won, lost := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			won++
		} else if errors.Is(err, ErrTaskOwnership) {
			lost++
		} else {
			t.Fatalf("claim failed unexpectedly: %v", err)
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("claims: %d winners, %d ownership conflicts", won, lost)
	}
}

func TestSubtaskSnapshotAndProjectScope(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	parent := makeTask(t, s, nil)
	child, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Child", Reporter: "fixture", ParentID: &parent.ID}, taskActor())
	if err != nil || child.Parent == nil || child.Parent.ID != parent.ID {
		t.Fatalf("child %+v, %v", child, err)
	}
	events, err := s.TaskEvents(ctx, EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Task
	if err := json.Unmarshal(events[0].Task, &snapshot); err != nil || snapshot.Parent == nil || snapshot.Parent.ID != parent.ID {
		t.Fatalf("parent not published atomically: %+v, %v", snapshot, err)
	}
	if err := s.InsertProject(ctx, fixtureProject("other")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertTask(ctx, TaskInsert{Project: "other", Title: "Wrong project", Reporter: "fixture", ParentID: &parent.ID}, nil); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("cross-project parent accepted: %v", err)
	}
}

func TestLegacyClaimCannotBeTakenByMachineLabel(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	task := makeTask(t, s, nil)
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET owner_agent_id='shared-machine',owner_origin_json=NULL,status='in_progress' WHERE id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "todo", taskActor()); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("legacy claim bypassed: %v", err)
	}
}

func TestTaskCommentUnicodeLimit(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	task := makeTask(t, s, nil)
	comment, err := s.AddTaskComment(ctx, "tasks", task.ID, "worker", "agent", strings.Repeat("🦀", 20000), nil, taskActor())
	if err != nil || len(comment.Content) > 65536 || !utf8.ValidString(comment.Content) || !strings.HasSuffix(comment.Content, "\n…[truncated]") {
		t.Fatalf("comment not bounded: %v", err)
	}
	host := "bad\nhost"
	if _, err := s.AddTaskComment(ctx, "tasks", task.ID, "worker", "agent", "bad host", &host, taskActor()); err == nil {
		t.Fatal("invalid hostname accepted")
	}
	detail, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || len(detail.Comments) != 1 {
		t.Fatalf("failed comment persisted: %+v, %v", detail, err)
	}
}

// Rust db.rs::task_detail_recent_keeps_newest_comments_in_order_with_full_count.
func TestTaskRecentCommentsPreserveHistoryAndOrder(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	task := makeTask(t, s, nil)
	large := strings.Repeat("x", 80000)
	for _, fixture := range []struct{ id, content string }{{"z-first", "First"}, {"a-second", large}, {"m-third", "Third"}} {
		if _, err := s.db.ExecContext(ctx, "INSERT INTO task_comments(id,task_id,author,author_type,content,created_at) VALUES(?,?,'fixture','user',?,1)", fixture.id, task.ID, fixture.content); err != nil {
			t.Fatal(err)
		}
	}
	two := uint64(2)
	detail, err := s.TaskRecent(ctx, "tasks", task.ID, &two)
	if err != nil || detail == nil || len(detail.Comments) != 2 || detail.CommentCount != 3 || detail.Comments[0].ID != "a-second" || detail.Comments[1].ID != "m-third" || len(detail.Comments[0].Content) > 65536 {
		t.Fatalf("recent %+v, %v", detail, err)
	}
	zero := uint64(0)
	detail, err = s.TaskRecent(ctx, "tasks", task.ID, &zero)
	if err != nil || len(detail.Comments) != 0 || detail.CommentCount != 3 {
		t.Fatalf("zero %+v, %v", detail, err)
	}
	detail, err = s.Task(ctx, "tasks", task.ID)
	if err != nil || len(detail.Comments) != 3 || len(detail.Comments[1].Content) != len(large) {
		t.Fatal("unlimited read changed persisted history")
	}
}

func TestTaskListRanksCountsAndDoneRetention(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	first := makeTask(t, s, nil)
	second := makeTask(t, s, nil)
	if _, err := s.AddTaskComment(ctx, "tasks", first.ID, "fixture", "user", "One", nil, nil); err != nil {
		t.Fatal(err)
	}
	list, err := s.Tasks(ctx, "TASKS", []string{"todo"}, false)
	if err != nil || len(list) != 2 || list[0].ID != first.ID || list[1].ID != second.ID || list[0].CommentCount != 1 {
		t.Fatalf("queue %+v, %v", list, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET status='done',done_at=1 WHERE id=?", first.ID); err != nil {
		t.Fatal(err)
	}
	list, err = s.Tasks(ctx, "tasks", []string{"done"}, false)
	if err != nil || len(list) != 0 {
		t.Fatalf("stale done listed: %+v, %v", list, err)
	}
	list, err = s.Tasks(ctx, "tasks", []string{"done"}, true)
	if err != nil || len(list) != 1 || list[0].ID != first.ID {
		t.Fatalf("explicit history unavailable: %+v, %v", list, err)
	}
}

// Rust task_types.rs::migration_preserves_existing_reviews_and_new_creation_defaults_to_general.
func TestLegacyTaskTypeMigrationPreservesReviewClassification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-task.db")
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, fixtureProject("tasks")); err != nil {
		t.Fatal(err)
	}
	old := makeTask(t, s, nil)
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE tasks DROP COLUMN task_type"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	detail, err := s.Task(ctx, "tasks", old.ID)
	if err != nil || detail == nil || detail.Task.TaskType != "software" {
		t.Fatalf("legacy classification %+v, %v", detail, err)
	}
	fresh := makeTask(t, s, nil)
	if fresh.TaskType != "general" {
		t.Fatalf("new classification %q", fresh.TaskType)
	}
}
