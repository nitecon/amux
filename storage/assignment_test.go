package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nitecon/amux/identity/agent"
)

func assignmentOrigin(session, instance, provider, os string) agent.Origin {
	return agent.Origin{SessionID: session, InstanceID: instance, Provider: provider, OS: os}
}

func reportAssignmentPresence(t *testing.T, s *Store, now time.Time, source string, sequence int64, agents ...PresenceAgent) {
	t.Helper()
	ok, err := s.ReportPresence(context.Background(), PresenceReport{Source: source, Sequence: sequence, Agents: agents}, now)
	if err != nil || !ok {
		t.Fatalf("report presence: accepted=%v err=%v", ok, err)
	}
}

func idleAssignmentAgent(project string, origin agent.Origin, now time.Time) PresenceAgent {
	activity := now.Add(-time.Duration(presenceIdleAfter) * time.Millisecond).UnixMilli()
	return PresenceAgent{ProjectIdent: project, Origin: origin, AutoAssign: true, LastActivityAt: &activity}
}

func setTaskCreatedAt(t *testing.T, s *Store, task *Task, at int64) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), "UPDATE tasks SET created_at=? WHERE id=?", at, task.ID); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileAssignmentsUsesFIFOAcrossSessions(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	first := makeTask(t, s, nil)
	second, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Second", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	setTaskCreatedAt(t, s, first, 10)
	setTaskCreatedAt(t, s, second, 20)
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE assignment_actor_audit(task_id TEXT,origin_json TEXT);
 CREATE TRIGGER assignment_actor_audit_insert AFTER UPDATE OF status ON tasks WHEN NEW.status='in_progress'
 BEGIN INSERT INTO assignment_actor_audit VALUES(NEW.id,(SELECT origin_json FROM task_mutation_context WHERE id=1)); END;`); err != nil {
		t.Fatal(err)
	}
	a := taskActor()
	b := assignmentOrigin("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "claude", "linux")
	reportAssignmentPresence(t, s, now, "workers", 1,
		idleAssignmentAgent("tasks", *a, now),
		idleAssignmentAgent("tasks", b, now),
	)
	if err := s.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	owners := make(map[string]agent.Origin)
	for _, task := range []*Task{first, second} {
		got, err := s.Task(ctx, "tasks", task.ID)
		if err != nil || got == nil || got.Task.Status != "in_progress" || got.Task.OwnerOrigin == nil ||
			(*got.Task.OwnerOrigin != *a && *got.Task.OwnerOrigin != b) {
			t.Fatalf("task %s assigned incorrectly: %+v, %v", task.ID, got, err)
		}
		owners[task.ID] = *got.Task.OwnerOrigin
	}
	if owners[first.ID] == owners[second.ID] {
		t.Fatal("one worker received two tasks")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT task_id,origin_json FROM assignment_actor_audit")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seenAudit := map[string]bool{}
	for rows.Next() {
		var id string
		var raw string
		var origin agent.Origin
		if err := rows.Scan(&id, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &origin); err != nil {
			t.Fatal(err)
		}
		seenAudit[id] = true
		if origin != owners[id] {
			t.Fatalf("task mutation trigger observed wrong origin for %s: %+v", id, origin)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seenAudit) != 2 {
		t.Fatalf("actor audit missed assignment: %v", seenAudit)
	}
}

func TestAssignmentGoalMatcherIsExactForNormalAndReviewTasks(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	selected := "ship"
	other := "Ship"
	candidate := assignmentCandidate{
		origin:   *taskActor(),
		presence: AgentPresence{PresenceAgent: PresenceAgent{ProjectIdent: "tasks", Origin: *taskActor(), Goal: &selected}, State: "idle"},
	}
	for _, test := range []struct {
		name   string
		labels []string
		goal   *string
		want   bool
	}{
		{name: "untagged normal task remains unrestricted", want: true},
		{name: "matching normal goal", goal: &selected, want: true},
		{name: "normal goal is case sensitive", goal: &other},
		{name: "matching review goal", labels: []string{"task-review"}, goal: &selected, want: true},
		{name: "review goal mismatch", labels: []string{"task-review"}, goal: &other},
	} {
		t.Run(test.name, func(t *testing.T) {
			task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: test.name, Reporter: "fixture", Labels: test.labels, Goal: test.goal}, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := assignmentEligible(ctx, s.db, *task, candidate)
			if err != nil || got != test.want {
				t.Fatalf("eligible=%v err=%v want=%v", got, err, test.want)
			}
		})
	}
}

func TestReconcileAssignmentsFailsClosedForUnavailableSessions(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Windows", Reporter: "fixture", Labels: []string{"os:windows"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	linux := assignmentOrigin("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "claude", "linux")
	unknown := idleAssignmentAgent("tasks", assignmentOrigin("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", "ffffffff-ffff-4fff-8fff-ffffffffffff", "codex", "windows"), now)
	unknown.LastActivityAt = nil
	busy := idleAssignmentAgent("tasks", assignmentOrigin("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "codex", "windows"), now)
	busy.Busy = true
	draft := idleAssignmentAgent("tasks", assignmentOrigin("33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444", "codex", "windows"), now)
	draft.Draft = true
	noOptIn := idleAssignmentAgent("tasks", assignmentOrigin("55555555-5555-4555-8555-555555555555", "66666666-6666-4666-8666-666666666666", "codex", "windows"), now)
	noOptIn.AutoAssign = false
	staleAt := now.Add(-time.Duration(presenceGoneAfter) * time.Millisecond)
	stale := idleAssignmentAgent("tasks", assignmentOrigin("77777777-7777-4777-8777-777777777777", "88888888-8888-4888-8888-888888888888", "codex", "windows"), staleAt)
	reportAssignmentPresence(t, s, staleAt, "unavailable", 1, stale)
	reportAssignmentPresence(t, s, now, "workers", 1,
		idleAssignmentAgent("tasks", linux, now), unknown, busy, draft, noOptIn,
	)
	if err := s.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || got.Task.Status != "todo" {
		t.Fatalf("ineligible worker claimed OS-restricted task: %+v, %v", got, err)
	}
}

func TestReconcileAssignmentsRepeatedAndConcurrentTicksDoNotDuplicate(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	first := makeTask(t, s, nil)
	second, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Later", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := taskActor()
	reportAssignmentPresence(t, s, now, "workers", 1, idleAssignmentAgent("tasks", *a, now))
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- s.ReconcileAssignments(ctx, now) }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if err := s.ReconcileAssignments(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	gotFirst, err := s.Task(ctx, "tasks", first.ID)
	if err != nil || gotFirst.Task.Status != "in_progress" || gotFirst.Task.OwnerOrigin == nil || *gotFirst.Task.OwnerOrigin != *a {
		t.Fatalf("first task not assigned once: %+v, %v", gotFirst, err)
	}
	gotSecond, err := s.Task(ctx, "tasks", second.ID)
	if err != nil || gotSecond.Task.Status != "todo" {
		t.Fatalf("one session received multiple tasks: %+v, %v", gotSecond, err)
	}
}

func TestReconcileAssignmentsKeepsProjectIsolation(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	if err := s.InsertProject(ctx, fixtureProject("other")); err != nil {
		t.Fatal(err)
	}
	first := makeTask(t, s, nil)
	other, err := s.InsertTask(ctx, TaskInsert{Project: "other", Title: "Other project", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(1_800_000_000_000)
	a := taskActor()
	reportAssignmentPresence(t, s, now, "workers", 1, idleAssignmentAgent("tasks", *a, now))
	if err := s.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	gotFirst, err := s.Task(ctx, "tasks", first.ID)
	if err != nil || gotFirst.Task.Status != "in_progress" {
		t.Fatalf("same-project task not assigned: %+v, %v", gotFirst, err)
	}
	gotOther, err := s.Task(ctx, "other", other.ID)
	if err != nil || gotOther.Task.Status != "todo" {
		t.Fatalf("cross-project task was assigned: %+v, %v", gotOther, err)
	}
}

func TestReconcileAssignmentsSuppressesExplicitReleaseUntilTaskChanges(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	task := makeTask(t, s, nil)
	a := taskActor()
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "todo", a); err != nil {
		t.Fatal(err)
	}
	var releases int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM assignment_releases WHERE task_id=?", task.ID).Scan(&releases); err != nil || releases != 1 {
		t.Fatalf("release not recorded: count=%d err=%v", releases, err)
	}
	reportAssignmentPresence(t, s, now, "workers", 1, idleAssignmentAgent("tasks", *a, now))
	for range 2 {
		if err := s.ReconcileAssignments(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || got.Task.Status != "todo" {
		t.Fatalf("released task looped back to same session: %+v, %v", got, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET title='Changed work' WHERE id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err = s.Task(ctx, "tasks", task.ID)
	if err != nil || got.Task.Status != "in_progress" {
		t.Fatalf("task change did not clear release suppression: %+v, %v", got, err)
	}
}

func TestReconcileAssignmentsUsesPresenceClockAtExactBoundaries(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	task := makeTask(t, s, nil)
	a := taskActor()
	activity := now.Add(-time.Duration(presenceIdleAfter) * time.Millisecond).UnixMilli()
	reportAssignmentPresence(t, s, now, "workers", 1, PresenceAgent{ProjectIdent: "tasks", Origin: *a, AutoAssign: true, LastActivityAt: &activity})
	if err := s.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || got.Task.Status != "in_progress" {
		t.Fatalf("exact idle boundary was not eligible: %+v, %v", got, err)
	}

	// A backwards caller clock must use Presence's persisted monotonic clock.
	second, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Backward clock", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := assignmentOrigin("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "claude", "linux")
	reportAssignmentPresence(t, s, now, "other-worker", 1, idleAssignmentAgent("tasks", b, now))
	if err := s.ReconcileAssignments(ctx, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err = s.Task(ctx, "tasks", second.ID)
	if err != nil || got.Task.Status != "in_progress" {
		t.Fatalf("backwards clock lost eligible assignment: %+v, %v", got, err)
	}
}
