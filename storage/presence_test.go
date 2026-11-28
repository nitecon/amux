package storage

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nitecon/amux/identity/agent"
)

func presenceAgent(session, instance string) PresenceAgent {
	return PresenceAgent{ProjectIdent: "presence", Origin: agent.Origin{
		SessionID: session, InstanceID: instance, Provider: "codex", OS: "linux",
	}}
}

func presenceStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(context.Background(), fixtureProject("presence")); err != nil {
		s.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func onlyPresence(t *testing.T, s *Store) AgentPresence {
	t.Helper()
	items, err := s.Presence(context.Background(), "presence")
	if err != nil || len(items) != 1 {
		t.Fatalf("presence = %+v, %v", items, err)
	}
	return items[0]
}

func TestPresenceTransitionsDurabilityAndMonotonicClock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "presence.db")
	s := presenceStore(t, path)
	start := time.UnixMilli(1_800_000_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	activity := start.UnixMilli()
	agent.LastActivityAt = &activity
	agent.AutoAssign = true
	accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 1, Agents: []PresenceAgent{agent}}, start)
	if err != nil || !accepted {
		t.Fatalf("initial report accepted=%v, err=%v", accepted, err)
	}
	if item := onlyPresence(t, s); item.State != "busy" || !item.AutoAssign {
		t.Fatalf("initial presence: %+v", item)
	}
	if err := s.ReconcilePresence(ctx, start.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if item := onlyPresence(t, s); item.State != "idle" {
		t.Fatalf("at idle threshold: %+v", item)
	}
	if err := s.ReconcilePresence(ctx, start.Add(29*time.Second)); err != nil {
		t.Fatal(err)
	}
	if item := onlyPresence(t, s); item.State != "idle" {
		t.Fatalf("reverse time regressed state: %+v", item)
	}
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 2, Agents: []PresenceAgent{agent}}, start.Add(20*time.Second)); err != nil || !accepted {
		t.Fatalf("reverse-time heartbeat accepted=%v, err=%v", accepted, err)
	}
	if item := onlyPresence(t, s); item.State != "idle" || item.SeenAt != start.Add(20*time.Second).UnixMilli() {
		t.Fatalf("reverse-time heartbeat changed monotonic state or renewed to future clock: %+v", item)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = presenceStore(t, path)
	item := onlyPresence(t, s)
	if item.State != "idle" || item.Source != "worker:a" || item.Origin.Provider != "codex" || item.Origin.OS != "linux" || !item.AutoAssign || item.LastActivityAt == nil || *item.LastActivityAt != activity {
		t.Fatalf("reopened presence lost durable fields: %+v", item)
	}
}

func TestPresenceUnknownDraftGoneOmissionAndReplay(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence.db"))
	start := time.UnixMilli(1_800_100_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	agent.Busy = true
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 1, Agents: []PresenceAgent{agent}}, start); err != nil || !ok {
		t.Fatalf("unknown report accepted=%v err=%v", ok, err)
	}
	if err := s.ReconcilePresence(ctx, start.Add(40*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := onlyPresence(t, s).State; got != "busy" {
		t.Fatalf("explicit busy became %q", got)
	}
	agent.Busy = false
	agent.Draft = true
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 2, Agents: []PresenceAgent{agent}}, start.Add(40*time.Second)); err != nil || !ok {
		t.Fatalf("draft report accepted=%v err=%v", ok, err)
	}
	if err := s.ReconcilePresence(ctx, start.Add(70*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := onlyPresence(t, s).State; got != "busy" {
		t.Fatalf("draft became %q", got)
	}
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 3}, start.Add(80*time.Second)); err != nil || !ok {
		t.Fatalf("omission accepted=%v err=%v", ok, err)
	}
	if got := onlyPresence(t, s).State; got != "gone" {
		t.Fatalf("omitted agent state %q", got)
	}
	if err := s.ReconcilePresence(ctx, start.Add(81*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := onlyPresence(t, s).State; got != "gone" {
		t.Fatalf("pre-expiry tick resurrected omitted agent to %q", got)
	}
	if err := s.ReconcilePresence(ctx, start.Add(200*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := onlyPresence(t, s).State; got != "gone" {
		t.Fatalf("tick resurrected omitted agent to %q", got)
	}
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 2, Agents: []PresenceAgent{agent}}, start.Add(201*time.Second)); err != nil || ok {
		t.Fatalf("replayed report accepted=%v err=%v", ok, err)
	}
	if got := onlyPresence(t, s).State; got != "gone" {
		t.Fatalf("replay revived omitted agent to %q", got)
	}
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 4, Agents: []PresenceAgent{agent}}, start.Add(201*time.Second)); err != nil || !ok {
		t.Fatalf("new snapshot accepted=%v err=%v", ok, err)
	}
	if got := onlyPresence(t, s).State; got != "busy" {
		t.Fatalf("new snapshot did not revive agent: %q", got)
	}
	agent.Busy = false
	agent.Draft = false
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 5, Agents: []PresenceAgent{agent}}, start.Add(202*time.Second)); err != nil || !ok {
		t.Fatalf("no-activity report accepted=%v err=%v", ok, err)
	}
	if err := s.ReconcilePresence(ctx, start.Add(240*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := onlyPresence(t, s).State; got != "unknown" {
		t.Fatalf("unknown activity became %q", got)
	}
}

func TestPresenceSourceTakeoverOriginFenceAndConcurrentReplay(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence.db"))
	start := time.UnixMilli(1_800_200_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 1, Agents: []PresenceAgent{agent}}, start); err != nil || !ok {
		t.Fatalf("initial report accepted=%v err=%v", ok, err)
	}
	changedOrigin := agent
	changedOrigin.Origin.Provider = "claude"
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 2, Agents: []PresenceAgent{changedOrigin}}, start.Add(time.Second)); !errors.Is(err, ErrPresenceConflict) {
		t.Fatalf("origin mutation error = %v", err)
	}
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:b", Sequence: 1, Agents: []PresenceAgent{agent}}, start.Add(90*time.Second)); err != nil || !ok {
		t.Fatalf("stale-source takeover accepted=%v err=%v", ok, err)
	}
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 2, Agents: []PresenceAgent{agent}}, start.Add(91*time.Second)); !errors.Is(err, ErrPresenceConflict) {
		t.Fatalf("old source overwrote live owner: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:b", Sequence: 2, Agents: []PresenceAgent{agent}}, start.Add(92*time.Second))
			if err != nil {
				t.Errorf("concurrent replay: %v", err)
				return
			}
			if ok {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("accepted %d concurrent copies of one sequence", accepted)
	}
}

func TestPresenceRejectsInvalidReportsAndArchivedProjects(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence.db"))
	start := time.UnixMilli(1_800_300_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	future := start.Add(time.Second).UnixMilli()
	agent.LastActivityAt = &future
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 1, Agents: []PresenceAgent{agent}}, start); !errors.Is(err, ErrPresenceInput) {
		t.Fatalf("future activity error = %v", err)
	}
	agent.LastActivityAt = nil
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 1, Agents: []PresenceAgent{agent, agent}}, start); !errors.Is(err, ErrPresenceInput) {
		t.Fatalf("duplicate session error = %v", err)
	}
	missing := presenceAgent("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	missing.ProjectIdent = "missing"
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 1, Agents: []PresenceAgent{missing}}, start); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing project error = %v", err)
	}
	if _, err := s.SetProjectArchived(ctx, "presence", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 1, Agents: []PresenceAgent{agent}}, start); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("archived project error = %v", err)
	}
}
