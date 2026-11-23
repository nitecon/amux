package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/router"
)

const notifierOwnerSession = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const notifierOwnerInstance = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
const notifierRequesterSession = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
const notifierRequesterInstance = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"

func notifierTestAgent(session, instance string) notifierAgent {
	return notifierAgent{SessionID: session, InstanceID: instance, OS: "linux"}
}

func notifierRegistry(t *testing.T) *router.Registry {
	t.Helper()
	return notificationRegistry(t)
}

func notifierCause(t *testing.T, s *Store, id string, payload string) router.Event {
	t.Helper()
	uid := uuid.NewString()
	result, err := s.db.ExecContext(context.Background(), `INSERT INTO router_events(uid,id,version,payload,source,emitted_at)
		VALUES(?,?,1,?,'tasks',?)`, uid, id, payload, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	seq, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return router.Event{Seq: seq, UID: uid, ID: id, Version: 1, Payload: json.RawMessage(payload), Source: "tasks", EmittedAt: time.Now().UnixMilli()}
}

func notifierPayload(task, actor string) string {
	return `{"project":"tasks","task":` + task + `,"comment":{"body":"ok","ordinal":9007199254740993},"actor":` + actor + `}`
}

func TestRouterNotifierAudienceMatchesRustRules(t *testing.T) {
	owner := notifierTestAgent(notifierOwnerSession, notifierOwnerInstance)
	cases := []struct {
		name  string
		kind  string
		task  string
		agent notifierAgent
		actor string
		want  bool
	}{
		{"exact owner receives comment", "task_commented", `{"status":"in_progress","owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"}}`, owner, `null`, true},
		{"other instance is excluded", "task_commented", `{"owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"other"}}`, owner, `null`, false},
		{"actor is excluded", "task_commented", `{"owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"}}`, owner, `{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"}`, false},
		{"user review suppresses owner", "task_commented", `{"status":"user_review","owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"}}`, owner, `null`, false},
		{"explicit unassigned task remains queued", "task_created", `{"status":"todo","routing":{"mode":"unassigned"}}`, owner, `null`, false},
		{"explicit unassigned suppresses even stale owner snapshot", "task_created", `{"status":"todo","owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"routing":{"mode":"unassigned"}}`, owner, `null`, false},
		{"ownerless auto task remains queued", "task_created", `{"status":"todo","routing":{"mode":"auto"}}`, owner, `null`, false},
		{"ownerless legacy task remains queued", "task_created", `{"status":"todo"}`, owner, `null`, false},
		{"ownerless unrelated event remains suppressed", "task_commented", `{"status":"todo","routing":{"mode":"broadcast"}}`, owner, `null`, false},
		{"ownerless broadcast task notifies compatible audience", "task_created", `{"status":"todo","routing":{"mode":"broadcast"}}`, owner, `null`, true},
		{"broadcast still excludes event actor", "task_created", `{"status":"todo","routing":{"mode":"broadcast"}}`, owner, `{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"}`, false},
		{"broadcast still enforces OS requirement", "task_created", `{"status":"todo","labels":["os:windows"],"routing":{"mode":"broadcast"}}`, owner, `null`, false},
		{"broadcast still enforces goal audience", "task_created", `{"status":"todo","goal":"ship","routing":{"mode":"broadcast"}}`, owner, `null`, false},
		{"owned by agent id alias", "task_commented", `{"status":"in_progress","owner_agent_id":"legacy-id"}`, func() notifierAgent { a := owner; id := "legacy-id"; a.AgentID = &id; return a }(), `null`, true},
		{"owned by legacy session alias", "task_commented", `{"status":"in_progress","owner_agent_id":"` + notifierOwnerSession + `"}`, owner, `null`, true},
		{"owner origin object takes precedence over alias", "task_commented", `{"status":"in_progress","owner_origin":{},"owner_agent_id":"` + notifierOwnerSession + `"}`, owner, `null`, false},
		{"requester result precedes review suppression", "task_completed", `{"status":"user_review","owner_origin":{"session_id":"other","instance_id":"old"},"requester":{"origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"generation":null}}`, owner, `null`, true},
		{"owner still receives own completion outside requester result path", "task_completed", `{"status":"done","owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"requester":{"origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"generation":null}}`, owner, `null`, true},
		{"requester generation mismatch", "task_completed", `{"status":"done","owner_origin":{"session_id":"other","instance_id":"old"},"requester":{"origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"generation":"old"}}`, owner, `null`, false},
		{"goal must match", "task_commented", `{"status":"in_progress","owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"goal":"ship"}`, owner, `null`, false},
		{"matching goal", "task_commented", `{"status":"in_progress","owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"goal":"ship"}`, func() notifierAgent { a := owner; goal := "ship"; a.Goal = &goal; return a }(), `null`, true},
		{"null goal matches absent agent goal", "task_commented", `{"status":"in_progress","owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"goal":null}`, owner, `null`, true},
		{"OS label conflict", "task_commented", `{"status":"in_progress","owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"labels":["os:windows"]}`, owner, `null`, false},
		{"conflicting OS labels fail closed", "task_commented", `{"status":"in_progress","owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"labels":["os:linux","os:windows"]}`, owner, `null`, false},
		{"malformed label types fail closed", "task_commented", `{"status":"in_progress","owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"labels":[3]}`, owner, `null`, false},
		{"session routing exact execution and null generation", "task_created", `{"status":"todo","owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"routing":{"mode":"session","target":{"origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"generation":null}}}`, owner, `null`, true},
		{"session routing stale generation", "task_created", `{"status":"todo","owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"routing":{"mode":"session","target":{"origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"generation":"old"}}}`, owner, `null`, false},
		{"session routing wrong instance", "task_created", `{"status":"todo","owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"routing":{"mode":"session","target":{"origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"old"},"generation":null}}}`, owner, `null`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routerNotifierRecipient(tc.kind, json.RawMessage(tc.task), json.RawMessage(tc.actor), tc.agent); got != tc.want {
				t.Fatalf("recipient = %t, want %t", got, tc.want)
			}
		})
	}
	ownerRequester := json.RawMessage(`{"owner_origin":{"session_id":"other","instance_id":"old"},"requester":{"origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"generation":null}}`)
	if routerNotifierRequesterResult("task_commented", ownerRequester, owner) != true {
		t.Fatal("requester comment must be idle-gated when both generations are null")
	}
	if routerNotifierRequesterResult("task_completed", json.RawMessage(`{"owner_origin":{"session_id":"`+notifierOwnerSession+`","instance_id":"`+notifierOwnerInstance+`"},"requester":{"origin":{"session_id":"`+notifierOwnerSession+`","instance_id":"`+notifierOwnerInstance+`"},"generation":null}}`), owner) {
		t.Fatal("requester result path must exclude the current owner")
	}
	if routerNotifierRecipient("task_commented", json.RawMessage(`{"status":"in_progress","owner_origin":{"session_id":"`+notifierOwnerSession+`","instance_id":"`+notifierOwnerInstance+`"},"labels":null}`), json.RawMessage(`null`), owner) != true {
		t.Fatal("null labels should remain unrestricted")
	}
	if routerNotifierRecipient("task_commented", json.RawMessage(`null`), json.RawMessage(`null`), owner) {
		t.Fatal("null task must fail closed")
	}
}

func TestReconcileRouterNotifiersUsesLivePresenceAndEnabledSetting(t *testing.T) {
	s := taskStore(t)
	defer s.Close()
	ctx := context.Background()
	registry := notifierRegistry(t)
	now := time.Now().UTC()
	owner := PresenceAgent{ProjectIdent: "tasks", Origin: AgentOrigin{SessionID: notifierOwnerSession, InstanceID: notifierOwnerInstance, Provider: "codex", OS: "linux"}}
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "notifier-native", Sequence: 1, Agents: []PresenceAgent{owner}}, now); err != nil || !ok {
		t.Fatalf("presence accepted=%t err=%v", ok, err)
	}
	if _, err := s.db.ExecContext(ctx, "CREATE TABLE notifier_agents(session_id TEXT PRIMARY KEY); INSERT INTO notifier_agents VALUES('departed-native-session')"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateRouterNotifier(ctx, tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var legacyRows int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notifier_agents WHERE session_id='departed-native-session'").Scan(&legacyRows); err != nil || legacyRows != 1 {
		t.Fatalf("legacy projection rows=%d err=%v; migration must leave legacy user data intact", legacyRows, err)
	}
	assertNoticeCount := func(want int) {
		t.Helper()
		var count int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'").Scan(&count); err != nil || count != want {
			t.Fatalf("notices=%d want=%d err=%v", count, want, err)
		}
	}
	legacyEvent := notifierCause(t, s, "task.commented", eventPayloadOwner("departed-native-session", "departed-instance"))
	if err := s.ReconcileRouterNotifiers(ctx, registry, legacyEvent); err != nil {
		t.Fatal(err)
	}
	assertNoticeCount(0)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('plugin.notifier.enabled','false')"); err != nil {
		t.Fatal(err)
	}
	event := notifierCause(t, s, "task.commented", notifierPayload(`{"status":"in_progress","owner_origin":{"session_id":"`+notifierOwnerSession+`","instance_id":"`+notifierOwnerInstance+`"}}`, `null`))
	if err := s.ReconcileRouterNotifiers(ctx, nil, event); err == nil {
		t.Fatal("nil registry should fail before recording a receipt")
	}
	var receiptCount int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notifier_processed WHERE event_seq=?", event.Seq).Scan(&receiptCount); err != nil || receiptCount != 0 {
		t.Fatalf("nil registry left receipt=%d err=%v", receiptCount, err)
	}
	if err := s.ReconcileRouterNotifiers(ctx, registry, event); err != nil {
		t.Fatal(err)
	}
	assertNoticeCount(0)
	if _, err := s.db.ExecContext(ctx, "UPDATE settings SET value='true' WHERE key='plugin.notifier.enabled'"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterNotifiers(ctx, registry, event); err != nil {
		t.Fatal(err)
	}
	assertNoticeCount(1)
	replacement := owner
	replacement.Origin.InstanceID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "notifier-native", Sequence: 2, Agents: []PresenceAgent{replacement}}, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("replacement presence accepted=%t err=%v", ok, err)
	}
	changed := notifierCause(t, s, "task.commented", eventPayloadOwner(notifierOwnerSession, notifierOwnerInstance))
	if err := s.ReconcileRouterNotifiers(ctx, registry, changed); err != nil {
		t.Fatal(err)
	}
	assertNoticeCount(1)
	current := notifierCause(t, s, "task.commented", eventPayloadOwner(notifierOwnerSession, replacement.Origin.InstanceID))
	if err := s.ReconcileRouterNotifiers(ctx, registry, current); err != nil {
		t.Fatal(err)
	}
	assertNoticeCount(2)
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "notifier-native", Sequence: 3}, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("departure snapshot accepted=%t err=%v", ok, err)
	}
	departed := notifierCause(t, s, "task.commented", eventPayloadOwner(notifierOwnerSession, replacement.Origin.InstanceID))
	if err := s.ReconcileRouterNotifiers(ctx, registry, departed); err != nil {
		t.Fatal(err)
	}
	assertNoticeCount(2)
}

func TestReconcileRouterNotifiersBroadcastsOwnerlessTasksButSuppressesUnassigned(t *testing.T) {
	s := taskStore(t)
	defer s.Close()
	ctx := context.Background()
	registry := notifierRegistry(t)
	owner := PresenceAgent{ProjectIdent: "tasks", Origin: AgentOrigin{SessionID: notifierOwnerSession, InstanceID: notifierOwnerInstance, Provider: "codex", OS: "linux"}}
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "notifier-routing", Sequence: 1, Agents: []PresenceAgent{owner}}, time.Now().UTC()); err != nil || !accepted {
		t.Fatalf("presence accepted=%t err=%v", accepted, err)
	}
	for _, tc := range []struct {
		name, mode string
		want       int
	}{
		{name: "broadcast", mode: "broadcast", want: 1},
		{name: "unassigned", mode: "unassigned", want: 1},
		{name: "auto", mode: "auto", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := notifierCause(t, s, "task.created", notifierPayload(`{"status":"todo","routing":{"mode":"`+tc.mode+`"}}`, `null`))
			if err := s.ReconcileRouterNotifiers(ctx, registry, event); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != tc.want {
				t.Fatalf("stored notices=%d want=%d", count, tc.want)
			}
		})
	}
	unrelated := notifierCause(t, s, "task.commented", notifierPayload(`{"status":"todo","routing":{"mode":"broadcast"}}`, `null`))
	if err := s.ReconcileRouterNotifiers(ctx, registry, unrelated); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("unrelated ownerless TODO notifications=%d err=%v; want broadcast-only notice", count, err)
	}
}

func TestRouterNotifierPresenceTTLBoundary(t *testing.T) {
	s := taskStore(t)
	defer s.Close()
	ctx := context.Background()
	start := time.UnixMilli(100_000)
	agent := PresenceAgent{ProjectIdent: "tasks", Origin: AgentOrigin{SessionID: notifierOwnerSession, InstanceID: notifierOwnerInstance, Provider: "codex", OS: "linux"}}
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "notifier-ttl", Sequence: 1, Agents: []PresenceAgent{agent}}, start); err != nil || !accepted {
		t.Fatalf("seed presence accepted=%t err=%v", accepted, err)
	}
	inside, _, err := livePresenceSnapshot(ctx, s.db, start.UnixMilli()+presenceGoneAfter-1)
	if err != nil || len(inside) != 1 {
		t.Fatalf("presence one millisecond inside TTL=%d err=%v", len(inside), err)
	}
	boundary, _, err := livePresenceSnapshot(ctx, s.db, start.UnixMilli()+presenceGoneAfter)
	if err != nil || len(boundary) != 0 {
		t.Fatalf("presence at exact TTL boundary=%d err=%v; want expired", len(boundary), err)
	}
}

func eventPayloadOwner(session, instance string) string {
	return notifierPayload(`{"status":"in_progress","owner_origin":{"session_id":"`+session+`","instance_id":"`+instance+`"}}`, `null`)
}

func TestReconcileRouterNotifiersRollsBackRetriesAndPreservesRawSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifier.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(context.Background(), fixtureProject("tasks")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	registry := notifierRegistry(t)
	now := time.Now().UTC()
	ownerHost, requesterHost := "OwnerMachine", "RequesterMachine"
	owner := PresenceAgent{ProjectIdent: "tasks", Origin: AgentOrigin{SessionID: notifierOwnerSession, InstanceID: notifierOwnerInstance, Provider: "codex", OS: "linux"}, Hostname: &ownerHost}
	requester := PresenceAgent{ProjectIdent: "tasks", Origin: AgentOrigin{SessionID: notifierRequesterSession, InstanceID: notifierRequesterInstance, Provider: "codex", OS: "linux"}, Hostname: &requesterHost}
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "notifier-native", Sequence: 1, Agents: []PresenceAgent{owner, requester}}, now); err != nil || !accepted {
		t.Fatalf("report presence accepted=%t err=%v", accepted, err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_requester_notice BEFORE INSERT ON router_events
		WHEN NEW.id='agent.notify' AND json_extract(NEW.payload,'$.target')='`+notifierRequesterSession+`'
		BEGIN SELECT RAISE(ABORT,'fixture second recipient failed'); END`); err != nil {
		t.Fatal(err)
	}
	rawTask := `{"id":"giant","status":"done","owner_origin":{"session_id":"` + notifierOwnerSession + `","instance_id":"` + notifierOwnerInstance + `"},"requester":{"origin":{"session_id":"` + notifierRequesterSession + `","instance_id":"` + notifierRequesterInstance + `"},"generation":null},"rank":9007199254740993,"labels":[]}`
	rawActor := `{"kind":"user","ordinal":9007199254740995}`
	event := notifierCause(t, s, "task.completed", notifierPayload(rawTask, rawActor))
	if err := s.ReconcileRouterNotifiers(ctx, registry, event); err == nil || !strings.Contains(err.Error(), "fixture second recipient failed") {
		t.Fatalf("injected recipient failure err=%v", err)
	}
	var notices, receipts int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'").Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notifier_processed WHERE event_seq=?", event.Seq).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if notices != 0 || receipts != 0 {
		t.Fatalf("failed transaction left notices=%d receipt=%d", notices, receipts)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_requester_notice"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterNotifiers(ctx, registry, event); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ReconcileRouterNotifiers(ctx, registry, event); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'").Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notifier_processed WHERE event_seq=?", event.Seq).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if notices != 2 || receipts != 1 {
		t.Fatalf("successful replay notices=%d receipt=%d", notices, receipts)
	}
	var notificationPayload string
	if err := s.db.QueryRowContext(ctx, "SELECT payload FROM router_events WHERE id='agent.notify' ORDER BY seq LIMIT 1").Scan(&notificationPayload); err != nil {
		t.Fatal(err)
	}
	for _, exact := range []string{"9007199254740993", "9007199254740995", `"origin":{"kind":"user","ordinal":9007199254740995}`, "OwnerMachine"} {
		if !strings.Contains(notificationPayload, exact) {
			t.Fatalf("notification payload lost %q: %s", exact, notificationPayload)
		}
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM router_events WHERE seq=?", event.Seq); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notifier_processed WHERE event_seq=?", event.Seq).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("cause deletion receipt count=%d err=%v", receipts, err)
	}
}

func TestRouterNotifierNewTaskIsBasedOnlyOnCreatedEventAndReviewLabel(t *testing.T) {
	for _, tc := range []struct {
		id     string
		labels string
		want   bool
	}{
		{"task.created", `[]`, true},
		{"task.created", `["task-review"]`, false},
		{"task.commented", `[]`, false},
		{"task.created", `[3]`, false},
	} {
		t.Run(tc.id+tc.labels, func(t *testing.T) {
			task := json.RawMessage(`{"status":"todo","labels":` + tc.labels + `}`)
			if got := routerNotifierNewTask(tc.id, task); got != tc.want {
				t.Fatalf("new_task=%t want=%t", got, tc.want)
			}
		})
	}
}
