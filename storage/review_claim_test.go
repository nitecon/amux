package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nitecon/amux/identity/agent"
)

func reviewTask(t *testing.T, s *Store, labels []string) *Task {
	t.Helper()
	task, err := s.InsertTask(context.Background(), TaskInsert{
		Project: "tasks", Title: "Review", Reporter: "fixture", Labels: labels,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func reviewProviderSource(t *testing.T, s *Store, provider string) *Task {
	t.Helper()
	source := makeTask(t, s, &agent.Origin{
		SessionID:  "11111111-1111-4111-8111-111111111111",
		InstanceID: "22222222-2222-4222-8222-222222222222",
		Provider:   provider, OS: "linux",
	})
	return source
}

func reviewPresence(origin agent.Origin, project string, now time.Time) PresenceAgent {
	activity := now.Add(-time.Duration(presenceIdleAfter) * time.Millisecond).UnixMilli()
	return PresenceAgent{
		ProjectIdent: project, Origin: origin, AutoAssign: true,
		ReviewContextClear: true, LastActivityAt: &activity,
	}
}

func TestReviewClaimPreferredProviderBlocksWorseClaim(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	source := reviewProviderSource(t, s, "codex")
	task := reviewTask(t, s, []string{"task-review", "review-of:" + source.ID, "review-provider:claude"})
	preferred := assignmentOrigin("33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444", "claude", "linux")
	reportAssignmentPresence(t, s, now, "reviewers", 1, reviewPresence(preferred, "tasks", now))
	badClaim := taskActor()
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", badClaim); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("worse provider claim error = %v, want conflict", err)
	}
	claimed, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", &preferred)
	if err != nil || claimed.Status != "in_progress" {
		t.Fatalf("preferred provider claim = %+v, %v", claimed, err)
	}
}

func TestReviewClaimUnavailablePreferredPeerDoesNotBlock(t *testing.T) {
	checks := []struct {
		name   string
		mutate func(*PresenceAgent)
	}{
		{name: "busy", mutate: func(p *PresenceAgent) { p.Busy = true }},
		{name: "draft", mutate: func(p *PresenceAgent) { p.Draft = true }},
		{name: "expired", mutate: func(p *PresenceAgent) { p.LastActivityAt = nil }},
		{name: "wrong os", mutate: func(p *PresenceAgent) { p.Origin.OS = "windows" }},
		{name: "other project", mutate: func(p *PresenceAgent) { p.ProjectIdent = "other" }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			s := taskStore(t)
			ctx := context.Background()
			if check.name == "other project" {
				if err := s.InsertProject(ctx, fixtureProject("other")); err != nil {
					t.Fatal(err)
				}
			}
			now := time.UnixMilli(1_800_000_000_000)
			source := reviewProviderSource(t, s, "codex")
			task := reviewTask(t, s, []string{"task-review", "review-of:" + source.ID, "review-provider:claude", "os:linux"})
			preferred := assignmentOrigin("33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444", "claude", "linux")
			report := reviewPresence(preferred, "tasks", now)
			check.mutate(&report)
			reportAssignmentPresence(t, s, now, "reviewers", 1, report)
			if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", taskActor()); err != nil {
				t.Fatalf("fallback claim blocked by unavailable preferred peer: %v", err)
			}
		})
	}
}

func TestReviewClaimUsesWallClockForPeerLivenessBetweenTicks(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	source := reviewProviderSource(t, s, "codex")
	task := reviewTask(t, s, []string{"task-review", "review-of:" + source.ID, "review-provider:claude"})
	peer := assignmentOrigin("33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444", "claude", "linux")
	staleReportAt := time.Now().Add(-time.Duration(presenceGoneAfter+1_000) * time.Millisecond)
	report := reviewPresence(peer, "tasks", staleReportAt)
	reportAssignmentPresence(t, s, staleReportAt, "reviewers", 1, report)
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", taskActor()); err != nil {
		t.Fatalf("expired preferred peer blocked fallback before the next reconciliation tick: %v", err)
	}
}

func TestReviewClaimContributorFallbackAndPreferredIndependentReviewer(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	source := reviewProviderSource(t, s, "codex")
	contributor := taskActor()
	task := reviewTask(t, s, []string{"task-review", "review-of:" + source.ID, "review-provider:claude", "review-exclude:" + contributor.SessionID})
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", contributor); err != nil {
		t.Fatalf("contributor fallback without an available peer: %v", err)
	}
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "todo", contributor); err != nil {
		t.Fatal(err)
	}
	preferred := assignmentOrigin("33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444", "claude", "linux")
	reportAssignmentPresence(t, s, now, "reviewers", 1, reviewPresence(preferred, "tasks", now))
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", contributor); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("contributor claim with independent preferred peer error = %v, want conflict", err)
	}
}

func TestReviewClaimLegacyProviderFallbackUsesSourceOwner(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	source := reviewProviderSource(t, s, "codex")
	task := reviewTask(t, s, []string{"task-review", "review-of:" + source.ID})
	preferred := assignmentOrigin("33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444", "claude", "linux")
	reportAssignmentPresence(t, s, now, "reviewers", 1, reviewPresence(preferred, "tasks", now))
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", taskActor()); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("legacy review label did not derive preferred provider: %v", err)
	}
}

func TestReviewClaimExistingOwnerRetrySurvivesBetterPeerAndCanRelease(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	source := reviewProviderSource(t, s, "codex")
	task := reviewTask(t, s, []string{"task-review", "review-of:" + source.ID, "review-provider:claude"})
	owner := taskActor()
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", owner); err != nil {
		t.Fatalf("fallback initial claim: %v", err)
	}
	preferred := assignmentOrigin("33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444", "claude", "linux")
	reportAssignmentPresence(t, s, now, "reviewers", 1, reviewPresence(preferred, "tasks", now))
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", owner); err != nil {
		t.Fatalf("existing owner retry re-ranked claim: %v", err)
	}
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "todo", owner); err != nil {
		t.Fatalf("release review claim: %v", err)
	}
}
