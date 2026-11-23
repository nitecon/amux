package storage

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/router"
)

const retentionTestDay int64 = 86_400_000

func retentionEvent(id string, at int64) router.Event {
	return router.Event{ID: id, Version: 1, UID: uuid.NewString(), EmittedAt: at}
}

func retentionInsertEvent(t *testing.T, s *Store, seq int64, at int64, state string) string {
	t.Helper()
	uid := uuid.NewString()
	if _, err := s.db.Exec(`INSERT INTO router_events(seq,uid,id,version,payload,source,emitted_at) VALUES(?,?,'fixture.event',1,'{}','test',?)`, seq, uid, at); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		if _, err := s.db.Exec(`INSERT INTO router_deliveries(event_seq,plugin,state,next_attempt_at,updated_at) VALUES(?,'test',?,0,0)`, seq, state); err != nil {
			t.Fatal(err)
		}
	}
	return uid
}

func TestRouterRetentionStartupTickGuardAndOverflow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','test','',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO messages(project_ident,source,content,sent_at,confirmed_at) VALUES('p','user','old',0,1)`); err != nil {
		t.Fatal(err)
	}
	started, count, err := s.ReconcileRouterRetention(ctx, retentionEvent("retention.startup", 10*retentionTestDay), 2, 2)
	if err != nil || !started || count != 1 {
		t.Fatalf("startup ran=%v deleted=%d err=%v", started, count, err)
	}
	for _, tc := range []struct {
		id string
		at int64
	}{
		{"retention.startup", 10 * retentionTestDay},
		{"clock.tick", 10*retentionTestDay + retentionTestDay - 1},
		{"clock.tick", 9 * retentionTestDay},
	} {
		ran, _, err := s.ReconcileRouterRetention(ctx, retentionEvent(tc.id, tc.at), 2, 2)
		if err != nil || ran {
			t.Fatalf("guarded event %s at %d: ran=%v err=%v", tc.id, tc.at, ran, err)
		}
	}
	ran, _, err := s.ReconcileRouterRetention(ctx, retentionEvent("clock.tick", 11*retentionTestDay), 2, 2)
	if err != nil || !ran {
		t.Fatalf("daily tick ran=%v err=%v", ran, err)
	}
	ran, _, err = s.ReconcileRouterRetention(ctx, retentionEvent("retention.startup", 11*retentionTestDay+1), 2, 2)
	if err != nil || !ran {
		t.Fatalf("fresh startup ran=%v err=%v", ran, err)
	}
	if _, _, err := s.ReconcileRouterRetention(ctx, retentionEvent("clock.tick", 12*retentionTestDay), ^uint64(0), ^uint64(0)); err != nil {
		t.Fatal(err)
	}
	if got := retentionCutoff(0, ^uint64(0)); got != -((1 << 63) - 1) {
		t.Fatalf("overflow cutoff=%d want saturated subtraction", got)
	}
}

func TestRouterRetentionUsesDistinctPoliciesAcrossStartupAndDailyTicks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := 10 * retentionTestDay
	if _, err := s.db.Exec(`INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','fixture','',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO messages(project_ident,source,content,sent_at,author_kind) VALUES
		('p','agent','old agent',?,NULL),('p','user','bot noise',?,'bot'),
		('p','agent','recent agent',?,NULL),('p','user','unanswered human',?,NULL)`,
		now-3*retentionTestDay, now-2*retentionTestDay, now-retentionTestDay, now-3*retentionTestDay); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		id      string
		at      int64
		ran     bool
		deleted int64
	}{
		{"retention.startup", now, true, 2},
		{"retention.startup", now, false, 0},
		{"clock.tick", now + retentionTestDay - 1, false, 0},
		{"clock.tick", now + retentionTestDay, true, 0},
		{"retention.startup", now + retentionTestDay + 1, true, 1},
	} {
		ran, deleted, err := s.ReconcileRouterRetention(ctx, retentionEvent(step.id, step.at), 2, 1)
		if err != nil || ran != step.ran || deleted != step.deleted {
			t.Fatalf("cleanup %s at %d: ran=%v deleted=%d error=%v", step.id, step.at, ran, deleted, err)
		}
	}
	var content string
	if err := s.db.QueryRow(`SELECT content FROM messages`).Scan(&content); err != nil || content != "unanswered human" {
		t.Fatalf("unanswered input after repeated cleanup: %q %v", content, err)
	}
}

func TestRouterRetentionPruneCutoffStatusesAndCursorBoundary(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := 20 * retentionTestDay
	cutoff := now - retentionTestDay
	retentionInsertEvent(t, s, 1, cutoff-1, "completed")
	retentionInsertEvent(t, s, 2, cutoff, "stopped")
	retentionInsertEvent(t, s, 3, cutoff-1, "skipped")
	retentionInsertEvent(t, s, 4, cutoff-1, "failed")
	retentionInsertEvent(t, s, 5, cutoff-1, "pending")
	retentionInsertEvent(t, s, 6, cutoff-1, "running")
	retentionInsertEvent(t, s, 7, cutoff-1, "retrying")
	retentionInsertEvent(t, s, 8, cutoff-1, "")
	retentionInsertEvent(t, s, 9, cutoff-1, "")
	retentionInsertEvent(t, s, 10, cutoff-1, "completed")
	if _, err := s.db.Exec(`ALTER TABLE router_deliveries RENAME TO router_deliveries_checked;
		CREATE TABLE router_deliveries(event_seq INTEGER NOT NULL,plugin TEXT NOT NULL,state TEXT NOT NULL,PRIMARY KEY(event_seq,plugin));
		INSERT INTO router_deliveries SELECT event_seq,plugin,state FROM router_deliveries_checked;
		DROP TABLE router_deliveries_checked;
		INSERT INTO router_deliveries(event_seq,plugin,state) VALUES(8,'fixture','future-state');`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO remote_consumers(consumer_id,cursor,last_seen_at) VALUES('cursor',8,?)`, now); err != nil {
		t.Fatal(err)
	}
	started, _, err := s.ReconcileRouterRetention(ctx, retentionEvent("retention.startup", now), 100, 100)
	if err != nil || !started {
		t.Fatalf("reconcile ran=%v err=%v", started, err)
	}
	var remaining int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM router_events`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 7 {
		rows, _ := s.db.Query(`SELECT seq FROM router_events ORDER BY seq`)
		var got []int64
		for rows.Next() {
			var seq int64
			_ = rows.Scan(&seq)
			got = append(got, seq)
		}
		_ = rows.Close()
		t.Fatalf("remaining=%d seqs=%v want 7 (inclusive cursor prunes seq<=8; cursor protects seq>8)", remaining, got)
	}
	if _, err := s.db.Exec(`UPDATE remote_consumers SET cursor=10`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE retention_runs SET completed_at=?`, now+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReconcileRouterRetention(ctx, retentionEvent("retention.startup", now+2), 100, 100); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM router_events WHERE seq=10`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("cursor boundary sequence remains=%d err=%v", remaining, err)
	}
}

func TestRouterRetentionExpiresLeasesAndProtectsNativeCauses(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := 30 * retentionTestDay
	retentionInsertEvent(t, s, 1, now-2*retentionTestDay, "completed")
	presenceUID := retentionInsertEvent(t, s, 2, now-2*retentionTestDay, "completed")
	heartbeatUID := retentionInsertEvent(t, s, 3, now-2*retentionTestDay, "completed")
	retentionInsertEvent(t, s, 4, now-2*retentionTestDay, "completed")
	retentionInsertEvent(t, s, 5, now-2*retentionTestDay, "completed")
	if _, err := s.db.Exec(`INSERT INTO remote_consumers VALUES('orphan','stale-connection',0,?)`, now-retentionTestDay); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO remote_consumers VALUES('recent-offline',NULL,3,?)`, now-1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO amux_presence_outbox(kind,payload,created_at,correlation) VALUES('cause','{}',?,?)`, now, presenceUID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO amux_heartbeat_outbox(uid,source,payload,created_at) VALUES(?,'test','{}',?)`, heartbeatUID, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReconcileRouterRetention(ctx, retentionEvent("retention.startup", now), 100, 100); err != nil {
		t.Fatal(err)
	}
	var consumers, found int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM remote_consumers`).Scan(&consumers); err != nil || consumers != 1 {
		t.Fatalf("consumer count=%d err=%v", consumers, err)
	}
	for _, uid := range []string{presenceUID, heartbeatUID} {
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM router_events WHERE uid=?`, uid).Scan(&found); err != nil || found != 1 {
			t.Fatalf("protected event %s count=%d err=%v", uid, found, err)
		}
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM router_events WHERE seq=1`).Scan(&found); err != nil || found != 0 {
		t.Fatalf("expired lease or acknowledged event count=%d err=%v", found, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM router_events WHERE seq IN (4,5)`).Scan(&found); err != nil || found != 2 {
		t.Fatalf("recent offline replay events count=%d err=%v", found, err)
	}
	if _, _, err := s.ReconcileRouterRetention(ctx, retentionEvent("clock.tick", now+retentionTestDay), 100, 100); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM remote_consumers`).Scan(&consumers); err != nil || consumers != 0 {
		t.Fatalf("expired offline consumers=%d err=%v", consumers, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM router_events WHERE seq IN (4,5)`).Scan(&found); err != nil || found != 0 {
		t.Fatalf("expired cursor still blocked settled cleanup: count=%d err=%v", found, err)
	}
}

func TestRouterRetentionRollbackAndRetry(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := 40 * retentionTestDay
	retentionInsertEvent(t, s, 1, now-2*retentionTestDay, "completed")
	if _, err := s.db.Exec(`INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','test','',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO messages(project_ident,source,content,sent_at,confirmed_at) VALUES('p','user','purge',0,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO remote_consumers VALUES('expired',NULL,0,?)`, now-2*retentionTestDay); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER retention_checkpoint_failure BEFORE INSERT ON retention_runs BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReconcileRouterRetention(ctx, retentionEvent("retention.startup", now), 1, 1); err == nil {
		t.Fatal("expected checkpoint failure")
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM router_events`:    1,
		`SELECT COUNT(*) FROM messages`:         1,
		`SELECT COUNT(*) FROM remote_consumers`: 1,
		`SELECT COUNT(*) FROM retention_runs`:   0,
	} {
		var got int
		if err := s.db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d, err %v; want %d", query, got, err, want)
		}
	}
	if _, err := s.db.Exec(`DROP TRIGGER retention_checkpoint_failure`); err != nil {
		t.Fatal(err)
	}
	ran, deleted, err := s.ReconcileRouterRetention(ctx, retentionEvent("retention.startup", now), 1, 1)
	if err != nil || !ran || deleted != 1 {
		t.Fatalf("retry ran=%v deleted=%d err=%v", ran, deleted, err)
	}
}

func TestRouterRetentionPrunesBacklogInBoundedBatches(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for seq := int64(1); seq <= retentionPruneBatch+44; seq++ {
		retentionInsertEvent(t, s, seq, 0, "completed")
	}
	tx := mustBeginTx(t, s)
	deleted, err := pruneRouterRetentionIn(ctx, tx, retentionTestDay, nil, nil)
	if err != nil || deleted != retentionPruneBatch+44 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM router_events`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("remaining=%d err=%v", remaining, err)
	}
}

func TestRouterRetentionIgnoredDeleteRollsBackAndCanRetry(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := 50 * retentionTestDay
	for seq := int64(1); seq <= retentionPruneBatch; seq++ {
		retentionInsertEvent(t, s, seq, now-2*retentionTestDay, "completed")
	}
	if _, err := s.db.Exec(`INSERT INTO remote_consumers VALUES('expired',NULL,0,?)`, now-2*retentionTestDay); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER ignore_router_retention_delete BEFORE DELETE ON router_events
		BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	event := retentionEvent("retention.startup", now)
	if _, _, err := s.ReconcileRouterRetention(ctx, event, 1, 1); err == nil {
		t.Fatal("expected ignored candidate delete to fail retention")
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM router_events`:    retentionPruneBatch,
		`SELECT COUNT(*) FROM remote_consumers`: 1,
		`SELECT COUNT(*) FROM retention_runs`:   0,
	} {
		var got int
		if err := s.db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d, err %v; want %d", query, got, err, want)
		}
	}
	if _, err := s.db.Exec(`DROP TRIGGER ignore_router_retention_delete`); err != nil {
		t.Fatal(err)
	}
	ran, _, err := s.ReconcileRouterRetention(ctx, event, 1, 1)
	if err != nil || !ran {
		t.Fatalf("retry ran=%v err=%v", ran, err)
	}
	var remaining int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM router_events`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("retry left %d router events, err=%v", remaining, err)
	}
}

func mustBeginTx(t *testing.T, s *Store) *sql.Tx {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

func TestRouterRetentionSchemaPreservesExistingRemoteConsumers(t *testing.T) {
	s := testStore(t)
	if _, err := s.db.Exec(`INSERT INTO remote_consumers VALUES('kept','connection',7,8)`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateRouterRetention(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var connection string
	var cursor int64
	if err := s.db.QueryRow(`SELECT connection_id,cursor FROM remote_consumers WHERE consumer_id='kept'`).Scan(&connection, &cursor); err != nil || connection != "connection" || cursor != 7 {
		t.Fatalf("existing consumer (%q,%d), err=%v", connection, cursor, err)
	}
	var index int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_remote_consumers_cursor'`).Scan(&index); err != nil || index != 1 {
		t.Fatalf("cursor index count=%d err=%v", index, err)
	}
}
