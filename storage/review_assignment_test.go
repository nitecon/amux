package storage

import (
	"context"
	"sync"
	"testing"
	"time"
)

func reviewAssignmentTask(t *testing.T, s *Store, labels ...string) *Task {
	t.Helper()
	task, err := s.InsertTask(context.Background(), TaskInsert{
		Project: "tasks", Title: "Queued review", Reporter: "fixture", Labels: append([]string{"task-review"}, labels...),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func reviewAssignmentAgent(project, session, instance, provider, os string, now time.Time, clear, busy bool) PresenceAgent {
	agent := idleAssignmentAgent(project, assignmentOrigin(session, instance, provider, os), now)
	agent.ReviewContextClear = clear
	agent.Busy = busy
	return agent
}

func TestReviewsPreferAvailablePeersAndFallbackWithRequiredContextClear(t *testing.T) {
	const (
		codexSession   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		codexInstance  = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		claudeSession  = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		claudeInstance = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	)
	for _, test := range []struct {
		name       string
		claudeOS   string
		claudeBusy bool
		codexClear bool
		wantOwner  string
		wantStatus string
	}{
		{name: "available preferred peer wins", claudeOS: "linux", codexClear: true, wantOwner: claudeSession, wantStatus: "in_progress"},
		{name: "fallback with clear context", claudeOS: "linux", claudeBusy: true, codexClear: true, wantOwner: codexSession, wantStatus: "in_progress"},
		{name: "fallback requires clear context", claudeOS: "linux", claudeBusy: true, wantStatus: "todo"},
		{name: "ineligible preferred peer permits fallback", claudeOS: "windows", codexClear: true, wantOwner: codexSession, wantStatus: "in_progress"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := taskStore(t)
			ctx := context.Background()
			now := time.UnixMilli(1_800_000_000_000)
			task := reviewAssignmentTask(t, s, "review-provider:claude", "os:linux")
			reportAssignmentPresence(t, s, now, "workers", 1,
				reviewAssignmentAgent("tasks", codexSession, codexInstance, "codex", "linux", now, test.codexClear, false),
				reviewAssignmentAgent("tasks", claudeSession, claudeInstance, "claude", test.claudeOS, now, false, test.claudeBusy),
			)
			if err := s.ReconcileAssignments(ctx, now); err != nil {
				t.Fatal(err)
			}
			got, err := s.Task(ctx, "tasks", task.ID)
			if err != nil || got == nil || got.Task.Status != test.wantStatus {
				t.Fatalf("review status = %+v, %v; want %s", got, err, test.wantStatus)
			}
			if test.wantOwner != "" && (got.Task.OwnerOrigin == nil || got.Task.OwnerOrigin.SessionID != test.wantOwner) {
				t.Fatalf("review owner = %+v; want session %s", got.Task.OwnerOrigin, test.wantOwner)
			}
		})
	}
}

func TestReviewAssignmentRanksIndependentPeerBeforeContributor(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	const (
		contributorSession  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		contributorInstance = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		independentSession  = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		independentInstance = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	)
	task := reviewAssignmentTask(t, s, "review-exclude:"+contributorSession, "review-provider:codex")
	reportAssignmentPresence(t, s, now, "workers", 1,
		reviewAssignmentAgent("tasks", contributorSession, contributorInstance, "codex", "linux", now, true, false),
		reviewAssignmentAgent("tasks", independentSession, independentInstance, "claude", "linux", now, true, false),
	)
	if err := s.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || got == nil || got.Task.Status != "in_progress" || got.Task.OwnerOrigin == nil || got.Task.OwnerOrigin.SessionID != independentSession {
		t.Fatalf("independent reviewer should beat contributor: %+v, %v", got, err)
	}
}

func TestReviewAssignmentContributorFallbackRequiresClearContext(t *testing.T) {
	const (
		contributorSession  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		contributorInstance = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	)
	for _, clear := range []bool{false, true} {
		t.Run(map[bool]string{false: "context required", true: "clear context fallback"}[clear], func(t *testing.T) {
			s := taskStore(t)
			ctx := context.Background()
			now := time.UnixMilli(1_800_000_000_000)
			task := reviewAssignmentTask(t, s, "review-exclude:"+contributorSession)
			reportAssignmentPresence(t, s, now, "workers", 1,
				reviewAssignmentAgent("tasks", contributorSession, contributorInstance, "codex", "linux", now, clear, false),
			)
			if err := s.ReconcileAssignments(ctx, now); err != nil {
				t.Fatal(err)
			}
			got, err := s.Task(ctx, "tasks", task.ID)
			wantStatus := "todo"
			if clear {
				wantStatus = "in_progress"
			}
			if err != nil || got == nil || got.Task.Status != wantStatus {
				t.Fatalf("contributor fallback status = %+v, %v; want %s", got, err, wantStatus)
			}
		})
	}
}

func TestEachOrdinaryAssignmentIsFollowedByAQueuedReview(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	first := makeTask(t, s, nil)
	second, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Second ordinary", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	review := reviewAssignmentTask(t, s)
	setTaskCreatedAt(t, s, first, 10)
	setTaskCreatedAt(t, s, second, 20)
	setTaskCreatedAt(t, s, review, 30)
	const (
		codexSession = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		claudeOne    = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		claudeTwo    = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	)
	reportAssignmentPresence(t, s, now, "workers", 1,
		reviewAssignmentAgent("tasks", codexSession, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "codex", "linux", now, true, false),
		reviewAssignmentAgent("tasks", claudeOne, "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "claude", "linux", now, true, false),
		reviewAssignmentAgent("tasks", claudeTwo, "ffffffff-ffff-4fff-8fff-ffffffffffff", "claude", "linux", now, true, false),
	)
	if err := s.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	owners := map[string]bool{}
	for _, task := range []*Task{first, review, second} {
		got, err := s.Task(ctx, "tasks", task.ID)
		if err != nil || got == nil || got.Task.Status != "in_progress" || got.Task.OwnerOrigin == nil {
			t.Fatalf("task %s assignment = %+v, %v", task.ID, got, err)
		}
		if owners[got.Task.OwnerOrigin.SessionID] {
			t.Fatal("one session received more than one task")
		}
		owners[got.Task.OwnerOrigin.SessionID] = true
	}
	var reviewNext bool
	if err := s.db.QueryRowContext(ctx, "SELECT review_next FROM assignment_turns WHERE project='tasks'").Scan(&reviewNext); err != nil || !reviewNext {
		t.Fatalf("ordinary/review/ordinary turns should queue the next review: review_next=%v err=%v", reviewNext, err)
	}
}

func TestOrdinaryAssignmentUsesAnAvailableAgent(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	task := makeTask(t, s, nil)
	claude := assignmentOrigin("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "claude", "linux")
	codex := taskActor()
	reportAssignmentPresence(t, s, now, "workers", 1,
		idleAssignmentAgent("tasks", claude, now),
		idleAssignmentAgent("tasks", *codex, now),
	)
	if err := s.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || got == nil || got.Task.OwnerOrigin == nil || (*got.Task.OwnerOrigin != *codex && *got.Task.OwnerOrigin != claude) {
		t.Fatalf("ordinary task should go to an available agent: %+v, %v", got, err)
	}
}

func TestCodexKeepsOrdinaryWorkWhenPreferredPeerTakesReview(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	ordinary := makeTask(t, s, nil)
	review := reviewAssignmentTask(t, s, "review-provider:claude", "os:linux")
	setTaskCreatedAt(t, s, ordinary, 20)
	setTaskCreatedAt(t, s, review, 10)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO assignment_turns(project,review_next) VALUES('tasks',1)"); err != nil {
		t.Fatal(err)
	}
	codex := taskActor()
	claude := assignmentOrigin("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "claude", "linux")
	reportAssignmentPresence(t, s, now, "workers", 1,
		reviewAssignmentAgent("tasks", codex.SessionID, codex.InstanceID, codex.Provider, codex.OS, now, true, false),
		reviewAssignmentAgent("tasks", claude.SessionID, claude.InstanceID, claude.Provider, claude.OS, now, false, false),
	)
	if err := s.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		task  *Task
		owner string
	}{{ordinary, codex.SessionID}, {review, claude.SessionID}} {
		got, err := s.Task(ctx, "tasks", check.task.ID)
		if err != nil || got == nil || got.Task.Status != "in_progress" || got.Task.OwnerOrigin == nil || got.Task.OwnerOrigin.SessionID != check.owner {
			t.Fatalf("task %s assignment = %+v, %v; want owner %s", check.task.ID, got, err, check.owner)
		}
	}
}

func TestReviewAssignmentMultiProjectBusyAndConcurrentTicksDoNotDuplicate(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	if err := s.InsertProject(ctx, fixtureProject("other")); err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(1_800_000_000_000)
	first := reviewAssignmentTask(t, s)
	other, err := s.InsertTask(ctx, TaskInsert{Project: "other", Title: "Other review", Reporter: "fixture", Labels: []string{"task-review"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	codex := taskActor()
	claude := assignmentOrigin("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "claude", "linux")
	reportAssignmentPresence(t, s, now, "workers", 1, idleAssignmentAgent("tasks", *codex, now))
	reportAssignmentPresence(t, s, now, "other-workers", 1, idleAssignmentAgent("other", claude, now))
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.ReconcileAssignments(ctx, now)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	gotFirst, err := s.Task(ctx, "tasks", first.ID)
	if err != nil || gotFirst == nil || gotFirst.Task.Status != "in_progress" || gotFirst.Task.OwnerOrigin == nil || *gotFirst.Task.OwnerOrigin != *codex {
		t.Fatalf("same-project review assignment = %+v, %v", gotFirst, err)
	}
	gotOther, err := s.Task(ctx, "other", other.ID)
	if err != nil || gotOther == nil || gotOther.Task.Status != "in_progress" || gotOther.Task.OwnerOrigin == nil || *gotOther.Task.OwnerOrigin != claude {
		t.Fatalf("other-project review assignment = %+v, %v", gotOther, err)
	}
	var duplicateClaims int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE status='in_progress' AND id IN(?,?)", first.ID, other.ID).Scan(&duplicateClaims); err != nil || duplicateClaims != 2 {
		t.Fatalf("concurrent ticks left duplicate/missing review claims: count=%d err=%v", duplicateClaims, err)
	}
}
