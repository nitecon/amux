package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
)

func emitObservationFixtureEvent(t *testing.T, s *Store, registry *router.Registry, id, source, payload string, at int64) router.Event {
	t.Helper()
	result, err := s.EmitRouterEvent(context.Background(), registry, router.EmitRequest{ID: id, Version: 1, Source: source, Payload: json.RawMessage(payload)})
	if err != nil || !result.Stored {
		t.Fatalf("persist %s: %+v %v", id, result, err)
	}
	// These are durable replay events from an earlier server time. The runtime
	// still reads the authoritative journal record rather than a forged callback.
	if _, err := s.db.Exec("UPDATE router_events SET emitted_at=? WHERE seq=?", at, result.Event.Seq); err != nil {
		t.Fatal(err)
	}
	result.Event.EmittedAt = at
	return result.Event
}

func TestPresenceObservationRuntimePreservesCausalityAndIndependentLeases(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "observations.db"))
	if _, err := s.db.Exec("UPDATE projects SET kind='adhoc' WHERE ident='presence'"); err != nil {
		t.Fatal(err)
	}
	origin := AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	base := time.Now().Add(-91 * time.Second).UnixMilli()
	if _, err := s.RegisterTarget(ctx, "presence", TargetRegistration{Origin: origin, Generation: uuid.NewString(), Hostname: ptr("retained-host")}, time.UnixMilli(base)); err != nil {
		t.Fatal(err)
	}
	task, err := s.InsertTask(ctx, TaskInsert{Project: "presence", Title: "Must stay queued", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE tasks SET created_at=?,updated_at=? WHERE id=?", base-1, base-1, task.ID); err != nil {
		t.Fatal(err)
	}
	runtime, err := router.NewRuntime(s, s.EventBus(), nativePresenceDefinitions(), []router.Plugin{plugins.PresencePlugin(s), nativePresenceObserver()}, router.RunConfig{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	heartbeatPayload := `{"os":"linux","hostname":"retained-host","agents":[{"project":"presence","session_id":"` + origin.SessionID + `","instance_id":"` + origin.InstanceID + `","provider":"codex","os":"linux","canonical_remote":"  ","auto_assign":true,"source":"forged","rank":9007199254740993}]}`
	heartbeat := emitObservationFixtureEvent(t, s, runtime.Registry(), plugins.ClientHeartbeatID, "legacy:runtime", heartbeatPayload, base)
	activity := emitObservationFixtureEvent(t, s, runtime.Registry(), plugins.ClientActivityID, "legacy:runtime", `{"session_id":"`+origin.SessionID+`","instance_id":"`+origin.InstanceID+`","output_at":`+strconv.FormatInt(base-31_000, 10)+`,"draft":false}`, base+1)
	stop := runRouterFixture(t, runtime)
	defer stop()
	waitRouterDeliveryState(t, s, heartbeat.Seq, "presence", "completed")
	waitRouterDeliveryState(t, s, activity.Seq, "presence", "completed")
	idle := awaitSourceEvent(t, s, "agent.idle")
	if idle.Source != "plugin:presence" || idle.Correlation == nil || *idle.Correlation != activity.UID {
		t.Fatalf("activity lost its cause: %+v", idle)
	}
	if !strings.Contains(string(idle.Payload), "9007199254740993") || !strings.Contains(string(idle.Payload), `"source":"legacy:runtime"`) {
		t.Fatalf("observation snapshot lost fields or source authority: %s", idle.Payload)
	}
	live, err := s.LivePresence(ctx, time.UnixMilli(base+31_001))
	if err != nil || len(live) != 1 || live[0].State != "idle" || live[0].AutoAssign || live[0].ReviewContextClear {
		t.Fatalf("raw observation gained native execution capabilities: %+v %v", live, err)
	}
	if err := s.ReconcileAssignments(ctx, time.UnixMilli(base+31_001)); err != nil {
		t.Fatal(err)
	}
	detail, err := s.Task(ctx, "presence", task.ID)
	if err != nil || detail.Task.Status != "todo" || detail.Task.OwnerOrigin != nil {
		t.Fatalf("raw observation claimed queued work: %+v %v", detail, err)
	}
	tick := emitObservationFixtureEvent(t, s, runtime.Registry(), "clock.tick", "clock", `{}`, base+91_000)
	waitRouterDeliveryState(t, s, tick.Seq, "presence", "completed")
	gone := awaitSourceEvent(t, s, "agent.gone")
	if gone.Correlation == nil || *gone.Correlation != tick.UID {
		t.Fatalf("expiry lost its clock cause: %+v", gone)
	}
	if targets, err := s.Targets(ctx, "presence", time.UnixMilli(base+91_000)); err != nil || len(targets) != 1 {
		t.Fatalf("event expiry erased independent routing lease: %+v %v", targets, err)
	}
	if err := s.ReconcileRouterPresence(ctx, runtime.Registry(), heartbeat); err != nil {
		t.Fatal(err)
	}
	if live, err := s.LivePresence(ctx, time.UnixMilli(base+91_000)); err != nil || len(live) != 0 {
		t.Fatalf("replay resurrected expired observation: %+v %v", live, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM pragma_foreign_key_check", int64(0))
}

func TestPresenceBindingFilterKeepsEmptySnapshotClock(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "filtered-clock.db"))
	base := time.Now().UnixMilli()
	if _, err := s.db.Exec("UPDATE projects SET canonical_remote='github.com/example/new' WHERE ident='presence'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE amux_presence_clock SET observed_at=? WHERE id=1", base+91_000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO rawpresence_sources(source,heartbeat_seq,heartbeat_uid,heartbeat_seen_at,os,hostname)
		VALUES('raw:clock',1,?,?,'linux','Expired-Machine')`, uuid.NewString(), base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO amux_presence_agents(project_ident,instance_id,session_id,provider,os,source,sequence,seen_at,
		last_activity_at,busy,draft,auto_assign,review_context_clear,state,report_kind,event_agent_json,binding_required,binding_remote)
		VALUES('presence','instance','session','codex','linux','raw:clock',1,?, ?,0,0,0,0,'idle','event',
		'{"project":"presence","instance_id":"instance","session_id":"session","provider":"codex","os":"linux"}',1,'github.com/example/old')`,
		base+90_000, base+50_000); err != nil {
		t.Fatal(err)
	}
	live, observed, err := livePresenceSnapshot(ctx, s.db, base+1)
	if err != nil || len(live) != 0 || observed != base+91_000 {
		t.Fatalf("all-hidden snapshot lost clock: live=%+v observed=%d error=%v", live, observed, err)
	}
	summary, err := s.PresenceSummary(ctx, time.UnixMilli(base+1))
	if err != nil || summary.Machines != 0 || summary.Agents != 0 {
		t.Fatalf("binding filter resurrected expired inventory: %+v %v", summary, err)
	}
}

func TestPresenceObservationFanoutFailureRollsBackCursorClockAndRows(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "observation-rollback.db"))
	registry, err := router.NewRegistry(nativePresenceDefinitions(), []router.PluginDefinition{plugins.PresencePlugin(nil).Definition, nativePresenceObserver().Definition})
	if err != nil {
		t.Fatal(err)
	}
	root := emitObservationFixtureEvent(t, s, registry, plugins.ClientHeartbeatID, "raw:atomic", `{"agents":[
		{"project":"unregistered","instance_id":"one","session_id":"first","provider":"legacy","os":"linux"},
		{"project":"","instance_id":"two","session_id":"second","provider":"legacy","os":"windows"}]}`, time.Now().UnixMilli())
	if _, err := s.db.Exec(`CREATE TRIGGER fail_second_presence_event BEFORE INSERT ON router_events
		WHEN NEW.id='agent.online' AND json_extract(NEW.payload,'$.agent.session_id')='second'
		BEGIN SELECT RAISE(ABORT,'fixture second observation failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterPresence(ctx, registry, root); err == nil {
		t.Fatal("partial fanout failure was swallowed")
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_agents", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM rawpresence_sources", int64(0))
	assertEqual(t, s.db, "SELECT observed_at FROM amux_presence_clock WHERE id=1", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_outbox", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events", int64(1))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_deliveries WHERE plugin='observer'", int64(0))
	if _, err := s.db.Exec("DROP TRIGGER fail_second_presence_event"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterPresence(ctx, registry, root); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterPresence(ctx, registry, root); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_agents WHERE report_kind='event' AND auto_assign=0", int64(2))
	assertEqual(t, s.db, "SELECT heartbeat_seq FROM rawpresence_sources WHERE source='raw:atomic'", root.Seq)
	assertEqual(t, s.db, "SELECT observed_at FROM amux_presence_clock WHERE id=1", root.EmittedAt)
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events WHERE id='agent.online'", int64(2))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM projects WHERE ident='unregistered' OR ident=''", int64(0))
}

func TestPresenceObservationFullRustProfileKeepsArchivesAndSeparateCursors(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rust-observations.db")
	schema, err := os.ReadFile("testdata/task_status_legacy.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, string(schema)); err != nil {
		t.Fatal(err)
	}
	seedRustTaskProfile(t, ctx, db)
	original := snapshotRustSchema(t, db)
	if _, err := db.ExecContext(ctx, `INSERT INTO presence_clock(id,observed_at) VALUES(1,1000);
		INSERT INTO presence_sources(source,heartbeat_seq) VALUES('rust-source',77);
		INSERT INTO router_events(seq,uid,id,version,payload,source,emitted_at)
		VALUES(77,'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','client.heartbeat',1,
		'{"os":"windows","hostname":"Archive-Host","agents":[]}','rust-source',1000);
		INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,seen_at,hostname,
		advertised,source,agent_json,report_seen_at,output_at,state,identity_seq,activity_seq)
		VALUES('rust-project','legacy-instance','legacy-session','{"provider":"codex"}','legacy-generation',1000,'Manual-Host',1,
		'rust-source','{"project":"rust-project","instance_id":"legacy-instance","session_id":"legacy-session","provider":"codex","os":"windows","opaque":9007199254740993}',1000,900,'idle',77,78);`); err != nil {
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
	assertEqual(t, s.db, "SELECT source FROM amux_presence_agents WHERE session_id='legacy-session'", "rust-source")
	assertEqual(t, s.db, "SELECT report_kind FROM amux_presence_agents WHERE session_id='legacy-session'", "event")
	assertEqual(t, s.db, "SELECT activity_seq FROM amux_presence_agents WHERE session_id='legacy-session'", int64(78))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_sources WHERE source='rust-source'", int64(0))
	assertEqual(t, s.db, "SELECT heartbeat_seq FROM rawpresence_sources WHERE source='rust-source'", int64(77))
	// The native local sequence starts at one even when the imported global
	// router sequence for the same source is much larger.
	origin := AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "rust-source", Sequence: 1, Agents: []PresenceAgent{{ProjectIdent: "rust-project", Origin: origin}}}, time.Now()); err != nil || !accepted {
		t.Fatalf("raw global cursor blocked native local sequence: %v %v", accepted, err)
	}
	assertEqual(t, s.db, "SELECT sequence FROM amux_presence_sources WHERE source='rust-source'", int64(1))
	assertEqual(t, s.db, "SELECT heartbeat_seq FROM rawpresence_sources WHERE source='rust-source'", int64(77))
	assertEqual(t, s.db, "SELECT state FROM amux_presence_agents WHERE session_id='legacy-session'", "gone")
	assertEqual(t, s.db, "SELECT report_seen_at FROM task_target_presence WHERE session_id='legacy-session'", int64(1000))
	assertEqual(t, s.db, "SELECT output_at FROM task_target_presence WHERE session_id='legacy-session'", int64(900))
	assertEqual(t, s.db, "SELECT activity_seq FROM task_target_presence WHERE session_id='legacy-session'", int64(78))
	assertEqual(t, s.db, "SELECT hostname FROM task_target_presence WHERE session_id='legacy-session'", "Manual-Host")
	assertEqual(t, s.db, "SELECT heartbeat_seq FROM presence_sources WHERE source='rust-source'", int64(77))
	assertEqual(t, s.db, "SELECT observed_at FROM presence_clock WHERE id=1", int64(1000))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM task_reviews WHERE source_task_id='rust-source'", int64(1))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM pragma_foreign_key_check", int64(0))
}

func TestPresenceObservationSummaryMatchesLegacyInventoryContract(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "legacy-summary.db"))
	registry, err := router.NewRegistry(nativePresenceDefinitions(), []router.PluginDefinition{plugins.PresencePlugin(nil).Definition, nativePresenceObserver().Definition})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UnixMilli()
	emit := func(id, source, payload string) {
		t.Helper()
		root := emitObservationFixtureEvent(t, s, registry, id, source, payload, base)
		if err := s.ReconcileRouterPresence(ctx, registry, root); err != nil {
			t.Fatal(err)
		}
	}
	linux := `{"project":"p","session_id":"linux-agent","instance_id":"instance","provider":"codex","os":"linux"}`
	unresolved := `{"project":"","session_id":"unresolved-agent","instance_id":"instance","provider":"codex","os":"linux"}`
	windows := `{"project":"p","session_id":"windows-agent","instance_id":"instance","provider":"claude","os":"windows"}`
	emit(plugins.ClientHeartbeatID, "client:linux", `{"os":"linux","agents":[`+linux+`,`+linux+`,`+unresolved+`]}`)
	emit(plugins.ClientHeartbeatID, "client:windows-one", `{"agents":[`+windows+`]}`)
	emit(plugins.ClientHeartbeatID, "client:windows-two", `{"os":"windows","agents":[]}`)
	emit(plugins.LegacyActivityID, "client:linux", `{"session_id":"linux-agent","instance_id":"instance","output_at":`+strconv.FormatInt(base, 10)+`,"draft":false}`)
	check := func(machines int, platforms []string) {
		t.Helper()
		summary, err := s.PresenceSummary(ctx, time.UnixMilli(base+10))
		if err != nil || summary.Machines != machines || summary.Agents != 3 || summary.Active != 1 || strings.Join(summary.OS, ",") != strings.Join(platforms, ",") {
			t.Fatalf("legacy inventory: %+v error=%v", summary, err)
		}
	}
	check(3, []string{"linux", "windows"})
	emit(plugins.ClientHeartbeatID, "client:windows-two", `{"os":"windows","agents":[]}`)
	check(3, []string{"linux", "windows"})
	emit(plugins.ClientHeartbeatID, "client:mac", `{"os":"macos","agents":[]}`)
	check(4, []string{"linux", "macos", "windows"})
	if _, err := s.db.Exec("UPDATE rawpresence_sources SET heartbeat_seen_at=? WHERE source='client:windows-two'", base-presenceGoneAfter); err != nil {
		t.Fatal(err)
	}
	check(3, []string{"linux", "macos", "windows"})
	assertEqual(t, s.db, "SELECT COUNT(*) FROM projects WHERE ident='p' OR ident=''", int64(0))
}

func TestPresenceObservationExtraHostIsNotRegisteredMetadata(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "opaque-host.db"))
	registry, err := router.NewRegistry(nativePresenceDefinitions(), []router.PluginDefinition{plugins.PresencePlugin(nil).Definition, nativePresenceObserver().Definition})
	if err != nil {
		t.Fatal(err)
	}
	root := emitObservationFixtureEvent(t, s, registry, plugins.ClientHeartbeatID, "raw:host", `{"agents":[
		{"project":"presence","instance_id":"instance","session_id":"session","provider":"codex","os":"linux",
		"name":"Agent label","hostname":"Unregistered-Extra"}]}`, time.Now().UnixMilli())
	if err := s.ReconcileRouterPresence(ctx, registry, root); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_presence_agents WHERE hostname IS NULL", int64(1))
	assertEqual(t, s.db, "SELECT json_extract(event_agent_json,'$.hostname') FROM amux_presence_agents", "Unregistered-Extra")
	host, err := registeredNotificationHost(ctx, s.db, "presence", json.RawMessage(`{"session_id":"session","instance_id":"instance"}`))
	if err != nil || host != nil {
		t.Fatalf("opaque event field became registered host provenance: %v %v", host, err)
	}
	summary, err := s.PresenceSummary(ctx, time.Now())
	if err != nil || summary.Machines != 1 || summary.Agents != 1 {
		t.Fatalf("opaque event metadata created a second machine: %+v %v", summary, err)
	}
}
