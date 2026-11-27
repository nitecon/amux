package storage

import (
	"context"
	"testing"
	"time"
)

func TestDisabledProviderReceivesNoAssignment(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	task := makeTask(t, s, nil)
	agy := assignmentOrigin("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "antigravity", "linux")
	if _, err := s.SetProviderEnabled(ctx, "antigravity", false); err != nil {
		t.Fatal(err)
	}
	reportAssignmentPresence(t, s, now, "workers", 1, idleAssignmentAgent("tasks", agy, now))
	if err := s.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || got.Task.Status != "todo" || got.Task.OwnerOrigin != nil {
		t.Fatalf("disabled provider received task: %+v, %v", got, err)
	}
	if _, err := s.SetProviderEnabled(ctx, "antigravity", true); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err = s.Task(ctx, "tasks", task.ID)
	if err != nil || got.Task.OwnerOrigin == nil || *got.Task.OwnerOrigin != agy {
		t.Fatalf("re-enabled provider did not receive task: %+v, %v", got, err)
	}
}

func TestDisablingProviderReleasesActiveTasksToEnabledAgents(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	agy := assignmentOrigin("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "antigravity", "linux")
	working := *taskActor()
	agyTask := makeTask(t, s, &agy)
	codexTask := makeTask(t, s, &working)

	released, err := s.SetProviderEnabled(ctx, "antigravity", false)
	if err != nil || released != 1 {
		t.Fatalf("released=%d err=%v, want 1", released, err)
	}
	got, err := s.Task(ctx, "tasks", agyTask.ID)
	if err != nil || got.Task.Status != "todo" || got.Task.OwnerOrigin != nil || got.Task.OwnerAgentID != nil ||
		string(got.Task.Routing) != `{"mode":"auto"}` {
		t.Fatalf("antigravity task not released: %+v, %v", got, err)
	}
	foundComment := false
	for _, comment := range got.Comments {
		foundComment = foundComment || comment.Content == providerDisabledComment
	}
	if !foundComment {
		t.Fatal("release comment missing")
	}
	kept, err := s.Task(ctx, "tasks", codexTask.ID)
	if err != nil || kept.Task.OwnerOrigin == nil || *kept.Task.OwnerOrigin != working {
		t.Fatalf("enabled provider task changed: %+v, %v", kept, err)
	}

	claude := assignmentOrigin("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", "ffffffff-ffff-4fff-8fff-ffffffffffff", "claude", "linux")
	reportAssignmentPresence(t, s, now, "workers", 1,
		idleAssignmentAgent("tasks", agy, now),
		idleAssignmentAgent("tasks", claude, now),
	)
	if err := s.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err = s.Task(ctx, "tasks", agyTask.ID)
	if err != nil || got.Task.OwnerOrigin == nil || *got.Task.OwnerOrigin != claude {
		t.Fatalf("released task not reassigned to an enabled provider: %+v, %v", got, err)
	}
}

func TestProviderSettingsRejectUnknownProvider(t *testing.T) {
	s := taskStore(t)
	if _, err := s.SetProviderEnabled(context.Background(), "gemini", false); err != ErrUnknownProvider {
		t.Fatalf("err=%v, want ErrUnknownProvider", err)
	}
	settings, err := s.ProviderSettings(context.Background())
	if err != nil || len(settings) != 3 || !settings[0].Enabled {
		t.Fatalf("settings=%+v err=%v", settings, err)
	}
}
