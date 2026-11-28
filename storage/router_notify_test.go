package storage

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
)

func notificationRegistry(t *testing.T) *fism.Registry {
	t.Helper()
	definition := controllers.NotificationEventDefinitions()[0]
	registry, err := fism.NewRegistry([]fism.EventDefinition{definition}, []fism.ControllerDefinition{{
		ID:            "notification-sink",
		Subscriptions: []fism.Subscription{{Event: controllers.NotifyEventID, MinVersion: 1, MaxVersion: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestRouterNewAssignmentMatchesRustHistoryContract(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	if isNew, err := routerNewAssignment(ctx, s.db, json.RawMessage(`{"id":"missing-history","labels":[]}`)); err != nil || isNew {
		t.Fatalf("missing history = %t, %v; want false", isNew, err)
	}
	created := makeTask(t, s, nil)
	base, err := json.Marshal(map[string]any{"id": created.ID, "labels": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if isNew, err := routerNewAssignment(ctx, s.db, base); err != nil || !isNew {
		t.Fatalf("first todo history = %t, %v; want true", isNew, err)
	}
	review, err := json.Marshal(map[string]any{"id": created.ID, "labels": []string{"task-review"}})
	if err != nil {
		t.Fatal(err)
	}
	if isNew, err := routerNewAssignment(ctx, s.db, review); err != nil || isNew {
		t.Fatalf("review assignment = %t, %v; want false", isNew, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET status='in_progress' WHERE id=?", created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET status='todo' WHERE id=?", created.ID); err != nil {
		t.Fatal(err)
	}
	if isNew, err := routerNewAssignment(ctx, s.db, base); err != nil || isNew {
		t.Fatalf("real requeue history = %t, %v; want false", isNew, err)
	}
	agentCommentTask := makeTask(t, s, nil)
	agentCommentPayload, err := json.Marshal(map[string]any{"id": agentCommentTask.ID, "labels": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if isNew, err := routerNewAssignment(ctx, s.db, agentCommentPayload); err != nil || !isNew {
		t.Fatalf("task before agent comment = %t, %v; want true", isNew, err)
	}
	if _, err := insertTaskComment(ctx, s.db, agentCommentTask.ID, "agent", "agent", "Earlier work", nil, nil); err != nil {
		t.Fatal(err)
	}
	if isNew, err := routerNewAssignment(ctx, s.db, agentCommentPayload); err != nil || isNew {
		t.Fatalf("real agent comment history = %t, %v; want false", isNew, err)
	}
	for _, history := range []struct {
		kind    string
		task    string
		comment string
	}{
		{kind: "task_updated", task: `{"id":"first","status":"in_progress"}`},
		{kind: "task_unclaimed", task: `{"id":"first","status":"todo"}`},
		{kind: "task_completed", task: `{"id":"first","status":"done"}`},
		{kind: "task_commented", task: `{"id":"first","status":"todo"}`, comment: `{"author_type":"agent"}`},
	} {
		t.Run(history.kind+history.task, func(t *testing.T) {
			if _, err := s.db.ExecContext(ctx, `DELETE FROM task_events WHERE json_extract(task_json,'$.id')=? AND kind!='task_created'`, created.ID); err != nil {
				t.Fatal(err)
			}
			historyTask := strings.Replace(history.task, "first", created.ID, 1)
			if _, err := s.db.ExecContext(ctx, `INSERT INTO task_events(kind,project_ident,task_json,comment_json,created_at) VALUES(?,?,?,?,2)`, history.kind, "tasks", historyTask, nullableTestJSON(history.comment)); err != nil {
				t.Fatal(err)
			}
			if isNew, err := routerNewAssignment(ctx, s.db, base); err != nil || isNew {
				t.Fatalf("prior work history = %t, %v; want false", isNew, err)
			}
		})
	}
}

func nullableTestJSON(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func TestRegisteredNotificationHostRanksNativeAndLegacyRecords(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	if err := s.InsertProject(ctx, fixtureProject("other")); err != nil {
		t.Fatal(err)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO amux_presence_agents(project_ident,instance_id,session_id,provider,os,source,sequence,seen_at,busy,draft,state,hostname) VALUES
		('other','instance-old','sender','codex','linux','source',1,10,0,0,'idle','ExactOldHost'),
		('tasks','instance-replacement','sender','codex','linux','source',2,30,0,0,'idle','ReplacementHost')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,seen_at,hostname) VALUES
		('tasks','instance-legacy','sender','{}','legacy-generation',40,'LegacyLatest'),
		('tasks','instance-owner','owner','{}','legacy-generation',50,'OwnerHost')`); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		origin string
		want   *string
	}{
		{name: "old explicit instance beats replacement", origin: `{"session_id":"sender","instance_id":"instance-old"}`, want: ptr("ExactOldHost")},
		{name: "same project outranks cross-project", origin: `{"session_id":"sender","instance_id":"missing"}`, want: ptr("LegacyLatest")},
		{name: "missing identity", origin: `{"instance_id":"instance-old"}`},
		{name: "null origin", origin: `null`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := registeredNotificationHost(ctx, s.db, "tasks", json.RawMessage(test.origin))
			if err != nil || !sameOptionalString(got, test.want) {
				t.Fatalf("registered host = %v, %v; want %v", got, err, test.want)
			}
		})
	}
}

func TestEmitRouterNotificationSnapshotsHostsPreservesJSONAndCallerTransaction(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO amux_presence_agents(project_ident,instance_id,session_id,provider,os,source,sequence,seen_at,busy,draft,state,hostname) VALUES('tasks','old','sender','codex','linux','source',1,1,0,0,'idle','SenderMachine')`); err != nil {
		t.Fatal(err)
	}
	registry := notificationRegistry(t)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	const causeUID = "00000000-0000-4000-8000-000000000001"
	payload := json.RawMessage(`{"target":"receiver","project":"tasks","mode":"recipient","require_idle":false,"event":{"kind":"task_created","origin":{"session_id":"sender","instance_id":"old"},"sender_name":"worker","task":{"hostname":"CreationMachine","owner_origin":{"session_id":"owner","instance_id":"unknown"},"wide":9007199254740993}}}`)
	result, err := emitRouterNotificationIn(ctx, tx, registry, "notifier", causeUID, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stored || result.Event.Source != "plugin:notifier" || result.Event.Correlation == nil || *result.Event.Correlation != causeUID {
		t.Fatalf("emitted result = %+v", result)
	}
	var stored string
	if err := tx.QueryRowContext(ctx, "SELECT payload FROM router_events WHERE uid=?", result.Event.UID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stored), &decoded); err != nil {
		t.Fatal(err)
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(decoded["event"], &event); err != nil {
		t.Fatal(err)
	}
	if string(event["sender_hostname"]) != `"SenderMachine"` || string(event["owner_hostname"]) != "null" || string(event["task"]) == "" {
		t.Fatalf("host snapshots = sender %s, owner %s", event["sender_hostname"], event["owner_hostname"])
	}
	var task map[string]json.RawMessage
	if err := json.Unmarshal(event["task"], &task); err != nil {
		t.Fatal(err)
	}
	if string(task["hostname"]) != `"CreationMachine"` || !strings.Contains(string(task["wide"]), "9007199254740993") {
		t.Fatalf("task snapshot lost values: %s", event["task"])
	}
	uids := []string{result.Event.UID}
	for i, kind := range []string{"task_commented", "task_completed"} {
		cause := []string{
			"00000000-0000-4000-8000-000000000003",
			"00000000-0000-4000-8000-000000000004",
		}[i]
		nextPayload := json.RawMessage(strings.Replace(string(payload), "task_created", kind, 1))
		next, err := emitRouterNotificationIn(ctx, tx, registry, "notifier", cause, nextPayload)
		if err != nil {
			t.Fatal(err)
		}
		if next.Event.Source != "plugin:notifier" || next.Event.Correlation == nil || *next.Event.Correlation != cause {
			t.Fatalf("%s source/correlation = %q/%v", kind, next.Event.Source, next.Event.Correlation)
		}
		uids = append(uids, next.Event.UID)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for _, uid := range uids {
		var count int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM router_events WHERE uid=?", uid).Scan(&count); err != nil || count != 0 {
			t.Fatalf("caller rollback left %d notification events for %s: %v", count, uid, err)
		}
	}
}

func TestEmitRouterNotificationAcceptanceFailureRollsBackWithCaller(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE notification_business_marker(id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_notification_acceptance BEFORE INSERT ON amux_heartbeat_outbox
		BEGIN SELECT RAISE(ABORT,'injected acceptance failure'); END`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO notification_business_marker(id) VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	_, emitErr := emitRouterNotificationIn(ctx, tx, notificationRegistry(t), "notifier", "00000000-0000-4000-8000-000000000005", json.RawMessage(`{"target":"receiver","project":"tasks","mode":"recipient","require_idle":false,"event":{"kind":"task_created","task":{}}}`))
	if emitErr == nil {
		t.Fatal("expected injected acceptance failure")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		name  string
		query string
	}{
		{name: "event", query: "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'"},
		{name: "accepted notification", query: "SELECT COUNT(*) FROM amux_heartbeat_outbox"},
		{name: "business marker", query: "SELECT COUNT(*) FROM notification_business_marker"},
	} {
		var count int
		if err := s.db.QueryRowContext(ctx, check.query).Scan(&count); err != nil || count != 0 {
			t.Errorf("%s count=%d, err=%v; want 0", check.name, count, err)
		}
	}
}

func TestEmitRouterNotificationUsesServerHostForAssignedNullOrigin(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	result, err := emitRouterNotificationIn(ctx, tx, notificationRegistry(t), "assignment", "00000000-0000-4000-8000-000000000002", json.RawMessage(`{"target":"receiver","project":"tasks","mode":"assigned","require_idle":true,"event":{"kind":"task_created","origin":null,"task":{"hostname":"CreationMachine"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]json.RawMessage
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(result.Event.Payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(envelope["event"], &event); err != nil {
		t.Fatal(err)
	}
	var host, senderName string
	if err := json.Unmarshal(event["sender_hostname"], &host); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(event["sender_name"], &senderName); err != nil {
		t.Fatal(err)
	}
	serverHost, hostErr := os.Hostname()
	if hostErr != nil || host != serverHost || senderName != "amux" {
		t.Fatalf("assigned server sender host=%q name=%q; hostname=%q err=%v", host, senderName, serverHost, hostErr)
	}
}
