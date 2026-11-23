package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/router"
)

func rawPresenceEvent(id, source string, seq, at int64, payload string) router.Event {
	return router.Event{UID: uuid.NewString(), ID: id, Version: 1, Source: source, Seq: seq, EmittedAt: at, Payload: json.RawMessage(payload)}
}

func rawPresenceRegistry(t *testing.T) *router.Registry {
	t.Helper()
	registry, err := router.NewRegistry(RouterPresenceEventDefinitions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestReconcilePresenceEventPreservesRawAndFencesActivity(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UnixMilli()
	heartbeat := rawPresenceEvent("client.heartbeat", "legacy:one", 40, now,
		`{"hostname":"`+strings.Repeat("h", 180)+`","agents":[{"project":"Legacy/Case","instance_id":"i","session_id":"s","provider":"claude","os":"linux","name":"Friendly","goal":"keep this","generation":"legacy-generation","agent_id":"legacy-agent","opaque":{"large":900719925474099312345,"items":[1,2]}}]}`)
	if err := s.ReconcilePresenceEvent(ctx, rawPresenceRegistry(t), heartbeat); err != nil {
		t.Fatal(err)
	}
	var reportKind, rawAgent string
	var identitySeq, activitySeq int64
	var busy, draft, autoAssign, reviewClear bool
	if err := s.db.QueryRowContext(ctx, `SELECT report_kind,event_agent_json,identity_seq,activity_seq,busy,draft,auto_assign,review_context_clear
		FROM amux_presence_agents WHERE project_ident='Legacy/Case'`).Scan(&reportKind, &rawAgent, &identitySeq, &activitySeq, &busy, &draft, &autoAssign, &reviewClear); err != nil {
		t.Fatal(err)
	}
	if reportKind != "event" || identitySeq != 40 || activitySeq != 0 || busy || draft || autoAssign || reviewClear {
		t.Fatalf("stored event observation kind=%s identity=%d activity=%d busy=%t draft=%t auto=%t review=%t", reportKind, identitySeq, activitySeq, busy, draft, autoAssign, reviewClear)
	}
	if !strings.Contains(rawAgent, `"source":"legacy:one"`) || !strings.Contains(rawAgent, `900719925474099312345`) || !strings.Contains(rawAgent, `"opaque"`) {
		t.Fatalf("raw observation lost source or unknown JSON: %s", rawAgent)
	}
	var storedHostname sql.NullString
	var goal, generation, agentID string
	if err := s.db.QueryRowContext(ctx, `SELECT hostname,goal,generation,agent_id FROM amux_presence_agents
		WHERE project_ident='Legacy/Case'`).Scan(&storedHostname, &goal, &generation, &agentID); err != nil {
		t.Fatal(err)
	}
	if storedHostname.Valid || goal != "keep this" || generation != "legacy-generation" || agentID != "legacy-agent" {
		t.Fatalf("legacy typed routing metadata lost or inferred: hostname=%+v goal=%q generation=%q agent_id=%q", storedHostname, goal, generation, agentID)
	}
	var sourceHostname string
	if err := s.db.QueryRowContext(ctx, "SELECT hostname FROM rawpresence_sources WHERE source='legacy:one'").Scan(&sourceHostname); err != nil {
		t.Fatal(err)
	}
	if sourceHostname != strings.Repeat("h", 180) {
		t.Fatalf("raw source hostname was normalized or truncated: length=%d", len(sourceHostname))
	}
	activity := rawPresenceEvent("client.activity", "legacy:one", 41, now+1,
		`{"session_id":"s","instance_id":"i","output_at":`+strconv.FormatInt(time.UnixMilli(now-60_000).UnixMilli(), 10)+`,"draft":false}`)
	if err := s.ReconcilePresenceEvent(ctx, rawPresenceRegistry(t), activity); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT activity_seq,state FROM amux_presence_agents WHERE project_ident='Legacy/Case'").Scan(&activitySeq, &reportKind); err != nil {
		t.Fatal(err)
	}
	if activitySeq != 41 || reportKind != "idle" {
		t.Fatalf("activity state sequence=%d state=%q", activitySeq, reportKind)
	}
	old := rawPresenceEvent("client.activity", "legacy:one", 40, now+2,
		`{"session_id":"s","instance_id":"i","output_at":`+strconv.FormatInt(now, 10)+`,"draft":true}`)
	if err := s.ReconcilePresenceEvent(ctx, rawPresenceRegistry(t), old); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT activity_seq,state FROM amux_presence_agents WHERE project_ident='Legacy/Case'").Scan(&activitySeq, &reportKind); err != nil {
		t.Fatal(err)
	}
	if activitySeq != 41 || reportKind != "idle" {
		t.Fatalf("old activity changed state sequence=%d state=%q", activitySeq, reportKind)
	}
	omitted := rawPresenceEvent("client.heartbeat", "legacy:one", 42, now+3, `{"agents":[]}`)
	if err := s.ReconcilePresenceEvent(ctx, rawPresenceRegistry(t), omitted); err != nil {
		t.Fatal(err)
	}
	late := rawPresenceEvent("client.activity", "legacy:one", 43, now+4,
		`{"session_id":"s","instance_id":"i","output_at":`+strconv.FormatInt(now+4, 10)+`,"draft":true}`)
	if err := s.ReconcilePresenceEvent(ctx, rawPresenceRegistry(t), late); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT activity_seq,state FROM amux_presence_agents WHERE project_ident='Legacy/Case'").Scan(&activitySeq, &reportKind); err != nil {
		t.Fatal(err)
	}
	if activitySeq != 41 || reportKind != "gone" {
		t.Fatalf("late activity resurrected omitted presence: sequence=%d state=%q", activitySeq, reportKind)
	}
}

func TestRawSummaryUsesOneSourceMachineForMultipleAgentNames(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UnixMilli()
	event := rawPresenceEvent("client.heartbeat", "raw:names", 1, now,
		`{"agents":[{"project":"legacy-a","instance_id":"i","session_id":"a","provider":"legacy","os":"linux","name":"Alice"},{"project":"legacy-b","instance_id":"i","session_id":"b","provider":"legacy","os":"linux","name":"Bob"}]}`)
	if err := s.ReconcilePresenceEvent(ctx, rawPresenceRegistry(t), event); err != nil {
		t.Fatal(err)
	}
	summary, err := s.PresenceSummary(ctx, time.UnixMilli(now))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Machines != 1 {
		t.Fatalf("agent display names inflated source machine count: %+v", summary)
	}
	var hosted int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM amux_presence_agents WHERE source='raw:names' AND hostname IS NOT NULL`).Scan(&hosted); err != nil {
		t.Fatal(err)
	}
	if hosted != 0 {
		t.Fatalf("agent display name was persisted as hostname on %d rows", hosted)
	}
}

func TestRawHeartbeatAllowsEmptyLegacyIdentityAndSkipsBadBindingSibling(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	project := fixtureProject("Bound")
	project.CanonicalRemote = ptr("github.com/team/repo")
	if err := s.InsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	event := rawPresenceEvent("client.heartbeat", "legacy:bindings", 1, now,
		`{"agents":[{"project":"","instance_id":"","session_id":"","provider":"","os":""},{"project":"bound","instance_id":"good-instance","session_id":"good-session","provider":"legacy","os":"linux","canonical_remote":"https://github.com/team/repo.git"},{"project":"bound","instance_id":"bad-instance","session_id":"bad-session","provider":"legacy","os":"linux","canonical_remote":"github.com/elsewhere/repo"},{"project":" bound ","instance_id":"normalized-spoof","session_id":"spoof-session","provider":"legacy","os":"linux","canonical_remote":"github.com/team/repo"}]}`)
	if err := s.ReconcilePresenceEvent(ctx, rawPresenceRegistry(t), event); err != nil {
		t.Fatal(err)
	}
	var emptyIdentity, goodBinding, badBinding, normalizedAlias int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_presence_agents WHERE project_ident='' AND instance_id='' AND session_id=''").Scan(&emptyIdentity); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_presence_agents WHERE project_ident='bound' AND instance_id='good-instance'").Scan(&goodBinding); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_presence_agents WHERE project_ident='bound' AND instance_id='bad-instance'").Scan(&badBinding); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_presence_agents WHERE project_ident=' bound ' AND instance_id='normalized-spoof'").Scan(&normalizedAlias); err != nil {
		t.Fatal(err)
	}
	if emptyIdentity != 1 || goodBinding != 1 || badBinding != 0 || normalizedAlias != 1 {
		t.Fatalf("empty identity/binding handling: empty=%d good=%d bad=%d normalized-alias=%d", emptyIdentity, goodBinding, badBinding, normalizedAlias)
	}
}

func TestRawHeartbeatCursorAndNativePrecedence(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "raw-presence.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now().UnixMilli()
	registry := rawPresenceRegistry(t)
	first := rawPresenceEvent("client.heartbeat", "raw:source", 10, now, `{"agents":[]}`)
	if err := s.ReconcilePresenceEvent(ctx, registry, first); err != nil {
		t.Fatal(err)
	}
	if ok, err := routerHeartbeatCurrent(ctx, s.db, first, now); err != nil || !ok {
		t.Fatalf("accepted raw heartbeat current=%t err=%v", ok, err)
	}
	stale := rawPresenceEvent("client.heartbeat", "raw:source", 9, now+1, `{"agents":[]}`)
	if err := s.ReconcilePresenceEvent(ctx, registry, stale); err != nil {
		t.Fatal(err)
	}
	if ok, err := routerHeartbeatCurrent(ctx, s.db, stale, now+1); err != nil || ok {
		t.Fatalf("stale raw heartbeat current=%t err=%v", ok, err)
	}
	oldClock := rawPresenceEvent("client.heartbeat", "raw:old-clock", 1, now-presenceGoneAfter-1, `{"agents":[]}`)
	if err := s.ReconcilePresenceEvent(ctx, registry, oldClock); err != nil {
		t.Fatal(err)
	}
	var oldCursor int64
	var oldRows int
	if err := s.db.QueryRowContext(ctx, "SELECT heartbeat_seq FROM rawpresence_sources WHERE source='raw:old-clock'").Scan(&oldCursor); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_presence_agents WHERE source='raw:old-clock'").Scan(&oldRows); err != nil {
		t.Fatal(err)
	}
	if oldCursor != oldClock.Seq || oldRows != 0 {
		t.Fatalf("old-clock heartbeat cursor=%d rows=%d", oldCursor, oldRows)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO amux_presence_sources(source,sequence,heartbeat_uid,heartbeat_seen_at) VALUES('native:source',1,?,?)`, uuid.NewString(), now); err != nil {
		t.Fatal(err)
	}
	nativeNamedRaw := rawPresenceEvent("client.heartbeat", "native:source", 2, now+2, `{"agents":[]}`)
	if err := s.ReconcilePresenceEvent(ctx, registry, nativeNamedRaw); err != nil {
		t.Fatal(err)
	}
	var seq int64
	if err := s.db.QueryRowContext(ctx, "SELECT sequence FROM amux_presence_sources WHERE source='native:source'").Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatalf("raw callback mutated native source sequence to %d", seq)
	}
	var rawRows int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM rawpresence_sources WHERE source='native:source'").Scan(&rawRows); err != nil {
		t.Fatal(err)
	}
	if rawRows != 0 {
		t.Fatalf("native source was copied into raw cursor table: rows=%d", rawRows)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := storeEventPresenceIn(ctx, tx, "native:source", json.RawMessage(`{"project":"not-in-native-snapshot","instance_id":"i","session_id":"s","provider":"legacy","os":"linux"}`), now, 3)
	_ = tx.Rollback()
	if err != nil || imported.Accepted {
		t.Fatalf("migration helper accepted a native-owned source: result=%+v err=%v", imported, err)
	}
}
