package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestInsertTaskPersistsCreationRoutingAndAuditAtomically(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='adhoc',canonical_remote=NULL WHERE ident='tasks'"); err != nil {
		t.Fatal(err)
	}
	other := "other"
	otherProject := fixtureProject(other)
	otherProject.Kind = "adhoc"
	if err := s.InsertProject(ctx, otherProject); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"auto", "unassigned", "broadcast"} {
		t.Run(mode, func(t *testing.T) {
			input := TaskInsert{Project: "tasks", Title: mode, Reporter: "user"}
			if mode != "auto" {
				input.Routing = &TaskRouting{Mode: mode}
			}
			task, err := s.InsertTask(ctx, input, nil)
			if err != nil {
				t.Fatal(err)
			}
			if task.Status != "todo" || task.OwnerOrigin != nil || task.OwnerAgentID != nil || task.StartedAt != nil {
				t.Fatalf("created task acquired unintended ownership: %+v", task)
			}
			var route TaskRouting
			if err := json.Unmarshal(task.Routing, &route); err != nil || route.Mode != mode || route.Target != nil {
				t.Fatalf("routing=%s parsed=%+v err=%v", task.Routing, route, err)
			}
			taskID := task.ID
			events, err := s.TaskEvents(ctx, EventQuery{TaskID: &taskID})
			if err != nil || len(events) != 1 || events[0].Kind != "task_created" || events[0].Origin != nil {
				t.Fatalf("human creation event=%+v err=%v", events, err)
			}
			var snapshot Task
			if err := json.Unmarshal(events[0].Task, &snapshot); err != nil || snapshot.ID != task.ID || string(snapshot.Routing) != string(task.Routing) {
				t.Fatalf("creation event snapshot=%+v err=%v", snapshot, err)
			}
		})
	}

	targetOrigin := AgentOrigin{SessionID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", InstanceID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Provider: "claude", OS: "windows"}
	const generation = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	if _, err := s.RegisterTarget(ctx, "tasks", TargetRegistration{Origin: targetOrigin, Generation: generation}, time.Now()); err != nil {
		t.Fatal(err)
	}
	target := RegisteredTargetIdentity{Origin: targetOrigin, Generation: generation}
	routed, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Exact route", Reporter: "user", Routing: &TaskRouting{Mode: "session", Target: &target}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if routed.Status != "todo" || routed.StartedAt != nil || routed.OwnerAgentID == nil || *routed.OwnerAgentID != targetOrigin.SessionID || routed.OwnerOrigin == nil || *routed.OwnerOrigin != targetOrigin {
		t.Fatalf("exact route owner/status=%+v", routed)
	}
	var route TaskRouting
	if err := json.Unmarshal(routed.Routing, &route); err != nil || route.Mode != "session" || route.Target == nil || *route.Target != target {
		t.Fatalf("exact route=%s parsed=%+v err=%v", routed.Routing, route, err)
	}
	routedID := routed.ID
	events, err := s.TaskEvents(ctx, EventQuery{TaskID: &routedID})
	if err != nil || len(events) != 1 || events[0].Origin != nil {
		t.Fatalf("routed human create event=%+v err=%v", events, err)
	}
	var snapshot Task
	if err := json.Unmarshal(events[0].Task, &snapshot); err != nil || snapshot.OwnerOrigin == nil || *snapshot.OwnerOrigin != targetOrigin || string(snapshot.Routing) != string(routed.Routing) {
		t.Fatalf("routed create event snapshot=%+v err=%v", snapshot, err)
	}
}

func TestInsertTaskRejectsInvalidCreationRoutingWithoutPartialTaskOrEvent(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='adhoc',canonical_remote=NULL WHERE ident='tasks'"); err != nil {
		t.Fatal(err)
	}
	targetOrigin := AgentOrigin{SessionID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", InstanceID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Provider: "claude", OS: "linux"}
	const generation = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	if _, err := s.RegisterTarget(ctx, "tasks", TargetRegistration{Origin: targetOrigin, Generation: generation}, time.Now()); err != nil {
		t.Fatal(err)
	}
	otherProject := fixtureProject("other")
	otherProject.Kind = "adhoc"
	if err := s.InsertProject(ctx, otherProject); err != nil {
		t.Fatal(err)
	}
	wrongProjectOrigin := AgentOrigin{SessionID: "99999999-9999-4999-8999-999999999999", InstanceID: "88888888-8888-4888-8888-888888888888", Provider: "codex", OS: "linux"}
	if _, err := s.RegisterTarget(ctx, "other", TargetRegistration{Origin: wrongProjectOrigin, Generation: generation}, time.Now()); err != nil {
		t.Fatal(err)
	}
	stale := RegisteredTargetIdentity{Origin: targetOrigin, Generation: "ffffffff-ffff-4fff-8fff-ffffffffffff"}
	wrongOS := RegisteredTargetIdentity{Origin: targetOrigin, Generation: generation}
	wrongProject := RegisteredTargetIdentity{Origin: wrongProjectOrigin, Generation: generation}
	for _, test := range []struct {
		name    string
		routing *TaskRouting
		labels  []string
	}{
		{name: "unknown mode", routing: &TaskRouting{Mode: "manual"}},
		{name: "unassigned cannot carry target", routing: &TaskRouting{Mode: "unassigned", Target: &stale}},
		{name: "session requires target", routing: &TaskRouting{Mode: "session"}},
		{name: "stale target generation", routing: &TaskRouting{Mode: "session", Target: &stale}},
		{name: "target registered to another project", routing: &TaskRouting{Mode: "session", Target: &wrongProject}},
		{name: "target OS mismatch", routing: &TaskRouting{Mode: "session", Target: &wrongOS}, labels: []string{"os:windows"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: test.name, Reporter: "user", Labels: test.labels, Routing: test.routing}, nil)
			if err == nil {
				t.Fatal("invalid routed task was accepted")
			}
			if test.name == "stale target generation" || test.name == "target OS mismatch" {
				if !errors.Is(err, ErrTaskConflict) {
					t.Fatalf("target validation error=%v, want conflict", err)
				}
			}
			var tasks, created int
			if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM tasks WHERE title=?", test.name).Scan(&tasks); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM task_events WHERE kind='task_created' AND json_extract(task_json,'$.title')=?", test.name).Scan(&created); err != nil {
				t.Fatal(err)
			}
			if tasks != 0 || created != 0 {
				t.Fatalf("failed create left tasks=%d creation events=%d", tasks, created)
			}
		})
	}
}

func TestInsertTaskCanonicalRemoteBindingIsCheckedAgainstTransactionalProject(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='repo',canonical_remote='github.com/acme/tasks' WHERE ident='tasks'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Auto without remote", Reporter: "user"}, nil); err != nil {
		t.Fatalf("auto creation without canonical remote regressed: %v", err)
	}
	if _, err := s.InsertTask(ctx, TaskInsert{
		Project: "tasks", Title: "Explicit without remote", Reporter: "user", Routing: &TaskRouting{Mode: "unassigned"},
	}, nil); !errors.Is(err, ErrTargetBinding) {
		t.Fatalf("explicit repo routing without remote error=%v, want binding conflict", err)
	}
	if tasks, events := countCreatedTaskState(t, s, "Explicit without remote"); tasks != 0 || events != 0 {
		t.Fatalf("rejected explicit create mutated state: tasks=%d events=%d", tasks, events)
	}
	validAlias := "git@github.com:acme/tasks.git"
	created, err := s.InsertTask(ctx, TaskInsert{
		Project: "tasks", Title: "Bound create", Reporter: "user", CanonicalRemote: &validAlias,
	}, nil)
	if err != nil {
		t.Fatalf("equivalent canonical remote rejected: %v", err)
	}
	if created.ID == "" {
		t.Fatal("bound create returned no task")
	}

	assertRejectedWithoutMutation := func(title, supplied string) {
		t.Helper()
		beforeTasks, beforeEvents := countCreatedTaskState(t, s, title)
		_, err := s.InsertTask(ctx, TaskInsert{
			Project: "tasks", Title: title, Reporter: "user", CanonicalRemote: &supplied,
		}, nil)
		if !errors.Is(err, ErrTargetBinding) {
			t.Fatalf("canonical remote %q error=%v, want binding conflict", supplied, err)
		}
		afterTasks, afterEvents := countCreatedTaskState(t, s, title)
		if afterTasks != beforeTasks || afterEvents != beforeEvents {
			t.Fatalf("rejected create mutated state: tasks %d->%d events %d->%d", beforeTasks, afterTasks, beforeEvents, afterEvents)
		}
	}

	assertRejectedWithoutMutation("Wrong repository", "github.com/other/tasks")
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET canonical_remote='github.com/acme/rebound' WHERE ident='tasks'"); err != nil {
		t.Fatal(err)
	}
	assertRejectedWithoutMutation("Rebound repository", validAlias)
}

func countCreatedTaskState(t *testing.T, s *Store, title string) (int, int) {
	t.Helper()
	var tasks, events int
	ctx := context.Background()
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM tasks WHERE title=?", title).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM task_events WHERE kind='task_created' AND json_extract(task_json,'$.title')=?", title).Scan(&events); err != nil {
		t.Fatal(err)
	}
	return tasks, events
}
