package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/identity/agent"
)

func sourceRegistry(t *testing.T) *fism.Registry {
	t.Helper()
	definitions := append(RouterTaskEventDefinitions(), controllers.ClockDefinition())
	subscriptions := make([]fism.Subscription, 0, len(definitions))
	for _, def := range definitions {
		subscriptions = append(subscriptions, fism.Subscription{Event: def.ID, MinVersion: 1, MaxVersion: 1})
	}
	r, err := fism.NewRegistry(definitions, []fism.ControllerDefinition{{ID: "observer", Subscriptions: subscriptions}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func awaitSourceEvent(t *testing.T, s *Store, id string) fism.Event {
	t.Helper()
	sub := s.EventBus().Subscribe(64)
	defer sub.Unsubscribe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		events, err := s.RouterEvents(ctx, 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.ID == id {
				return event
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("source did not emit %s", id)
		case _, ok := <-sub.C:
			if !ok {
				t.Fatal("source watcher closed")
			}
		}
	}
}

type sourceDrainProbe struct {
	*Store
	once    sync.Once
	drained chan struct{}
}

func (p *sourceDrainProbe) DrainRouterTaskOutbox(ctx context.Context, r *fism.Registry, limit int) (int, error) {
	count, err := p.Store.DrainRouterTaskOutbox(ctx, r, limit)
	p.once.Do(func() { close(p.drained) })
	return count, err
}

func TestTaskSourcePublishesCommittedNativeMutationsAndRestartsWithoutDuplicates(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	r := sourceRegistry(t)
	actor := taskActor()
	task := makeTask(t, s, actor)
	stop := runOwnedRouterFixture(t, func(ctx context.Context) error { return controllers.RunTaskSource(ctx, s, r, s.EventBus()) })
	created := awaitSourceEvent(t, s, "task.created")
	var payload struct {
		Project string          `json:"project"`
		Task    Task            `json:"task"`
		Actor   agent.Origin    `json:"actor"`
		Comment json.RawMessage `json:"comment"`
	}
	if err := json.Unmarshal(created.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if created.Source != "tasks" || created.Correlation != nil || payload.Project != "tasks" || payload.Task.ID != task.ID || payload.Actor != *actor || string(payload.Comment) != "null" {
		t.Fatalf("created provenance %+v payload %+v", created, payload)
	}
	if _, err := s.AddTaskComment(ctx, "tasks", task.ID, "fixture", "agent", "Native source comment", nil, actor); err != nil {
		t.Fatal(err)
	}
	commented := awaitSourceEvent(t, s, "task.commented")
	var commentPayload struct {
		Comment TaskComment  `json:"comment"`
		Actor   agent.Origin `json:"actor"`
	}
	if err := json.Unmarshal(commented.Payload, &commentPayload); err != nil {
		t.Fatal(err)
	}
	if commentPayload.Comment.Content != "Native source comment" || commentPayload.Actor != *actor {
		t.Fatalf("comment provenance %+v", commentPayload)
	}
	stop()
	assertEqual(t, s.db, "SELECT COUNT(*) FROM task_events", int64(2))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_task_outbox", int64(0))
	before, err := s.RouterEvents(ctx, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	probe := &sourceDrainProbe{Store: s, drained: make(chan struct{})}
	stopAgain := runOwnedRouterFixture(t, func(ctx context.Context) error { return controllers.RunTaskSource(ctx, probe, r, s.EventBus()) })
	awaitRouterSignal(t, probe.drained)
	stopAgain()
	after, err := s.RouterEvents(ctx, 0, 200)
	if err != nil || len(after) != len(before) {
		t.Fatalf("restart duplicated source events: before=%d after=%d %v", len(before), len(after), err)
	}
}

func TestTaskOutboxCommitFailuresRetainEntireBatchAndDoNotAnnounce(t *testing.T) {
	for _, failure := range []string{"delivery", "acknowledge", "ignored-ack"} {
		t.Run(failure, func(t *testing.T) {
			s := taskStore(t)
			makeTask(t, s, taskActor())
			second := makeTask(t, s, taskActor())
			ctx := context.Background()
			if _, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('outbox-test-reject',?)`, second.ID); err != nil {
				t.Fatal(err)
			}
			trigger := `CREATE TRIGGER reject_outbox_commit BEFORE INSERT ON router_deliveries
    WHEN json_extract((SELECT payload FROM router_events WHERE seq=NEW.event_seq),'$.task.id')=(SELECT value FROM settings WHERE key='outbox-test-reject')
    BEGIN SELECT RAISE(ABORT,'fixture delivery failure'); END;`
			if failure == "acknowledge" {
				trigger = `CREATE TRIGGER reject_outbox_commit BEFORE DELETE ON router_task_outbox BEGIN SELECT RAISE(ABORT,'fixture acknowledgement failure'); END;`
			}
			if failure == "ignored-ack" {
				trigger = `CREATE TRIGGER reject_outbox_commit BEFORE DELETE ON router_task_outbox BEGIN SELECT RAISE(IGNORE); END;`
			}
			if _, err := s.db.ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			sub := s.EventBus().Subscribe(8)
			defer sub.Unsubscribe()
			if _, err := s.DrainRouterTaskOutbox(ctx, sourceRegistry(t), 200); err == nil {
				t.Fatal("failed outbox transaction succeeded")
			}
			assertEqual(t, s.db, "SELECT COUNT(*) FROM router_task_outbox", int64(2))
			assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events", int64(0))
			assertEqual(t, s.db, "SELECT COUNT(*) FROM router_deliveries", int64(0))
			assertEqual(t, s.db, "SELECT COUNT(*) FROM task_events", int64(2))
			select {
			case <-sub.C:
				t.Fatal("failed batch announced committed work")
			default:
			}
			if _, err := s.db.ExecContext(ctx, "DROP TRIGGER reject_outbox_commit"); err != nil {
				t.Fatal(err)
			}
			n, err := s.DrainRouterTaskOutbox(ctx, sourceRegistry(t), 200)
			if err != nil || n != 2 {
				t.Fatalf("recovered drain %d %v", n, err)
			}
			assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events", int64(2))
			assertEqual(t, s.db, "SELECT COUNT(*) FROM router_task_outbox", int64(0))
		})
	}
}

func TestRolledBackTaskMutationLeavesNoRouterOutboxRows(t *testing.T) {
	s := taskStore(t)
	fixtureErr := errors.New("fixture mutation rolled back")
	err := s.taskMutation(context.Background(), taskActor(), func(tx *sql.Tx, actor *agent.Origin) error {
		if _, err := insertTaskRecord(context.Background(), tx, TaskInsert{Project: "tasks", Title: "Rollback fixture", Reporter: "fixture"}, actor, "general"); err != nil {
			return err
		}
		return fixtureErr
	})
	if !errors.Is(err, fixtureErr) {
		t.Fatalf("mutation result %v", err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM tasks", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM task_events", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_task_outbox", int64(0))
}

func TestTaskOutboxDrainsRetainedRustRowsWithoutReplacingHistory(t *testing.T) {
	ctx := context.Background()
	schema, err := os.ReadFile("testdata/task_status_legacy.sql")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rust-task-source.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(schema)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	seedRustTaskProfile(t, ctx, db)
	var pending, legacyEvents, routerEvents int64
	if err := db.QueryRow("SELECT COUNT(*) FROM router_task_outbox").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM task_events").Scan(&legacyEvents); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM router_events").Scan(&routerEvents); err != nil {
		t.Fatal(err)
	}
	if pending == 0 {
		t.Fatal("Rust fixture contains no source obligations")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n, err := s.DrainRouterTaskOutbox(ctx, sourceRegistry(t), 200)
	if err != nil || int64(n) != pending {
		t.Fatalf("retained Rust drain %d want %d: %v", n, pending, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_task_outbox", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM task_events", legacyEvents)
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events", routerEvents+pending)
	assertEqual(t, s.db, "SELECT state FROM router_deliveries WHERE plugin='fixture'", "completed")
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events WHERE source='tasks' AND json_extract(payload,'$.project')='rust-project'", pending)
}
