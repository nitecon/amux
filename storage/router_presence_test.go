package storage

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

func routerPresenceRegistry(t *testing.T, subscribed bool) *router.Registry {
	t.Helper()
	events := RouterPresenceEventDefinitions()
	plugins := []router.PluginDefinition(nil)
	if subscribed {
		plugin := router.PluginDefinition{ID: "presence-consumer"}
		for _, event := range events {
			plugin.Subscriptions = append(plugin.Subscriptions, router.Subscription{Event: event.ID, MinVersion: 1, MaxVersion: 1})
		}
		plugins = append(plugins, plugin)
	}
	registry, err := router.NewRegistry(events, plugins)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func presenceTick(at time.Time) router.Event {
	return router.Event{UID: uuid.NewString(), ID: "clock.tick", Version: 1, Payload: json.RawMessage(`{"at":1}`), Source: "clock", EmittedAt: at.UnixMilli()}
}

func readPresenceOutboxEvents(t *testing.T, s *Store, registry *router.Registry) []router.Event {
	t.Helper()
	if _, err := s.DrainRouterPresenceOutbox(context.Background(), registry, 200); err != nil {
		t.Fatal(err)
	}
	events, err := s.RouterEvents(context.Background(), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestRouterPresenceDefinitionsAndPayloadContract(t *testing.T) {
	definitions := RouterPresenceEventDefinitions()
	want := []string{"agent.online", "agent.idle", "agent.busy", "agent.gone"}
	if len(definitions) != len(want) {
		t.Fatalf("definitions=%+v", definitions)
	}
	for i, definition := range definitions {
		if definition.ID != want[i] || definition.Version != 1 || definition.RemoteEmit {
			t.Fatalf("definition[%d]=%+v", i, definition)
		}
		if err := definition.Validate(json.RawMessage(`{"agent":{"project":"p","session_id":"s"}}`)); err != nil {
			t.Fatalf("valid payload rejected: %v", err)
		}
		for _, payload := range []string{`{}`, `{"agent":{}}`, `{"agent":{"project":null,"session_id":"s"}}`, `{"agent":{"project":"p","session_id":null}}`, `[]`} {
			if err := definition.Validate(json.RawMessage(payload)); err == nil {
				t.Fatalf("%s accepted %s", definition.ID, payload)
			}
		}
	}
}

func TestRouterPresenceOutboxTransitionsReplayAndLiveRead(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-router.db"))
	registry := routerPresenceRegistry(t, true)
	start := time.UnixMilli(1_800_400_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	activity := start.UnixMilli()
	agent.LastActivityAt = &activity
	agent.AutoAssign = true
	agent.ReviewContextClear = true
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 1, Agents: []PresenceAgent{agent}}, start); err != nil || !ok {
		t.Fatalf("report accepted=%v err=%v", ok, err)
	}
	if rows := queryPresenceOutbox(t, s); len(rows) != 2 || rows[0] != "online" || rows[1] != "busy" {
		t.Fatalf("initial known-state transitions=%v", rows)
	}
	// A replay cannot create another transition.
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 1, Agents: []PresenceAgent{agent}}, start); err != nil || ok {
		t.Fatalf("replay accepted=%v err=%v", ok, err)
	}
	if _, err := s.DrainRouterPresenceOutbox(ctx, registry, 1); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DrainRouterPresenceOutbox(ctx, registry, 200); err != nil || n != 1 {
		t.Fatalf("remaining drain=%d err=%v", n, err)
	}
	// Heartbeat-only source changes do not reannounce online; capability changes do.
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 2, Agents: []PresenceAgent{agent}}, start.Add(time.Second)); err != nil || !ok {
		t.Fatalf("heartbeat accepted=%v err=%v", ok, err)
	}
	agent.ReviewContextClear = false
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 3, Agents: []PresenceAgent{agent}}, start.Add(2*time.Second)); err != nil || !ok {
		t.Fatalf("capability report accepted=%v err=%v", ok, err)
	}
	if rows := queryPresenceOutbox(t, s); len(rows) != 1 || rows[0] != "online" {
		t.Fatalf("capability transition=%v", rows)
	}
	events := readPresenceOutboxEvents(t, s, registry)
	if len(events) != 3 || events[0].ID != "agent.online" || events[1].ID != "agent.busy" || events[2].ID != "agent.online" {
		t.Fatalf("persisted transition order=%v", eventIDs(events))
	}
	var payload struct {
		Agent map[string]any `json:"agent"`
	}
	if err := json.Unmarshal(events[2].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"project", "session_id", "instance_id", "provider", "os", "source", "busy", "draft", "auto_assign", "review_context_clear", "last_activity_at", "seen_at", "state"} {
		if _, ok := payload.Agent[key]; !ok {
			t.Errorf("event agent missing %q: %v", key, payload.Agent)
		}
	}
	if events[0].Source != "plugin:presence" || events[0].Correlation != nil {
		t.Fatalf("native source metadata=%+v", events[0])
	}

	// LivePresence is a sorted snapshot and derives TTL from the monotonic clock without changing it.
	before := readPresenceClock(t, s)
	live, err := s.LivePresence(ctx, start.Add(3*time.Second))
	if err != nil || len(live) != 1 || live[0].State != "busy" {
		t.Fatalf("live=%+v err=%v", live, err)
	}
	live, err = s.LivePresence(ctx, start.Add(200*time.Second))
	if err != nil || len(live) != 0 {
		t.Fatalf("expired live=%+v err=%v", live, err)
	}
	if after := readPresenceClock(t, s); after != before {
		t.Fatalf("LivePresence mutated clock: %d -> %d", before, after)
	}
}

func TestPresenceRoutingMetadataNormalizationReplayAndCapabilityEvents(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-routing.db"))
	start := time.UnixMilli(1_800_410_000_000)
	registry := routerPresenceRegistry(t, true)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	agent.LastActivityAt = ptrI64(start.UnixMilli())
	goal, generation, agentID := "  Ship safely  ", "cccccccc-cccc-4ccc-8ccc-cccccccccccc", "  worker-17  "
	agent.Goal, agent.Generation, agent.AgentID = &goal, &generation, &agentID
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:routing", Sequence: 4, Agents: []PresenceAgent{agent}}, start); err != nil || !accepted {
		t.Fatalf("accepted normalized routing snapshot=%v err=%v", accepted, err)
	}
	got := onlyPresence(t, s)
	if got.Goal == nil || *got.Goal != "Ship safely" || got.Generation == nil || *got.Generation != generation || got.AgentID == nil || *got.AgentID != "worker-17" {
		t.Fatalf("normalized presence routing metadata=%+v", got)
	}
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:routing", Sequence: 4, Agents: []PresenceAgent{presenceAgent(agent.Origin.SessionID, agent.Origin.InstanceID)}}, start.Add(time.Second)); err != nil || accepted {
		t.Fatalf("replay accepted=%v err=%v", accepted, err)
	}
	if got := onlyPresence(t, s); got.Goal == nil || *got.Goal != "Ship safely" || got.Generation == nil || *got.Generation != generation || got.AgentID == nil || *got.AgentID != "worker-17" {
		t.Fatalf("replay changed stored metadata=%+v", got)
	}
	if n, err := s.DrainRouterPresenceOutbox(ctx, registry, 200); err != nil || n != 2 {
		t.Fatalf("initial transitions drain=%d err=%v", n, err)
	}

	// A complete report can replace the advertised generation and invalidates
	// existing routing targets through the existing online capability event.
	newGeneration := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	agent.Generation = &newGeneration
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:routing", Sequence: 5, Agents: []PresenceAgent{agent}}, start.Add(time.Second)); err != nil || !accepted {
		t.Fatalf("generation update accepted=%v err=%v", accepted, err)
	}
	if kinds := queryPresenceOutbox(t, s); len(kinds) != 1 || kinds[0] != "online" {
		t.Fatalf("generation update transitions=%v", kinds)
	}
	if n, err := s.DrainRouterPresenceOutbox(ctx, registry, 200); err != nil || n != 1 {
		t.Fatalf("generation transition drain=%d err=%v", n, err)
	}
	events, err := s.RouterEvents(ctx, 0, 20)
	if err != nil || len(events) != 3 || events[2].ID != "agent.online" {
		t.Fatalf("generation update created a new event kind: ids=%v err=%v", eventIDs(events), err)
	}
	var transitioned struct {
		Agent struct {
			Goal       *string `json:"goal"`
			Generation *string `json:"generation"`
			AgentID    *string `json:"agent_id"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(events[2].Payload, &transitioned); err != nil || transitioned.Agent.Goal == nil || *transitioned.Agent.Goal != "Ship safely" || transitioned.Agent.Generation == nil || *transitioned.Agent.Generation != newGeneration || transitioned.Agent.AgentID == nil || *transitioned.Agent.AgentID != "worker-17" {
		t.Fatalf("durable transition metadata=%+v err=%v", transitioned, err)
	}
	live, err := s.LivePresence(ctx, start.Add(2*time.Second))
	if err != nil || len(live) != 1 || live[0].Generation == nil || *live[0].Generation != newGeneration {
		t.Fatalf("live typed metadata=%+v err=%v", live, err)
	}
	var heartbeat []byte
	if err := s.db.QueryRowContext(ctx, "SELECT payload FROM amux_heartbeat_outbox ORDER BY id DESC LIMIT 1").Scan(&heartbeat); err != nil {
		t.Fatal(err)
	}
	var flattened struct {
		Agents []struct {
			Goal       *string `json:"goal"`
			Generation *string `json:"generation"`
			AgentID    *string `json:"agent_id"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(heartbeat, &flattened); err != nil || len(flattened.Agents) != 1 || flattened.Agents[0].Generation == nil || *flattened.Agents[0].Generation != newGeneration || flattened.Agents[0].AgentID == nil || *flattened.Agents[0].AgentID != "worker-17" {
		t.Fatalf("flattened heartbeat routing metadata=%+v err=%v", flattened, err)
	}

	for name, field := range map[string]func(*PresenceAgent){
		"goal":             func(item *PresenceAgent) { bad := strings.Repeat("x", 129); item.Goal = &bad },
		"generation":       func(item *PresenceAgent) { bad := "not-a-uuid"; item.Generation = &bad },
		"agent id length":  func(item *PresenceAgent) { bad := strings.Repeat("x", 129); item.AgentID = &bad },
		"agent id control": func(item *PresenceAgent) { bad := "bad\nvalue"; item.AgentID = &bad },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := presenceAgent(agent.Origin.SessionID, agent.Origin.InstanceID)
			field(&invalid)
			if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:routing", Sequence: 6, Agents: []PresenceAgent{invalid}}, start.Add(2*time.Second)); accepted || !errors.Is(err, ErrPresenceInput) {
				t.Fatalf("bad routing report accepted=%v err=%v", accepted, err)
			}
		})
	}
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:routing", Sequence: 6, Agents: []PresenceAgent{agent}}, start.Add(2*time.Second)); err != nil || !accepted {
		t.Fatalf("invalid reports advanced cursor: accepted=%v err=%v", accepted, err)
	}
}

func TestPresenceRoutingMetadataMigrationFromVersion20RetainsRowsAndCursor(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "presence-v20.db")
	s := presenceStore(t, path)
	at := time.UnixMilli(1_800_420_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "retained-v20", Sequence: 12, Agents: []PresenceAgent{agent}}, at); err != nil || !accepted {
		t.Fatalf("seed version-20 row accepted=%v err=%v", accepted, err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE amux_presence_agents DROP COLUMN goal;
		ALTER TABLE amux_presence_agents DROP COLUMN generation;
		ALTER TABLE amux_presence_agents DROP COLUMN agent_id;
		DELETE FROM amux_go_migrations WHERE version>=21`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade version 20 database: %v", err)
	}
	defer upgraded.Close()
	items, err := upgraded.Presence(ctx, "presence")
	if err != nil || len(items) != 1 || items[0].SeenAt != at.UnixMilli() || items[0].Goal != nil || items[0].Generation != nil || items[0].AgentID != nil {
		t.Fatalf("upgraded presence rows=%+v err=%v", items, err)
	}
	if accepted, err := upgraded.ReportPresence(ctx, PresenceReport{Source: "retained-v20", Sequence: 12, Agents: []PresenceAgent{agent}}, at.Add(time.Second)); err != nil || accepted {
		t.Fatalf("migration lost replay cursor: accepted=%v err=%v", accepted, err)
	}
	if accepted, err := upgraded.ReportPresence(ctx, PresenceReport{Source: "retained-v20", Sequence: 13, Agents: []PresenceAgent{agent}}, at.Add(time.Second)); err != nil || !accepted {
		t.Fatalf("new post-upgrade report accepted=%v err=%v", accepted, err)
	}
}

func TestRouterPresenceSourceTakeoverUnknownStaleAndOmission(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-takeover.db"))
	start := time.UnixMilli(1_800_500_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 1, Agents: []PresenceAgent{agent}}, start); err != nil || !ok {
		t.Fatalf("unknown report=%v %v", ok, err)
	}
	if rows := queryPresenceOutbox(t, s); len(rows) != 1 || rows[0] != "online" {
		t.Fatalf("unknown arrival transitions=%v", rows)
	}
	if err := s.ReconcilePresence(ctx, start.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if rows := queryPresenceOutbox(t, s); len(rows) != 2 || rows[1] != "gone" {
		t.Fatalf("expiry transitions=%v", rows)
	}
	if err := s.ReconcilePresence(ctx, start.Add(180*time.Second)); err != nil {
		t.Fatal(err)
	}
	if rows := queryPresenceOutbox(t, s); len(rows) != 2 {
		t.Fatalf("gone was emitted more than once: %v", rows)
	}
	// A fresh source rejoining emits online before its known state.
	agent.LastActivityAt = ptrI64(start.UnixMilli())
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:b", Sequence: 1, Agents: []PresenceAgent{agent}}, start.Add(181*time.Second)); err != nil || !ok {
		t.Fatalf("takeover=%v %v", ok, err)
	}
	if rows := queryPresenceOutbox(t, s); len(rows) != 4 || rows[2] != "online" || rows[3] != "idle" {
		t.Fatalf("takeover transitions=%v", rows)
	}
	// A fresh but already-expired arrival must remain gone without online/gone.
	stale := presenceAgent("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:c", Sequence: 1, Agents: []PresenceAgent{stale}}, start.Add(-time.Second)); err != nil || !ok {
		t.Fatalf("stale report=%v %v", ok, err)
	}
	if got := queryPresenceOutbox(t, s); len(got) != 4 {
		t.Fatalf("never-live stale arrival created events: %v", got)
	}
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:b", Sequence: 2}, start.Add(182*time.Second)); err != nil || !ok {
		t.Fatalf("omission report=%v %v", ok, err)
	}
	if got := queryPresenceOutbox(t, s); len(got) != 5 || got[4] != "gone" {
		t.Fatalf("omission transitions=%v", got)
	}
}

func TestRouterPresenceUnknownBecomesKnownWithoutOnlineAndExpiredPersistedRowGoesGone(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-unknown-known.db"))
	start := time.UnixMilli(1_800_550_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 1, Agents: []PresenceAgent{agent}}, start); err != nil || !ok {
		t.Fatalf("unknown report=%v %v", ok, err)
	}
	agent.LastActivityAt = ptrI64(start.UnixMilli())
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 2, Agents: []PresenceAgent{agent}}, start.Add(time.Second)); err != nil || !ok {
		t.Fatalf("known report=%v %v", ok, err)
	}
	if got := queryPresenceOutbox(t, s); len(got) != 2 || got[0] != "online" || got[1] != "busy" {
		t.Fatalf("unknown-to-known transitions=%v", got)
	}

	// Advance the global clock via a different current report without reconciling the first row.
	other := presenceAgent("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	other.LastActivityAt = ptrI64(start.Add(91 * time.Second).UnixMilli())
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:b", Sequence: 1, Agents: []PresenceAgent{other}}, start.Add(91*time.Second)); err != nil || !ok {
		t.Fatalf("clock advance report=%v %v", ok, err)
	}
	if err := s.ReconcilePresence(ctx, start.Add(91*time.Second)); err != nil {
		t.Fatal(err)
	}
	var oldState string
	if err := s.db.QueryRowContext(ctx, "SELECT state FROM amux_presence_agents WHERE session_id=?", agent.Origin.SessionID).Scan(&oldState); err != nil || oldState != "gone" {
		t.Fatalf("expired persisted state=%q err=%v", oldState, err)
	}
	got := queryPresenceOutbox(t, s)
	if len(got) != 5 || got[4] != "gone" {
		t.Fatalf("expired persisted transitions=%v", got)
	}
}

func TestRouterPresenceTickCausalityAndRollback(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-tick.db"))
	registry := routerPresenceRegistry(t, true)
	start := time.UnixMilli(1_800_600_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	activity := start.UnixMilli()
	agent.LastActivityAt = &activity
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 1, Agents: []PresenceAgent{agent}}, start); err != nil || !ok {
		t.Fatalf("report=%v %v", ok, err)
	}
	if n, err := s.DrainRouterPresenceOutbox(ctx, registry, 200); err != nil || n != 2 {
		t.Fatalf("native transition drain=%d err=%v", n, err)
	}
	changes := s.EventBus().Subscribe(4)
	defer changes.Unsubscribe()
	clock := presenceTick(start.Add(30 * time.Second))
	if err := s.ReconcileRouterPresence(ctx, registry, clock); err != nil {
		t.Fatal(err)
	}
	seenRouterChanged, seenPresenceChanged := false, false
	for i := 0; i < 2; i++ {
		select {
		case change := <-changes.C:
			seenRouterChanged = seenRouterChanged || change.Topic == eventbus.RouterChanged
			seenPresenceChanged = seenPresenceChanged || change.Topic == eventbus.PresenceChanged
		case <-time.After(time.Second):
			t.Fatal("clock reconciliation did not publish both committed change notifications")
		}
	}
	if !seenRouterChanged || !seenPresenceChanged {
		t.Fatalf("committed notifications router=%v presence=%v", seenRouterChanged, seenPresenceChanged)
	}
	events, err := s.RouterEvents(ctx, 0, 20)
	if err != nil || len(events) != 3 || events[2].ID != "agent.idle" {
		t.Fatalf("tick events=%v err=%v", eventIDs(events), err)
	}
	if events[2].Source != "plugin:presence" || events[2].Correlation == nil || *events[2].Correlation != clock.UID {
		t.Fatalf("tick event causality=%+v", events[2])
	}
	if err := s.db.QueryRowContext(ctx, "SELECT observed_at FROM amux_presence_clock WHERE id=1").Scan(new(int64)); err != nil {
		t.Fatal(err)
	}

	// An invalid clock-tick event still advances/evaluates atomically only when all outputs can persist.
	rollbackStore := presenceStore(t, filepath.Join(t.TempDir(), "presence-rollback.db"))
	if ok, err := rollbackStore.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 1, Agents: []PresenceAgent{agent}}, start); err != nil || !ok {
		t.Fatalf("rollback fixture report=%v %v", ok, err)
	}
	if _, err := rollbackStore.db.ExecContext(ctx, `CREATE TRIGGER fail_presence_idle BEFORE INSERT ON router_events WHEN NEW.id='agent.idle' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	failedTick := presenceTick(start.Add(30 * time.Second))
	if err := rollbackStore.ReconcileRouterPresence(ctx, registry, failedTick); err == nil {
		t.Fatal("tick unexpectedly succeeded after injected event insert failure")
	}
	if got := onlyPresence(t, rollbackStore).State; got != "busy" {
		t.Fatalf("failed tick changed state to %q", got)
	}
	if pending := queryPresenceOutbox(t, rollbackStore); len(pending) != 2 {
		t.Fatalf("failed tick did not retain causal events: %v", pending)
	}
}

func TestRouterPresenceOutboxAckRollbackAndUnsubscribedDrain(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-ack.db"))
	start := time.UnixMilli(1_800_700_000_000)
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	agent.LastActivityAt = ptrI64(start.UnixMilli())
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 1, Agents: []PresenceAgent{agent}}, start); err != nil || !ok {
		t.Fatalf("report=%v %v", ok, err)
	}
	if n, err := s.DrainRouterPresenceOutbox(ctx, routerPresenceRegistry(t, false), 200); err != nil || n != 2 {
		t.Fatalf("unsubscribed drain=%d err=%v", n, err)
	}
	if count := countRouterEvents(t, s); count != 0 {
		t.Fatalf("unsubscribed events persisted=%d", count)
	}
	if pending := queryPresenceOutbox(t, s); len(pending) != 0 {
		t.Fatalf("unsubscribed rows not acknowledged: %v", pending)
	}

	rollbackStore := presenceStore(t, filepath.Join(t.TempDir(), "presence-ack-rollback.db"))
	if ok, err := rollbackStore.ReportPresence(ctx, PresenceReport{Source: "worker:a", Sequence: 1, Agents: []PresenceAgent{agent}}, start); err != nil || !ok {
		t.Fatalf("rollback fixture=%v %v", ok, err)
	}
	if _, err := rollbackStore.db.ExecContext(ctx, `CREATE TRIGGER fail_presence_delivery BEFORE INSERT ON router_deliveries BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := rollbackStore.DrainRouterPresenceOutbox(ctx, routerPresenceRegistry(t, true), 200); err == nil {
		t.Fatal("drain unexpectedly succeeded after injected delivery failure")
	}
	if pending := queryPresenceOutbox(t, rollbackStore); len(pending) != 2 || countRouterEvents(t, rollbackStore) != 0 {
		t.Fatalf("failed ack did not rollback events/outbox: pending=%v", pending)
	}
}

func queryPresenceOutbox(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), "SELECT kind FROM amux_presence_outbox ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return kinds
}

func readPresenceClock(t *testing.T, s *Store) int64 {
	t.Helper()
	var value int64
	if err := s.db.QueryRowContext(context.Background(), "SELECT observed_at FROM amux_presence_clock WHERE id=1").Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func countRouterEvents(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	if err := s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM router_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func eventIDs(events []router.Event) []string {
	ids := make([]string, len(events))
	for i, event := range events {
		ids[i] = event.ID
	}
	return ids
}

func ptrI64(value int64) *int64 { return &value }
