package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func remoteConsumerIDs() (string, string) { return uuid.NewString(), uuid.NewString() }

func insertRemoteEvent(t *testing.T, s *Store, id, payload string) int64 {
	t.Helper()
	result, err := s.db.ExecContext(context.Background(), `INSERT INTO router_events(uid,id,version,payload,source,emitted_at)
		VALUES(?,?,1,?,'fixture',?)`, uuid.NewString(), id, payload, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	seq, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func remoteConsumerCursor(t *testing.T, s *Store, consumerID string) int64 {
	t.Helper()
	var cursor int64
	if err := s.db.QueryRowContext(context.Background(), "SELECT cursor FROM remote_consumers WHERE consumer_id=?", consumerID).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	return cursor
}

func TestRemoteConsumerOpenReplaysAcrossReopenAndExplicitAfterWins(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "remote-consumers.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	insertRemoteEvent(t, s, "task.created", `{}`)
	secondSeq := insertRemoteEvent(t, s, "task.commented", `{}`)
	consumerID, firstConnection := remoteConsumerIDs()
	start := int64(0)
	if cursor, err := s.OpenRemoteConsumer(ctx, consumerID, firstConnection, &start); err != nil || cursor != 0 {
		t.Fatalf("explicit replay cursor=%d err=%v", cursor, err)
	}
	got, err := s.NextRemoteEvent(ctx, consumerID, firstConnection, []string{"task.commented"}, nil, "", nil)
	if err != nil || got == nil || got.Seq != secondSeq {
		t.Fatalf("filtered replay event=%+v err=%v", got, err)
	}
	if cursor := remoteConsumerCursor(t, s, consumerID); cursor != secondSeq-1 {
		t.Fatalf("matching event advanced before ack: cursor=%d want=%d", cursor, secondSeq-1)
	}
	if err := s.AckRemoteEvent(ctx, consumerID, firstConnection, secondSeq); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseRemoteConsumer(ctx, consumerID, firstConnection); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, secondConnection := remoteConsumerIDs()
	if cursor, err := s.OpenRemoteConsumer(ctx, consumerID, secondConnection, nil); err != nil || cursor != secondSeq {
		t.Fatalf("reopened consumer cursor=%d err=%v, want %d", cursor, err, secondSeq)
	}
	// An explicit rewind wins even when an unexpired cursor is available.
	if cursor, err := s.OpenRemoteConsumer(ctx, consumerID, firstConnection, &start); err != nil || cursor != start {
		t.Fatalf("explicit cursor did not override saved cursor: %d %v", cursor, err)
	}
}

func TestRemoteConsumerUnackedOfflineReplayRetainsHistoryUntilSharedCursorAdvances(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "remote-unacked-replay.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seq := insertRemoteEvent(t, s, "task.changed", `{"value":9007199254740993}`)
	oldAt := time.Now().UTC().Add(-2 * 24 * time.Hour).UnixMilli()
	if _, err := s.db.ExecContext(ctx, "UPDATE router_events SET emitted_at=? WHERE seq=?", oldAt, seq); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO router_deliveries(event_seq,plugin,state,next_attempt_at,updated_at)
		VALUES(?,'fixture','completed',0,0)`, seq); err != nil {
		t.Fatal(err)
	}

	consumerID, firstConnection := remoteConsumerIDs()
	start := int64(0)
	if _, err := s.OpenRemoteConsumer(ctx, consumerID, firstConnection, &start); err != nil {
		t.Fatal(err)
	}
	original, err := s.NextRemoteEvent(ctx, consumerID, firstConnection, []string{"task.changed"}, nil, "", nil)
	if err != nil || original == nil || original.Seq != seq {
		t.Fatalf("initial delivery=%+v err=%v", original, err)
	}
	if err := s.CloseRemoteConsumer(ctx, consumerID, firstConnection); err != nil {
		t.Fatal(err)
	}

	// A second consumer skips the event. Its cursor advances, but the shared
	// minimum stays at the first consumer's unacked cursor and protects history.
	secondID, secondConnection := remoteConsumerIDs()
	if _, err := s.OpenRemoteConsumer(ctx, secondID, secondConnection, &start); err != nil {
		t.Fatal(err)
	}
	if event, err := s.NextRemoteEvent(ctx, secondID, secondConnection, nil, nil, "", nil); err != nil || event != nil {
		t.Fatalf("nonmatching consumer event=%+v err=%v", event, err)
	}
	if cursor := remoteConsumerCursor(t, s, secondID); cursor != seq {
		t.Fatalf("nonmatching consumer cursor=%d want=%d", cursor, seq)
	}
	now := time.Now().UTC().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	through, err := remoteRetentionCursorIn(ctx, tx, now)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if through == nil || *through != 0 {
		tx.Rollback()
		t.Fatalf("shared retention cursor=%v, want first consumer's unacked cursor 0", through)
	}
	deleted, err := pruneRouterRetentionIn(ctx, tx, now-retentionDayMS, through, nil)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if deleted != 0 {
		tx.Rollback()
		t.Fatalf("unacked offline event pruned before reopen: deleted=%d", deleted)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := readRouterEventByUID(ctx, s.db, original.UID)
	if err != nil || persisted.Seq != original.Seq || persisted.UID != original.UID || string(persisted.Payload) != string(original.Payload) {
		t.Fatalf("event changed across database reopen: original=%+v persisted=%+v err=%v", original, persisted, err)
	}
	if cursor := remoteConsumerCursor(t, s, consumerID); cursor != 0 {
		t.Fatalf("first consumer cursor after database reopen=%d want 0", cursor)
	}
	if cursor := remoteConsumerCursor(t, s, secondID); cursor != seq {
		t.Fatalf("second consumer cursor after database reopen=%d want=%d", cursor, seq)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	through, err = remoteRetentionCursorIn(ctx, tx, now)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if through == nil || *through != 0 {
		tx.Rollback()
		t.Fatalf("shared cursor after database reopen=%v want 0", through)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	_, resumedConnection := remoteConsumerIDs()
	if cursor, err := s.OpenRemoteConsumer(ctx, consumerID, resumedConnection, nil); err != nil || cursor != 0 {
		t.Fatalf("offline consumer resumed at cursor=%d err=%v, want 0", cursor, err)
	}
	replayed, err := s.NextRemoteEvent(ctx, consumerID, resumedConnection, []string{"task.changed"}, nil, "", nil)
	if err != nil || replayed == nil || replayed.Seq != original.Seq || replayed.UID != original.UID || string(replayed.Payload) != string(original.Payload) {
		t.Fatalf("reopened unacked event changed: original=%+v replayed=%+v err=%v", original, replayed, err)
	}
	if err := s.AckRemoteEvent(ctx, consumerID, resumedConnection, seq); err != nil {
		t.Fatal(err)
	}

	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	through, err = remoteRetentionCursorIn(ctx, tx, now)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if through == nil || *through != seq {
		tx.Rollback()
		t.Fatalf("acked shared retention cursor=%v want=%d", through, seq)
	}
	deleted, err = pruneRouterRetentionIn(ctx, tx, now-retentionDayMS, through, nil)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if deleted != 1 {
		tx.Rollback()
		t.Fatalf("ack did not release old settled history: deleted=%d", deleted)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteConsumerFiltersTargetsAndPersistsOnlySkippedCursor(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	first := insertRemoteEvent(t, s, "ignored", `{}`)
	second := insertRemoteEvent(t, s, "wanted", `{"target":"elsewhere"}`)
	third := insertRemoteEvent(t, s, "wanted", `{"target":"receiver"}`)
	consumerID, connectionID := remoteConsumerIDs()
	start := int64(0)
	if _, err := s.OpenRemoteConsumer(ctx, consumerID, connectionID, &start); err != nil {
		t.Fatal(err)
	}
	event, err := s.NextRemoteEvent(ctx, consumerID, connectionID, []string{"wanted"}, []string{"receiver"}, "", nil)
	if err != nil || event == nil || event.Seq != third {
		t.Fatalf("next matching event=%+v err=%v", event, err)
	}
	if cursor := remoteConsumerCursor(t, s, consumerID); cursor != second {
		t.Fatalf("skip cursor=%d, want last skipped seq %d (first=%d)", cursor, second, first)
	}
	if err := s.AckRemoteEvent(ctx, consumerID, connectionID, third); err != nil {
		t.Fatal(err)
	}
	if cursor := remoteConsumerCursor(t, s, consumerID); cursor != third {
		t.Fatalf("acked cursor=%d want=%d", cursor, third)
	}
	if err := s.AckRemoteEvent(ctx, consumerID, connectionID, second); err != nil {
		t.Fatalf("lower duplicate ack should be monotonic and harmless: %v", err)
	}
	if cursor := remoteConsumerCursor(t, s, consumerID); cursor != third {
		t.Fatalf("lower ack regressed cursor to %d", cursor)
	}
}

func TestRemoteConsumerUntargetedNullAndWrongTypesAndEmptyTarget(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seqs := []int64{
		insertRemoteEvent(t, s, "wanted", `{"target":null}`),
		insertRemoteEvent(t, s, "wanted", `{"target":17}`),
		insertRemoteEvent(t, s, "wanted", `{}`),
		insertRemoteEvent(t, s, "wanted", `{"target":""}`),
	}
	consumerID, connectionID := remoteConsumerIDs()
	start := int64(0)
	if _, err := s.OpenRemoteConsumer(ctx, consumerID, connectionID, &start); err != nil {
		t.Fatal(err)
	}
	for _, expected := range seqs[:3] {
		event, err := s.NextRemoteEvent(ctx, consumerID, connectionID, []string{"wanted"}, nil, "", nil)
		if err != nil || event == nil || event.Seq != expected {
			t.Fatalf("untargeted seq %d returned event=%+v err=%v", expected, event, err)
		}
		if err := s.AckRemoteEvent(ctx, consumerID, connectionID, expected); err != nil {
			t.Fatal(err)
		}
	}
	if event, err := s.NextRemoteEvent(ctx, consumerID, connectionID, []string{"wanted"}, nil, "", nil); err != nil || event != nil {
		t.Fatalf("empty-string target matched empty target set: event=%+v err=%v", event, err)
	}
	if cursor := remoteConsumerCursor(t, s, consumerID); cursor != seqs[3] {
		t.Fatalf("empty target was not durably skipped: cursor=%d want=%d", cursor, seqs[3])
	}
	otherConsumer, otherConnection := remoteConsumerIDs()
	resumeAt := seqs[2]
	if _, err := s.OpenRemoteConsumer(ctx, otherConsumer, otherConnection, &resumeAt); err != nil {
		t.Fatal(err)
	}
	event, err := s.NextRemoteEvent(ctx, otherConsumer, otherConnection, []string{"wanted"}, []string{""}, "", nil)
	if err != nil || event == nil || event.Seq != seqs[3] {
		t.Fatalf("explicit empty target filter event=%+v err=%v", event, err)
	}
}

func TestRemoteConsumerTakeoverRejectsStaleAckAndTouch(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seq := insertRemoteEvent(t, s, "wanted", `{}`)
	consumerID, oldConnection := remoteConsumerIDs()
	if _, err := s.OpenRemoteConsumer(ctx, consumerID, oldConnection, nil); err != nil {
		t.Fatal(err)
	}
	_, currentConnection := remoteConsumerIDs()
	if _, err := s.OpenRemoteConsumer(ctx, consumerID, currentConnection, nil); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"next": func() error {
			_, err := s.NextRemoteEvent(ctx, consumerID, oldConnection, []string{"wanted"}, nil, "", nil)
			return err
		}(),
		"ack":   s.AckRemoteEvent(ctx, consumerID, oldConnection, seq),
		"check": s.CheckRemoteConsumer(ctx, consumerID, oldConnection),
		"touch": s.TouchRemoteConsumer(ctx, consumerID, oldConnection),
	} {
		if !errors.Is(err, ErrRemoteConsumerReplaced) {
			t.Errorf("stale %s error=%v, want ErrRemoteConsumerReplaced", name, err)
		}
	}
	if err := s.CloseRemoteConsumer(ctx, consumerID, oldConnection); err != nil {
		t.Fatalf("stale close should be harmless: %v", err)
	}
	var owner string
	if err := s.db.QueryRowContext(ctx, "SELECT connection_id FROM remote_consumers WHERE consumer_id=?", consumerID).Scan(&owner); err != nil || owner != currentConnection {
		t.Fatalf("stale close changed owner to %q: %v", owner, err)
	}
	if err := s.TouchRemoteConsumer(ctx, consumerID, currentConnection); err != nil {
		t.Fatalf("current owner touch: %v", err)
	}
}

func TestRemoteConsumerLeaseExpiryStartsAtTailAndRetentionDeletesRow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	tail := insertRemoteEvent(t, s, "wanted", `{}`)
	consumerID, connectionID := remoteConsumerIDs()
	start := int64(0)
	if _, err := s.OpenRemoteConsumer(ctx, consumerID, connectionID, &start); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().UnixMilli()
	if _, err := s.db.ExecContext(ctx, "UPDATE remote_consumers SET last_seen_at=? WHERE consumer_id=?", now-retentionReplayMS, consumerID); err != nil {
		t.Fatal(err)
	}
	if cursor, err := s.OpenRemoteConsumer(ctx, consumerID, connectionID, nil); err != nil || cursor != tail {
		t.Fatalf("expired cursor resumed at %d err=%v, want tail %d", cursor, err, tail)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE remote_consumers SET last_seen_at=? WHERE consumer_id=?", now-retentionReplayMS, consumerID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := remoteRetentionCursorIn(ctx, tx, now)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if cursor != nil {
		tx.Rollback()
		t.Fatalf("expired consumer retained cursor %d", *cursor)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchRemoteConsumer(ctx, consumerID, connectionID); !errors.Is(err, ErrRemoteConsumerReplaced) {
		t.Fatalf("touch after retention cleanup error=%v", err)
	}
}

func TestRemoteConsumerOpenValidatesUUIDsAndAllowsNegativeReplayCursor(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	consumerID, connectionID := remoteConsumerIDs()
	negative := int64(-1)
	if cursor, err := s.OpenRemoteConsumer(ctx, consumerID, connectionID, &negative); err != nil || cursor != negative {
		t.Fatalf("negative replay cursor=%d err=%v", cursor, err)
	}
	if _, err := s.OpenRemoteConsumer(ctx, "not-a-uuid", connectionID, nil); err == nil {
		t.Fatal("accepted noncanonical consumer UUID")
	}
	if err := s.TouchRemoteConsumer(ctx, consumerID, "not-a-uuid"); err == nil {
		t.Fatal("accepted noncanonical connection UUID")
	}
}

func TestRemoteConsumerIdleNextDoesNotRenewLease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	consumerID, connectionID := remoteConsumerIDs()
	if _, err := s.OpenRemoteConsumer(ctx, consumerID, connectionID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE remote_consumers SET last_seen_at=1 WHERE consumer_id=?", consumerID); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckRemoteConsumer(ctx, consumerID, connectionID); err != nil {
		t.Fatalf("read-only ownership check: %v", err)
	}
	if event, err := s.NextRemoteEvent(ctx, consumerID, connectionID, []string{"wanted"}, nil, "", nil); err != nil || event != nil {
		t.Fatalf("empty poll event=%+v err=%v", event, err)
	}
	var lastSeen int64
	if err := s.db.QueryRowContext(ctx, "SELECT last_seen_at FROM remote_consumers WHERE consumer_id=?", consumerID).Scan(&lastSeen); err != nil || lastSeen != 1 {
		t.Fatalf("empty poll renewed lease at %d: %v", lastSeen, err)
	}
}
