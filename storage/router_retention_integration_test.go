package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
)

func TestRetentionOnFullRustProfilePreservesUnansweredInputAndDelegationHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rust-retention.db")
	schema, err := os.ReadFile("testdata/task_status_legacy.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(schema)); err != nil {
		t.Fatal(err)
	}
	seedRustTaskProfile(t, ctx, db)
	original := snapshotRustSchema(t, db)
	if _, err := db.ExecContext(ctx, `INSERT INTO messages(id,project_ident,source,content,sent_at,author_kind) VALUES
		(1,'rust-project','user','Unanswered human input',1,'human'),
		(2,'rust-project','user','Acknowledged input',1,'human'),
		(3,'rust-project','agent','Delegation complete',1,'agent');
		INSERT INTO agent_confirmations(agent_id,project_ident,message_id,confirmed_at) VALUES('fixture','rust-project',2,2);
		INSERT INTO task_delegations(id,source_project_ident,source_task_id,target_project_ident,target_task_id,created_at,completed_at,completion_message_id)
		VALUES('retained-delegation','rust-project','rust-source','rust-project','rust-review',1,2,3)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertRustSchemaPreserved(t, s.db, original)
	event := router.Event{UID: uuid.NewString(), ID: plugins.RetentionStartupID, Version: 1, EmittedAt: time.Now().UnixMilli()}
	ran, deleted, err := s.ReconcileRouterRetention(ctx, event, 1, 1)
	if err != nil || !ran || deleted != 2 {
		t.Fatalf("retained profile cleanup: ran=%v deleted=%d error=%v", ran, deleted, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM messages WHERE id=1 AND content='Unanswered human input'", int64(1))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM agent_confirmations", int64(0))
	assertEqual(t, s.db, "SELECT completed_at FROM task_delegations WHERE id='retained-delegation'", int64(2))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM task_delegations WHERE completion_message_id IS NULL", int64(1))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM task_reviews WHERE source_task_id='rust-source'", int64(1))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM task_event_deliveries", int64(1))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM execution_runs WHERE id='rust-run'", int64(1))
	var violations int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("retained profile foreign keys: %d %v", violations, err)
	}
}

func TestRetentionRuntimeKeepsOfflineNativeObligationsAndActiveDelivery(t *testing.T) {
	s, convo, owner := commandConversation(t)
	ctx := context.Background()
	if _, err := s.TransitionTask(ctx, "tasks", convo.TaskID, "in_progress", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendConversationEvent(ctx, "tasks", convo.ID, uuid.NewString(), conversation.Event{Kind: "session_ready", SessionID: "native"}, owner); err != nil {
		t.Fatal(err)
	}
	command, err := s.EnqueueConversationCommand(ctx, "tasks", convo.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: "Continue offline work"})
	if err != nil {
		t.Fatal(err)
	}
	run := WorkerRun{ID: uuid.NewString(), Server: "http://127.0.0.1:7913", Project: "tasks", ConversationID: convo.ID, Directory: t.TempDir(), Owner: *owner}
	if err := s.SaveWorkerRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueWorkerEvent(ctx, run.ID, conversation.Event{Kind: "session_ready", SessionID: "native"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueWorkerEvent(ctx, run.ID, conversation.Event{Kind: "text_delta", SessionID: "native", Text: "Unsent offline result"}); err != nil {
		t.Fatal(err)
	}
	changes, err := s.ConversationCommandChanges(ctx, "tasks", convo.ID, 0, 10)
	if err != nil || len(changes) != 1 || changes[0].ID != command.ID {
		t.Fatalf("command change feed: %+v %v", changes, err)
	}
	if err := s.ObserveWorkerCommand(ctx, run.ID, changes[0]); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if _, err := s.SubscribeTaskEvents(ctx, "native-connection", "native-consumer", &zero); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OfferTaskEvent(ctx, "native-connection", "native-consumer", zero); err != nil {
		t.Fatal(err)
	}
	reviewSource := makeTask(t, s, owner)
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER hold_retention_review BEFORE INSERT ON tasks WHEN NEW.reporter='amux-autoreview' BEGIN SELECT RAISE(ABORT,'offline review obligation'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", reviewSource.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`["main.go"]`)}, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER hold_retention_review"); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	blocked := router.Plugin{
		Definition: router.PluginDefinition{ID: "blocked", Subscriptions: []router.Subscription{{Event: "fixture.work", MinVersion: 1, MaxVersion: 1}}},
		Exec: []router.Step{func(ctx context.Context, _ router.Event, _ *router.StepContext) router.Outcome {
			close(entered)
			<-ctx.Done()
			return router.Outcome{Kind: router.Continue}
		}},
	}
	definitions := append(RouterPresenceEventDefinitions(), plugins.ClockDefinition(), plugins.RetentionStartupDefinition(), router.EventDefinition{ID: "fixture.work", Version: 1, Validate: router.Object})
	runtime, err := router.NewRuntime(s, s.EventBus(), definitions, []router.Plugin{plugins.RetentionPlugin(s, 1, 1), blocked}, router.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-3 * 24 * time.Hour)
	seed := func(id string, completed bool) router.Event {
		t.Helper()
		emitted, err := s.EmitRouterEvent(ctx, runtime.Registry(), router.EmitRequest{ID: id, Version: 1, Source: "fixture", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE router_events SET emitted_at=? WHERE seq=?", old.UnixMilli(), emitted.Event.Seq); err != nil {
			t.Fatal(err)
		}
		if completed {
			if _, err := s.db.ExecContext(ctx, "UPDATE router_deliveries SET state='completed' WHERE event_seq=?", emitted.Event.Seq); err != nil {
				t.Fatal(err)
			}
		}
		emitted.Event.EmittedAt = old.UnixMilli()
		return emitted.Event
	}
	active, settled := seed("fixture.work", false), seed("fixture.work", true)
	cause, unread := seed("clock.tick", true), seed("fixture.work", true)
	// Materialize a real causal presence batch. Its tail still owes emission
	// after the first atomic 200-row drain, so cleanup must retain its cause.
	at := old.Add(-31 * time.Second)
	agents := make([]PresenceAgent, 201)
	activity := at.UnixMilli()
	for i := range agents {
		agents[i] = PresenceAgent{ProjectIdent: "tasks", Origin: AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}, LastActivityAt: &activity}
	}
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "native-retention", Sequence: 1, Agents: agents}, at); err != nil || !ok {
		t.Fatalf("native presence backlog: %v %v", ok, err)
	}
	if err := s.ReconcileRouterPresence(ctx, runtime.Registry(), cause); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_outbox WHERE correlation='"+cause.UID+"'", int64(1))
	if _, err := s.db.ExecContext(ctx, "INSERT INTO remote_consumers(consumer_id,connection_id,cursor,last_seen_at) VALUES('offline',NULL,?,?)", cause.Seq, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO messages(project_ident,source,content,sent_at) VALUES('tasks','agent','old status',?),('tasks','user','unanswered question',?)", old.UnixMilli(), old.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	protected := map[string]int64{}
	for _, table := range []string{"amux_worker_runs", "amux_worker_commands", "amux_worker_outbox", "amux_conversations", "amux_conversation_events", "amux_conversation_commands", "amux_conversation_command_changes", "task_events", "task_stream_consumers", "task_event_deliveries", "task_review_outbox", "router_task_outbox", "amux_presence_outbox", "amux_heartbeat_outbox"} {
		var count int64
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count == 0 {
			t.Fatalf("missing native fixture %s count=%d: %v", table, count, err)
		}
		protected[table] = count
	}
	stop := runRouterFixture(t, runtime)
	awaitRouterSignal(t, entered)
	startup, err := s.EmitRouterEvent(ctx, runtime.Registry(), router.EmitRequest{ID: plugins.RetentionStartupID, Version: 1, Source: "runtime", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitRouterDeliveryState(t, s, startup.Event.Seq, "retention", "completed")
	for _, event := range []router.Event{active, cause, unread} {
		var count int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM router_events WHERE seq=?", event.Seq).Scan(&count); err != nil || count != 1 {
			t.Fatalf("deleted outstanding cause/replay/work seq=%d: %d %v", event.Seq, count, err)
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM router_events WHERE seq=?", settled.Seq).Scan(&count); err != nil || count != 0 {
		t.Fatalf("settled acknowledged history survived cleanup: %d %v", count, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM messages", int64(1))
	var remaining string
	if err := s.db.QueryRowContext(ctx, "SELECT content FROM messages").Scan(&remaining); err != nil || remaining != "unanswered question" {
		t.Fatalf("lost unacknowledged human input: %q %v", remaining, err)
	}
	for table, before := range protected {
		assertEqual(t, s.db, "SELECT COUNT(*) FROM "+table, before)
	}
	current, err := s.Task(ctx, "tasks", convo.TaskID)
	if err != nil || current.Task.OwnerOrigin == nil || *current.Task.OwnerOrigin != *owner {
		t.Fatalf("retention changed offline ownership: %+v %v", current, err)
	}
	stop()
}
