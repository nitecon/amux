package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

var routerTaskKinds = []string{"created", "updated", "commented", "assigned", "started", "user_review", "completed", "cancelled", "unclaimed"}

func routerTaskRegistry(t *testing.T, subscribed bool) *router.Registry {
	t.Helper()
	events := RouterTaskEventDefinitions()
	plugins := []router.PluginDefinition(nil)
	if subscribed {
		plugin := router.PluginDefinition{ID: "tasks-plugin"}
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

func TestRouterTaskEventDefinitionsMatchRust(t *testing.T) {
	definitions := RouterTaskEventDefinitions()
	if len(definitions) != len(routerTaskKinds) {
		t.Fatalf("definitions count = %d", len(definitions))
	}
	for i, kind := range routerTaskKinds {
		definition := definitions[i]
		if definition.ID != "task."+kind || definition.Version != 1 || definition.RemoteEmit {
			t.Fatalf("definition[%d] = %+v", i, definition)
		}
		for _, payload := range []string{`{"project":"p","task":{}}`, `{"project":"p","task":{"n":9007199254740993}}`} {
			if err := definition.Validate(json.RawMessage(payload)); err != nil {
				t.Fatalf("%s rejected %s: %v", definition.ID, payload, err)
			}
		}
		for _, payload := range []string{`{}`, `{"project":1,"task":{}}`, `{"project":null,"task":{}}`, `{"project":"p","task":null}`, `[]`} {
			if err := definition.Validate(json.RawMessage(payload)); err == nil {
				t.Fatalf("%s accepted %s", definition.ID, payload)
			}
		}
	}
}

func TestRouterTaskOutboxNativeParentAndRootCommentSnapshots(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	actor := taskActor()
	parent := makeTask(t, s, nil)
	child, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Child", Reporter: "fixture", ParentID: &parent.ID}, actor)
	if err != nil {
		t.Fatal(err)
	}
	details, rootReason := "A durable root specification.", "user-directed"
	root, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Root", Reporter: "fixture", Details: &details, RootReason: &rootReason}, actor)
	if err != nil {
		t.Fatal(err)
	}
	var childSnapshot, rootSnapshot, commentSnapshot, commentActor []byte
	if err := s.db.QueryRowContext(ctx, `SELECT task_json FROM router_task_outbox WHERE kind='created' AND json_extract(task_json,'$.id')=?`, child.ID).Scan(&childSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT task_json FROM router_task_outbox WHERE kind='created' AND json_extract(task_json,'$.id')=?`, root.ID).Scan(&rootSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT comment_json,actor_json FROM router_task_outbox WHERE kind='commented' AND json_extract(task_json,'$.id')=?`, root.ID).Scan(&commentSnapshot, &commentActor); err != nil {
		t.Fatal(err)
	}
	var childTask Task
	if err := json.Unmarshal(childSnapshot, &childTask); err != nil || childTask.Parent == nil || childTask.Parent.ID != parent.ID {
		t.Fatalf("child outbox parent snapshot = %+v, %v", childTask.Parent, err)
	}
	var rootTask Task
	if err := json.Unmarshal(rootSnapshot, &rootTask); err != nil || rootTask.Parent != nil {
		t.Fatalf("root task snapshot parent = %+v, %v", rootTask.Parent, err)
	}
	var comment TaskComment
	var gotActor AgentOrigin
	if err := json.Unmarshal(commentSnapshot, &comment); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(commentActor, &gotActor); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(comment.Content, "Root task created (user-directed).") || gotActor != *actor {
		t.Fatalf("root comment=%+v actor=%+v", comment, gotActor)
	}
	var legacyBefore int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events`).Scan(&legacyBefore); err != nil {
		t.Fatal(err)
	}
	n, err := s.DrainRouterTaskOutbox(ctx, routerTaskRegistry(t, true), 200)
	if err != nil || n != 5 {
		var kinds []string
		rows, queryErr := s.db.QueryContext(ctx, `SELECT kind FROM router_task_outbox ORDER BY id`)
		if queryErr == nil {
			for rows.Next() {
				var kind string
				if rows.Scan(&kind) == nil {
					kinds = append(kinds, kind)
				}
			}
			rows.Close()
		}
		t.Fatalf("drain=%d, kinds=%v, query err=%v, %v", n, kinds, queryErr, err)
	}
	var legacyAfter int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events`).Scan(&legacyAfter); err != nil || legacyAfter != legacyBefore {
		t.Fatalf("legacy task journal changed from %d to %d: %v", legacyBefore, legacyAfter, err)
	}
}

func TestRouterTaskTriggersMatchRustAndDrain(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `UPDATE task_mutation_context SET origin_json=? WHERE id=1`, `{"agent":"actor"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at) VALUES('task-a','tasks','first','user',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET owner_agent_id='worker',status='in_progress' WHERE id='task-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='todo',owner_agent_id=NULL WHERE id='task-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='user_review' WHERE id='task-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET title='renamed' WHERE id='task-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO task_comments(id,task_id,author,author_type,content,created_at) VALUES('comment-a','task-a','user','user','hello',2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='done' WHERE id='task-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at) VALUES('task-b','tasks','second','user',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='cancelled' WHERE id='task-b'`); err != nil {
		t.Fatal(err)
	}
	var got []string
	rows, err := s.db.QueryContext(ctx, `SELECT kind FROM router_task_outbox ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		got = append(got, kind)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"created", "assigned", "started", "updated", "unclaimed", "updated", "updated", "user_review", "updated", "commented", "completed", "updated", "created", "cancelled", "updated"}
	sort.Strings(got)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("outbox kinds = %v, want %v", got, want)
	}

	if _, err := s.db.ExecContext(ctx, `UPDATE task_mutation_context SET origin_json=NULL WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET updated_at=updated_at+1 WHERE id='task-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE task_mutation_context SET origin_json=? WHERE id=1`, `{"agent":"actor"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET updated_at=updated_at+1 WHERE id='task-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO task_comments(id,task_id,author,author_type,content,created_at,origin_json) VALUES('comment-b','task-a','worker','agent','actor comment',3,'null')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET details=?,rank=9007199254740993 WHERE id='task-a'`, `{"huge":9007199254740993}`); err != nil {
		t.Fatal(err)
	}

	busSub := s.EventBus().Subscribe(8)
	defer busSub.Unsubscribe()
	count, err := s.DrainRouterTaskOutbox(ctx, routerTaskRegistry(t, true), 200)
	if err != nil || count != len(want)+4 {
		t.Fatalf("drain = %d, %v", count, err)
	}
	select {
	case event := <-busSub.C:
		if event.Topic != eventbus.RouterChanged {
			t.Fatalf("wake topic = %s", event.Topic)
		}
	default:
		t.Fatal("committed drain did not publish router wake")
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM router_task_outbox`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("remaining outbox rows = %d, %v", count, err)
	}
	var eventCount, deliveryCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM router_events`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM router_deliveries`).Scan(&deliveryCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != len(want)+4 || deliveryCount != eventCount {
		t.Fatalf("journal counts events=%d deliveries=%d", eventCount, deliveryCount)
	}
	var payload []byte
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM router_events WHERE id='task.updated' ORDER BY seq DESC LIMIT 1`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"rank":9007199254740993`) || !strings.Contains(string(payload), `9007199254740993`) {
		t.Fatalf("large integer lost: %s", payload)
	}
	if !strings.Contains(string(payload), `"actor":{"agent":"actor"}`) || !strings.Contains(string(payload), `"comment":null`) {
		t.Fatalf("actor/null payload = %s", payload)
	}
	var source string
	var correlation sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT source,correlation FROM router_events ORDER BY seq DESC LIMIT 1`).Scan(&source, &correlation); err != nil {
		t.Fatal(err)
	}
	if source != "tasks" || correlation.Valid {
		t.Fatalf("source=%q correlation=%+v", source, correlation)
	}
}

func TestRouterTaskOutboxRollbackAndNoSubscribers(t *testing.T) {
	for _, subscribed := range []bool{true, false} {
		t.Run(fmt.Sprintf("subscribed=%v", subscribed), func(t *testing.T) {
			s := taskStore(t)
			ctx := context.Background()
			if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at) VALUES('one','tasks','one','user',1,1),('two','tasks','two','user',1,1)`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE router_task_outbox SET task_json='{' WHERE id=(SELECT MAX(id) FROM router_task_outbox)`); err != nil {
				t.Fatal(err)
			}
			_, err := s.DrainRouterTaskOutbox(ctx, routerTaskRegistry(t, subscribed), 200)
			if err == nil {
				t.Fatal("malformed task JSON drained")
			}
			var outbox, events int
			_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM router_task_outbox`).Scan(&outbox)
			_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM router_events`).Scan(&events)
			if outbox != 2 || events != 0 {
				t.Fatalf("rollback left outbox=%d events=%d", outbox, events)
			}
		})
	}
	t.Run("no subscribers acknowledges", func(t *testing.T) {
		s := taskStore(t)
		ctx := context.Background()
		wake := s.EventBus().Subscribe(1)
		defer wake.Unsubscribe()
		if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at) VALUES('one','tasks','one','user',1,1)`); err != nil {
			t.Fatal(err)
		}
		n, err := s.DrainRouterTaskOutbox(ctx, routerTaskRegistry(t, false), 200)
		if err != nil || n != 1 {
			t.Fatalf("drain=%d, %v", n, err)
		}
		var pending int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM router_task_outbox`).Scan(&pending)
		if pending != 0 {
			t.Fatalf("unsubscribed outbox rows=%d", pending)
		}
		var stored int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM router_events`).Scan(&stored)
		if stored != 0 {
			t.Fatalf("unsubscribed events=%d", stored)
		}
		select {
		case event := <-wake.C:
			t.Fatalf("unsubscribed event emitted wake %s", event.Topic)
		default:
		}
	})
}

func TestRouterTaskOutboxMalformedOptionalJSONAndDeleteFailureRollback(t *testing.T) {
	t.Run("empty non-null optional JSON is malformed", func(t *testing.T) {
		s := taskStore(t)
		ctx := context.Background()
		if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at) VALUES('one','tasks','one','user',1,1),('two','tasks','two','user',1,1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE router_task_outbox SET comment_json='' WHERE id=(SELECT MAX(id) FROM router_task_outbox)`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DrainRouterTaskOutbox(ctx, routerTaskRegistry(t, true), 200); err == nil {
			t.Fatal("empty non-null comment JSON was treated as SQL NULL")
		}
		var outbox, events int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM router_task_outbox`).Scan(&outbox)
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM router_events`).Scan(&events)
		if outbox != 2 || events != 0 {
			t.Fatalf("failed optional JSON drain left outbox=%d events=%d", outbox, events)
		}
	})
	t.Run("delete failure rolls back events", func(t *testing.T) {
		s := taskStore(t)
		ctx := context.Background()
		if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at) VALUES('one','tasks','one','user',1,1)`); err != nil {
			t.Fatal(err)
		}
		wake := s.EventBus().Subscribe(2)
		defer wake.Unsubscribe()
		if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fixture_reject_outbox_delete BEFORE DELETE ON router_task_outbox BEGIN SELECT RAISE(ABORT,'fixture delete failure'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DrainRouterTaskOutbox(ctx, routerTaskRegistry(t, true), 200); err == nil {
			t.Fatal("delete failure committed")
		}
		var outbox, events int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM router_task_outbox`).Scan(&outbox)
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM router_events`).Scan(&events)
		if outbox != 1 || events != 0 {
			t.Fatalf("delete rollback left outbox=%d events=%d", outbox, events)
		}
		select {
		case event := <-wake.C:
			t.Fatalf("rolled-back drain published %s", event.Topic)
		default:
		}
	})
	t.Run("unknown kind fails closed", func(t *testing.T) {
		s := taskStore(t)
		ctx := context.Background()
		if _, err := s.db.ExecContext(ctx, `INSERT INTO router_task_outbox(kind,project,task_json,created_at) VALUES('future-kind','tasks','{}',1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DrainRouterTaskOutbox(ctx, routerTaskRegistry(t, true), 200); err == nil {
			t.Fatal("unknown outbox kind was classified as an event")
		}
		var outbox int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM router_task_outbox`).Scan(&outbox)
		if outbox != 1 {
			t.Fatalf("unknown outbox row was deleted: %d", outbox)
		}
	})
}

func TestRouterTaskOutboxInputValidationPaginationAndCancellation(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	for _, limit := range []int{0, -1, 201} {
		if _, err := s.DrainRouterTaskOutbox(ctx, routerTaskRegistry(t, true), limit); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
	if _, err := s.DrainRouterTaskOutbox(ctx, routerTaskRegistry(t, true), 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 205; i++ {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at) VALUES(?,?,?,?,1,1)`, fmt.Sprintf("t-%03d", i), "tasks", "task", "user"); err != nil {
			t.Fatal(err)
		}
	}
	reg := routerTaskRegistry(t, true)
	first, err := s.DrainRouterTaskOutbox(ctx, reg, 200)
	if err != nil || first != 200 {
		t.Fatalf("first page=%d, %v", first, err)
	}
	second, err := s.DrainRouterTaskOutbox(ctx, reg, 200)
	if err != nil || second != 5 {
		t.Fatalf("second page=%d, %v", second, err)
	}
	var outbox int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM router_task_outbox`).Scan(&outbox)
	if outbox != 0 {
		t.Fatalf("remaining rows=%d", outbox)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.DrainRouterTaskOutbox(cancelled, reg, 200); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}
