package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
)

func nativePresenceDefinitions() []fism.EventDefinition {
	return append(append(RouterPresenceEventDefinitions(), controllers.ClockDefinition(), controllers.ClientHeartbeatDefinition()), controllers.ActivityEventDefinitions()...)
}
func nativePresenceObserver() fism.Controller {
	subscriptions := make([]fism.Subscription, 0, 4)
	for _, id := range []string{"agent.online", "agent.busy", "agent.idle", "agent.gone"} {
		subscriptions = append(subscriptions, fism.Subscription{Event: id, MinVersion: 1, MaxVersion: 1})
	}
	return fism.Controller{Definition: fism.ControllerDefinition{ID: "observer", Subscriptions: subscriptions}}
}
func nativePresenceRegistry(t *testing.T) *fism.Registry {
	t.Helper()
	r, err := fism.NewRegistry(nativePresenceDefinitions(), []fism.ControllerDefinition{nativePresenceObserver().Definition})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPresencePluginClockTransitionIsCausalAndDoesNotRunAssignment(t *testing.T) {
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-plugin.db"))
	ctx := context.Background()
	runtime, err := fism.NewRuntime(s, s.EventBus(), nativePresenceDefinitions(), []fism.Controller{controllers.PresenceController(s), nativePresenceObserver()}, fism.RunConfig{Capacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	reportedAt := now.Add(-31 * time.Second)
	activity := reportedAt.UnixMilli()
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	agent.LastActivityAt = &activity
	agent.AutoAssign = true
	agent.ReviewContextClear = true
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:presence", Sequence: 1, Agents: []PresenceAgent{agent}}, reportedAt); err != nil || !ok {
		t.Fatalf("report %v %v", ok, err)
	}
	queued, err := s.InsertTask(ctx, TaskInsert{Project: "presence", Title: "Must remain queued", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stopSource := runOwnedRouterFixture(t, func(ctx context.Context) error {
		return controllers.RunPresenceSource(ctx, presenceSourceFixture{Store: s, runtime: runtime}, runtime.Registry(), s.EventBus())
	})
	stopRuntime := runRouterFixture(t, runtime)
	online := awaitSourceEvent(t, s, "agent.online")
	busy := awaitSourceEvent(t, s, "agent.busy")
	if online.Correlation != nil || busy.Correlation != nil {
		t.Fatalf("native report incorrectly borrowed clock cause: online=%+v busy=%+v", online, busy)
	}
	clock, err := s.EmitRouterEvent(ctx, runtime.Registry(), fism.EmitRequest{ID: "clock.tick", Version: 1, Source: "clock", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	idle := awaitSourceEvent(t, s, "agent.idle")
	if idle.Source != "plugin:presence" || idle.Correlation == nil || *idle.Correlation != clock.Event.UID {
		t.Fatalf("idle causality %+v", idle)
	}
	var payload struct {
		Agent struct {
			Project     string `json:"project"`
			SessionID   string `json:"session_id"`
			Source      string `json:"source"`
			AutoAssign  bool   `json:"auto_assign"`
			ReviewClear bool   `json:"review_context_clear"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(idle.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Agent.Project != "presence" || payload.Agent.SessionID != agent.Origin.SessionID || payload.Agent.Source != "worker:presence" || !payload.Agent.AutoAssign || !payload.Agent.ReviewClear {
		t.Fatalf("native capability snapshot %+v", payload)
	}
	waitRouterDeliveryState(t, s, clock.Event.Seq, "presence", "completed")
	live, err := s.LivePresence(ctx, time.Now())
	if err != nil || len(live) != 1 || live[0].State != "idle" {
		t.Fatalf("authoritative live snapshot %+v %v", live, err)
	}
	var owner *string
	if err := s.db.QueryRow("SELECT owner_agent_id FROM tasks WHERE id=?", queued.ID).Scan(&owner); err != nil || owner != nil {
		t.Fatalf("presence plugin invoked assignment owner=%v err=%v", owner, err)
	}
	stopSource()
	stopRuntime()
}

func TestPresenceTransitionFailureRollsBackStateClockAndReportCursor(t *testing.T) {
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-rollback.db"))
	ctx := context.Background()
	r := nativePresenceRegistry(t)
	start := time.Now().UTC()
	activity := start.UnixMilli()
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	agent.LastActivityAt = &activity
	report := PresenceReport{Source: "worker:presence", Sequence: 1, Agents: []PresenceAgent{agent}}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_presence_transition BEFORE INSERT ON amux_presence_outbox BEGIN SELECT RAISE(ABORT,'fixture presence write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReportPresence(ctx, report, start); err == nil {
		t.Fatal("report accepted without durable transition")
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_sources", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_agents", int64(0))
	if _, err := s.db.Exec("DROP TRIGGER reject_presence_transition"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReportPresence(ctx, report, start); err != nil || !ok {
		t.Fatalf("same sequence not retryable %v %v", ok, err)
	}
	if _, err := drainPresenceFixture(ctx, s, r, 200); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM router_events").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_idle_event BEFORE INSERT ON router_events WHEN NEW.id='agent.idle' BEGIN SELECT RAISE(ABORT,'fixture idle event failure'); END`); err != nil {
		t.Fatal(err)
	}
	event := fism.Event{ID: "clock.tick", Version: 1, UID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Source: "clock", EmittedAt: start.Add(31 * time.Second).UnixMilli(), Payload: []byte(`{}`)}
	if err := s.ReconcileRouterPresence(ctx, r, event); err == nil {
		t.Fatal("clock transition accepted without emitted event")
	}
	if got := onlyPresence(t, s).State; got != "busy" {
		t.Fatalf("failed transition advanced state %s", got)
	}
	assertEqual(t, s.db, "SELECT observed_at FROM amux_presence_clock WHERE id=1", start.UnixMilli())
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events", before)
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_outbox", int64(0))
	if _, err := s.db.Exec("DROP TRIGGER reject_idle_event"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterPresence(ctx, r, event); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterPresence(ctx, r, event); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events WHERE id='agent.idle'", int64(1))
}

func TestPresenceGoneDoesNotReleaseOfflineNativeTaskOwnership(t *testing.T) {
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-offline.db"))
	ctx := context.Background()
	start := time.Now().UTC()
	activity := start.UnixMilli()
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	agent.Busy = true
	agent.LastActivityAt = &activity
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:offline", Sequence: 1, Agents: []PresenceAgent{agent}}, start); err != nil || !ok {
		t.Fatalf("report %v %v", ok, err)
	}
	task, err := s.InsertTask(ctx, TaskInsert{Project: "presence", Title: "Offline execution", Reporter: "fixture"}, &agent.Origin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(ctx, "presence", task.ID, "in_progress", &agent.Origin); err != nil {
		t.Fatal(err)
	}
	tick := fism.Event{ID: "clock.tick", Version: 1, UID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Source: "clock", EmittedAt: start.Add(90 * time.Second).UnixMilli(), Payload: []byte(`{}`)}
	if err := s.ReconcileRouterPresence(ctx, nativePresenceRegistry(t), tick); err != nil {
		t.Fatal(err)
	}
	if got := onlyPresence(t, s).State; got != "gone" {
		t.Fatalf("presence did not expire: %s", got)
	}
	current, err := readTask(ctx, s.db, "presence", task.ID)
	if err != nil || current == nil || current.Status != "in_progress" || current.OwnerOrigin == nil || *current.OwnerOrigin != agent.Origin {
		t.Fatalf("presence expiry reclaimed offline execution: %+v %v", current, err)
	}
}

type gatedPresenceDrain struct {
	*Store
	ready, begun, released, finished chan struct{}
	calls                            int
}

func (p *gatedPresenceDrain) DrainRouterPresenceOutbox(ctx context.Context, r *fism.Registry, limit int) (int, error) {
	p.calls++
	if p.calls == 1 {
		n, err := drainPresenceFixture(ctx, p.Store, r, limit)
		close(p.ready)
		return n, err
	}
	if p.calls == 2 {
		close(p.begun)
		select {
		case <-p.released:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		n, err := drainPresenceFixture(ctx, p.Store, r, limit)
		if n < limit || err != nil {
			close(p.finished)
		}
		return n, err
	}
	n, err := drainPresenceFixture(ctx, p.Store, r, limit)
	if n < limit || err != nil {
		select {
		case <-p.finished:
		default:
			close(p.finished)
		}
	}
	return n, err
}

func TestPresenceClockWakeDrainsCausalRowsBeyondAtomicBatch(t *testing.T) {
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-batch.db"))
	ctx := context.Background()
	start := time.Now().UTC()
	activity := start.UnixMilli()
	agents := make([]PresenceAgent, 201)
	for i := range agents {
		agents[i] = presenceAgent(uuid.New().String(), "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
		agents[i].LastActivityAt = &activity
	}
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:bulk", Sequence: 1, Agents: agents}, start); err != nil || !ok {
		t.Fatalf("report %v %v", ok, err)
	}
	r := nativePresenceRegistry(t)
	for {
		n, err := drainPresenceFixture(ctx, s, r, 200)
		if err != nil {
			t.Fatal(err)
		}
		if n < 200 {
			break
		}
	}
	probe := &gatedPresenceDrain{Store: s, ready: make(chan struct{}), begun: make(chan struct{}), released: make(chan struct{}), finished: make(chan struct{})}
	stop := runOwnedRouterFixture(t, func(ctx context.Context) error { return controllers.RunPresenceSource(ctx, probe, r, s.EventBus()) })
	awaitRouterSignal(t, probe.ready)
	tick := fism.Event{ID: "clock.tick", Version: 1, UID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", Source: "clock", EmittedAt: start.Add(31 * time.Second).UnixMilli(), Payload: []byte(`{}`)}
	if err := s.ReconcileRouterPresence(ctx, r, tick); err != nil {
		t.Fatal(err)
	}
	awaitRouterSignal(t, probe.begun)
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_outbox", int64(201))
	close(probe.released)
	awaitRouterSignal(t, probe.finished)
	stop()
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_outbox", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events WHERE id='agent.idle'", int64(201))
	var causal int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM router_events WHERE id='agent.idle' AND correlation=?", tick.UID).Scan(&causal); err != nil || causal != 201 {
		t.Fatalf("causal rows %d %v", causal, err)
	}
}

type presenceSourceFixture struct {
	*Store
	runtime *fism.Runtime
}

func drainPresenceFixture(ctx context.Context, s *Store, registry *fism.Registry, limit int) (int, error) {
	return (presenceSourceFixture{Store: s}).DrainRouterPresenceOutbox(ctx, registry, limit)
}

func (s presenceSourceFixture) DrainRouterPresenceOutbox(ctx context.Context, registry *fism.Registry, limit int) (int, error) {
	changes, err := s.PublishPresenceChanges(ctx, registry, limit)
	if err != nil {
		return 0, err
	}
	for i, change := range changes {
		var outcomes []fism.Delivery
		if s.runtime != nil {
			outcomes, err = s.runtime.ReconcileEvent(ctx, change.Event)
			if err != nil {
				return i, err
			}
		}
		if err := s.SettlePresenceChange(ctx, change, outcomes); err != nil {
			return i, err
		}
	}
	return len(changes), nil
}
