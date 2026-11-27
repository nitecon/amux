package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
)

func TestAssignmentFeedbackRuntimeRefusalAndNativeAdoptionBoundary(t *testing.T) {
	s, agent, first, second := assignmentPluginFixture(t)
	ctx := context.Background()
	definitions := append(append(nativePresenceDefinitions(), RouterTaskEventDefinitions()...), plugins.NotificationEventDefinitions()...)
	newRuntime := func(store *Store) *router.Runtime {
		t.Helper()
		runtime, err := router.NewRuntime(store, store.EventBus(), definitions, []router.Plugin{
			plugins.PresencePlugin(store), plugins.AssignmentPlugin(store),
			{Definition: router.PluginDefinition{ID: "notification-observer", Subscriptions: []router.Subscription{{Event: plugins.NotifyEventID, MinVersion: 1, MaxVersion: 1}}}},
		}, router.RunConfig{Capacity: 4})
		if err != nil {
			t.Fatal(err)
		}
		return runtime
	}
	runtime := newRuntime(s)
	stop := runRouterFixture(t, runtime)
	defer stop()
	tick := func() router.Event {
		t.Helper()
		result, err := s.EmitRouterEvent(ctx, runtime.Registry(), router.EmitRequest{ID: "clock.tick", Version: 1, Source: "clock", Payload: []byte(`{}`)})
		if err != nil || !result.Stored {
			t.Fatalf("tick admission: %+v %v", result, err)
		}
		waitRouterDeliveryState(t, s, result.Event.Seq, "assignment", "completed")
		return result.Event
	}
	feedback := func(source, uid, target string) {
		t.Helper()
		payload, err := json.Marshal(map[string]string{"target": target, "status": "skipped"})
		if err != nil {
			t.Fatal(err)
		}
		result, err := s.EmitRouterEvent(ctx, runtime.Registry(), router.EmitRequest{ID: plugins.NotifyResultEventID, Version: 1, Source: source, Correlation: &uid, Payload: payload})
		if err != nil || !result.Stored {
			t.Fatalf("feedback admission: %+v %v", result, err)
		}
		waitRouterDeliveryState(t, s, result.Event.Seq, "assignment", "completed")
	}
	offer := func() (string, []byte) {
		t.Helper()
		var uid string
		var payload []byte
		if err := s.db.QueryRow(`SELECT o.notify_uid,e.payload FROM assignment_offers o
			JOIN router_events e ON e.uid=o.notify_uid WHERE o.task_id=?`, first.ID).Scan(&uid, &payload); err != nil {
			t.Fatal(err)
		}
		return uid, payload
	}
	root := tick()
	uid, payload := offer()
	var notification struct {
		NewTask      bool   `json:"new_task"`
		RequireIdle  bool   `json:"require_idle"`
		ClearContext bool   `json:"clear_context"`
		Heading      string `json:"heading"`
		Instructions string `json:"instructions"`
		Event        struct {
			Kind string `json:"kind"`
		} `json:"event"`
	}
	if err := json.Unmarshal(payload, &notification); err != nil || !notification.NewTask || !notification.RequireIdle || notification.ClearContext || notification.Heading != "Queued task review" || notification.Event.Kind != "task_created" {
		t.Fatalf("assignment notification contract: %s %v", payload, err)
	}
	assertQuestionDecisionGuidance(t, "already-claimed", notification.Instructions)
	assertEqual(t, s.db, "SELECT COUNT(*) FROM task_comments WHERE task_id='"+first.ID+"' AND author='amux' AND author_type='system'", int64(1))
	var causeID, causeUID string
	if err := s.db.QueryRow(`SELECT cause.id,cause.uid FROM router_events notice
		JOIN router_events cause ON cause.uid=notice.correlation WHERE notice.uid=?`, uid).Scan(&causeID, &causeUID); err != nil {
		t.Fatal(err)
	}
	if causeID != "agent.idle" && (causeID != "clock.tick" || causeUID != root.UID) {
		t.Fatalf("unexpected assignment cause %s %s", causeID, causeUID)
	}
	var databaseNumber int
	var databaseName, path string
	if err := s.db.QueryRow("PRAGMA database_list").Scan(&databaseNumber, &databaseName, &path); err != nil {
		t.Fatal(err)
	}
	stop()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runtime = newRuntime(s)
	stop = runRouterFixture(t, runtime)
	defer stop()
	restoredUID, _ := offer()
	if restoredUID != uid {
		t.Fatal("restart lost the original notification offer")
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_assignment_offer_claims", int64(1))
	feedback("worker:spoofed", uid, agent.Origin.SessionID)
	assertEqual(t, s.db, "SELECT status FROM tasks WHERE id='"+first.ID+"'", "todo")
	assertEqual(t, s.db, "SELECT COUNT(*) FROM assignment_offers", int64(1))
	feedback("worker:assignment", uid, "wrong-target")
	assertEqual(t, s.db, "SELECT status FROM tasks WHERE id='"+first.ID+"'", "todo")
	assertEqual(t, s.db, "SELECT COUNT(*) FROM assignment_offers", int64(1))
	feedback("worker:assignment", uid, agent.Origin.SessionID)
	assertEqual(t, s.db, "SELECT status FROM tasks WHERE id='"+first.ID+"'", "todo")
	assertEqual(t, s.db, "SELECT COUNT(*) FROM assignment_offers", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_assignment_offer_claims", int64(0))
	assertEqual(t, s.db, "SELECT output_at FROM assignment_refusals WHERE session_id='"+agent.Origin.SessionID+"'", *agent.LastActivityAt)
	tick()
	assertEqual(t, s.db, "SELECT status FROM tasks WHERE id='"+first.ID+"'", "todo")
	assertEqual(t, s.db, "SELECT status FROM tasks WHERE id='"+second.ID+"'", "todo")
	output := *agent.LastActivityAt + 1
	agent.LastActivityAt = &output
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:assignment", Sequence: 2, Agents: []PresenceAgent{agent}}, time.Now()); err != nil || !accepted {
		t.Fatalf("new output report: %v %v", accepted, err)
	}
	tick()
	newUID, payload := offer()
	if uid == newUID {
		t.Fatal("new claim reused an old offer")
	}
	if err := json.Unmarshal(payload, &notification); err != nil || notification.NewTask {
		t.Fatalf("reoffered task was marked new: %s %v", payload, err)
	}
	adopted, err := s.AdoptWorkerAssignment(ctx, first.ProjectIdent, &agent.Origin)
	if err != nil || adopted == nil {
		t.Fatalf("native adoption: %+v %v", adopted, err)
	}
	feedback("worker:assignment", newUID, agent.Origin.SessionID)
	current, err := s.Task(ctx, first.ProjectIdent, first.ID)
	if err != nil || current.Task.Status != "in_progress" || current.Task.OwnerOrigin == nil || *current.Task.OwnerOrigin != agent.Origin {
		t.Fatalf("callback released adopted native work: %+v %v", current, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_assignment_runs WHERE run_id='"+adopted.RunID+"'", int64(1))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands WHERE conversation_id='"+adopted.Conversation.ID+"' AND kind='prompt'", int64(1))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM assignment_offers", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM pragma_foreign_key_check", int64(0))
}
