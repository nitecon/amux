package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPresenceHostnameMigrationUpgradesVersion17DataAndRejectsFutureVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "presence-v17.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, fixtureProject("presence")); err != nil {
		t.Fatal(err)
	}
	start := time.UnixMilli(1_800_790_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	agent.Hostname = ptr("retained-host")
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "retained-source", Sequence: 7, Agents: []PresenceAgent{agent}}, start); err != nil || !ok {
		t.Fatalf("seed report accepted=%v err=%v", ok, err)
	}
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE amux_presence_agents DROP COLUMN hostname; DELETE FROM amux_go_migrations WHERE version=18"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade version 17: %v", err)
	}
	defer s.Close()
	items, err := s.Presence(ctx, "presence")
	if err != nil || len(items) != 1 || items[0].State != "unknown" || items[0].Hostname != nil || items[0].SeenAt != start.UnixMilli() {
		t.Fatalf("upgraded presence=%+v err=%v", items, err)
	}
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "retained-source", Sequence: 7, Agents: []PresenceAgent{agent}}, start.Add(time.Second)); err != nil || ok {
		t.Fatalf("upgrade lost report replay cursor: accepted=%v err=%v", ok, err)
	}

	futurePath := filepath.Join(t.TempDir(), "presence-v19.db")
	future, err := Open(ctx, futurePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := future.db.ExecContext(ctx, "INSERT INTO amux_go_migrations(version) VALUES(?)", maxGoMigrationVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := future.Close(); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(ctx, futurePath); err == nil {
		opened.Close()
		t.Fatal("opened a database from a future Go migration")
	}
}

func TestPresenceHostnameMigrationValidationReplayAndTransitions(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-hostname.db"))
	var migration int
	if err := s.db.QueryRowContext(ctx, "SELECT MAX(version) FROM amux_go_migrations").Scan(&migration); err != nil || migration < 18 {
		t.Fatalf("migration version=%d, want at least 18, err=%v", migration, err)
	}
	var nullable int
	if err := s.db.QueryRowContext(ctx, "SELECT "+`"notnull"`+" FROM pragma_table_info('amux_presence_agents') WHERE name='hostname'").Scan(&nullable); err != nil || nullable != 0 {
		t.Fatalf("hostname nullability=%d err=%v", nullable, err)
	}

	start := time.UnixMilli(1_800_800_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	agent.Hostname = ptr("  worker-one  ")
	agent.LastActivityAt = ptrI64(start.UnixMilli())
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "native", Sequence: 1, Agents: []PresenceAgent{agent}}, start); err != nil || !ok {
		t.Fatalf("report accepted=%v err=%v", ok, err)
	}
	item := onlyPresence(t, s)
	if item.Hostname == nil || *item.Hostname != "worker-one" {
		t.Fatalf("stored hostname=%v", item.Hostname)
	}

	// Replaying sequence 1 with different metadata cannot overwrite the accepted snapshot.
	agent.Hostname = ptr("attacker")
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "native", Sequence: 1, Agents: []PresenceAgent{agent}}, start.Add(time.Second)); err != nil || ok {
		t.Fatalf("replay accepted=%v err=%v", ok, err)
	}
	if got := onlyPresence(t, s).Hostname; got == nil || *got != "worker-one" {
		t.Fatalf("replay changed hostname=%v", got)
	}

	registry := routerPresenceRegistry(t, true)
	if n, err := s.DrainRouterPresenceOutbox(ctx, registry, 200); err != nil || n != 2 {
		t.Fatalf("initial transitions drained=%d err=%v", n, err)
	}
	events, err := s.RouterEvents(ctx, 0, 20)
	if err != nil || len(events) != 2 {
		t.Fatalf("initial native transitions=%+v err=%v", events, err)
	}
	for _, event := range events {
		var payload struct {
			Agent struct {
				Hostname string `json:"hostname"`
			} `json:"agent"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Agent.Hostname != "worker-one" {
			t.Fatalf("existing transition omitted hostname: %s err=%v", event.Payload, err)
		}
	}
	agent.Hostname = ptr("worker-two")
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "native", Sequence: 2, Agents: []PresenceAgent{agent}}, start.Add(time.Second)); err != nil || !ok {
		t.Fatalf("hostname update accepted=%v err=%v", ok, err)
	}
	rows := queryPresenceOutbox(t, s)
	if len(rows) != 0 {
		t.Fatalf("hostname-only report emitted transitions=%v", rows)
	}
	if after, err := s.RouterEvents(ctx, 0, 20); err != nil || len(after) != 2 {
		t.Fatalf("hostname-only report changed native events=%+v err=%v", after, err)
	}
	seen, err := s.Presence(ctx, "presence")
	if err != nil || len(seen) != 1 || seen[0].Hostname == nil || *seen[0].Hostname != "worker-two" {
		t.Fatalf("durable presence=%+v err=%v", seen, err)
	}
	live, err := s.LivePresence(ctx, start.Add(2*time.Second))
	if err != nil || len(live) != 1 || live[0].Hostname == nil || *live[0].Hostname != "worker-two" {
		t.Fatalf("live presence=%+v err=%v", live, err)
	}
}

func TestPresenceHostnameInvalidReportRollsBackAndBlankMeansNil(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-hostname-validation.db"))
	start := time.UnixMilli(1_800_810_000_000)
	for _, invalid := range []string{strings.Repeat("x", 129), "line\nbreak"} {
		agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
		agent.Hostname = &invalid
		if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "native", Sequence: 1, Agents: []PresenceAgent{agent}}, start); err == nil || ok {
			t.Fatalf("invalid hostname %q accepted=%v err=%v", invalid, ok, err)
		}
	}
	blank := " \t "
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	agent.Hostname = &blank
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "native", Sequence: 1, Agents: []PresenceAgent{agent}}, start); err != nil || !ok {
		t.Fatalf("blank hostname report accepted=%v err=%v", ok, err)
	}
	if item := onlyPresence(t, s); item.Hostname != nil {
		t.Fatalf("blank hostname persisted as %q", *item.Hostname)
	}
}
