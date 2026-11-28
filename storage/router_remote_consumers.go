package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
)

const (
	remoteConsumerScanBatch      = 100
	remoteConsumerMaxScanBatches = 100
)

// ErrRemoteConsumerReplaced means this connection no longer owns the consumer.
var ErrRemoteConsumerReplaced = errors.New("remote consumer connection was replaced")

// OpenRemoteConsumer claims a consumer for this connection and returns its
// replay cursor. Explicit cursors override saved cursors; expired or missing
// consumers without an explicit cursor start at the current event tail.
func (s *Store) OpenRemoteConsumer(ctx context.Context, consumerID, connectionID string, after *int64) (int64, error) {
	if !canonicalUUID(consumerID) || !canonicalUUID(connectionID) {
		return 0, fmt.Errorf("consumer and connection IDs must be canonical UUIDs")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().UnixMilli()
	var saved sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT cursor FROM remote_consumers
		WHERE consumer_id=? AND last_seen_at>?`, consumerID, retentionCutoff(now, 1)).Scan(&saved)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read remote consumer cursor: %w", err)
	}
	var tail int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM router_events").Scan(&tail); err != nil {
		return 0, fmt.Errorf("read router event tail: %w", err)
	}
	cursor := tail
	if saved.Valid {
		cursor = saved.Int64
	}
	if after != nil {
		cursor = *after
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO remote_consumers(consumer_id,connection_id,cursor,last_seen_at)
		VALUES(?,?,?,?) ON CONFLICT(consumer_id) DO UPDATE SET
		connection_id=excluded.connection_id,cursor=excluded.cursor,last_seen_at=excluded.last_seen_at`, consumerID, connectionID, cursor, now); err != nil {
		return 0, fmt.Errorf("claim remote consumer: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return cursor, nil
}

// NextRemoteEvent returns the next matching event after durably skipping
// nonmatching rows. A matching event never advances the cursor; the caller
// advances it only after validating and accepting its in-flight ack.
func (s *Store) NextRemoteEvent(ctx context.Context, consumerID, connectionID string, events, targets []string, principal string, enrollment *SecurityEnrollment) (*fism.Event, error) {
	if err := validateRemoteConsumerIDs(consumerID, connectionID); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cursor, err := ownedRemoteCursor(ctx, tx, consumerID, connectionID)
	if err != nil {
		return nil, err
	}
	eventSet := make(map[string]struct{}, len(events))
	for _, event := range events {
		eventSet[event] = struct{}{}
	}
	targetSet := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		targetSet[target] = struct{}{}
	}
	startCursor := cursor
	lastSkipped := startCursor
	for batch := 0; batch < remoteConsumerMaxScanBatches; batch++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, err := readRemoteEventSequenceBatch(ctx, tx, cursor)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			event, err := readRouterEventByUID(ctx, tx, row.uid)
			if err != nil {
				return nil, fmt.Errorf("read remote router event %d: %w", row.seq, err)
			}
			matches := remoteEventMatches(event, eventSet, targetSet)
			if matches && event.ID == controllers.NotifyEventID {
				matches, err = remoteNotificationAllowed(ctx, tx, event, principal, enrollment)
				if err != nil {
					return nil, err
				}
			}
			if matches {
				if lastSkipped != startCursor {
					if err := updateOwnedRemoteCursor(ctx, tx, consumerID, connectionID, lastSkipped, time.Now().UTC().UnixMilli()); err != nil {
						return nil, err
					}
				}
				if err := tx.Commit(); err != nil {
					return nil, err
				}
				return &event, nil
			}
			lastSkipped = row.seq
		}
		cursor = rows[len(rows)-1].seq
	}
	if lastSkipped != startCursor {
		if err := updateOwnedRemoteCursor(ctx, tx, consumerID, connectionID, lastSkipped, time.Now().UTC().UnixMilli()); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return nil, nil
}

// CheckRemoteConsumer validates the current connection owner without renewing
// its replay lease. It is suitable for outbound retry and heartbeat ticks.
func (s *Store) CheckRemoteConsumer(ctx context.Context, consumerID, connectionID string) error {
	if err := validateRemoteConsumerIDs(consumerID, connectionID); err != nil {
		return err
	}
	_, err := ownedRemoteCursor(ctx, s.db, consumerID, connectionID)
	return err
}

// AckRemoteEvent monotonically advances the owned cursor. The server validates
// that seq is the exact in-flight event before calling this method.
func (s *Store) AckRemoteEvent(ctx context.Context, consumerID, connectionID string, seq int64) error {
	if err := validateRemoteConsumerIDs(consumerID, connectionID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := updateOwnedRemoteCursor(ctx, tx, consumerID, connectionID, seq, time.Now().UTC().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// TouchRemoteConsumer renews the lease only for its current connection owner.
func (s *Store) TouchRemoteConsumer(ctx context.Context, consumerID, connectionID string) error {
	if err := validateRemoteConsumerIDs(consumerID, connectionID); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE remote_consumers SET last_seen_at=?
		WHERE consumer_id=? AND connection_id=?`, time.Now().UTC().UnixMilli(), consumerID, connectionID)
	if err != nil {
		return fmt.Errorf("renew remote consumer lease: %w", err)
	}
	return requireRemoteConsumerOwner(result)
}

// CloseRemoteConsumer releases only this connection's claim. A stale close is
// harmless and cannot clear a replacement connection's ownership.
func (s *Store) CloseRemoteConsumer(ctx context.Context, consumerID, connectionID string) error {
	if err := validateRemoteConsumerIDs(consumerID, connectionID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE remote_consumers SET connection_id=NULL
		WHERE consumer_id=? AND connection_id=?`, consumerID, connectionID)
	if err != nil {
		return fmt.Errorf("close remote consumer: %w", err)
	}
	return nil
}

func validateRemoteConsumerIDs(consumerID, connectionID string) error {
	if !canonicalUUID(consumerID) || !canonicalUUID(connectionID) {
		return fmt.Errorf("consumer and connection IDs must be canonical UUIDs")
	}
	return nil
}

func ownedRemoteCursor(ctx context.Context, q queryer, consumerID, connectionID string) (int64, error) {
	var cursor int64
	err := q.QueryRowContext(ctx, `SELECT cursor FROM remote_consumers
		WHERE consumer_id=? AND connection_id=?`, consumerID, connectionID).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrRemoteConsumerReplaced
	}
	if err != nil {
		return 0, fmt.Errorf("check remote consumer ownership: %w", err)
	}
	return cursor, nil
}

func updateOwnedRemoteCursor(ctx context.Context, tx *sql.Tx, consumerID, connectionID string, cursor, now int64) error {
	result, err := tx.ExecContext(ctx, `UPDATE remote_consumers SET cursor=MAX(cursor,?),last_seen_at=?
		WHERE consumer_id=? AND connection_id=?`, cursor, now, consumerID, connectionID)
	if err != nil {
		return fmt.Errorf("advance remote consumer cursor: %w", err)
	}
	return requireRemoteConsumerOwner(result)
}

func requireRemoteConsumerOwner(result sql.Result) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect remote consumer ownership update: %w", err)
	}
	if changed != 1 {
		return ErrRemoteConsumerReplaced
	}
	return nil
}

type remoteEventSequence struct {
	seq int64
	uid string
}

func readRemoteEventSequenceBatch(ctx context.Context, q queryer, after int64) ([]remoteEventSequence, error) {
	rows, err := q.QueryContext(ctx, `SELECT seq,uid FROM router_events WHERE seq>? ORDER BY seq LIMIT ?`, after, remoteConsumerScanBatch)
	if err != nil {
		return nil, fmt.Errorf("scan remote router events: %w", err)
	}
	defer rows.Close()
	var batch []remoteEventSequence
	for rows.Next() {
		var row remoteEventSequence
		if err := rows.Scan(&row.seq, &row.uid); err != nil {
			return nil, err
		}
		batch = append(batch, row)
	}
	return batch, rows.Err()
}

func remoteEventMatches(event fism.Event, events, targets map[string]struct{}) bool {
	if _, ok := events[event.ID]; !ok {
		return false
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(event.Payload, &payload) != nil || payload == nil {
		return true
	}
	targetRaw, exists := payload["target"]
	if !exists || isJSONNull(targetRaw) {
		return true
	}
	var target string
	if json.Unmarshal(targetRaw, &target) != nil {
		return true
	}
	_, ok := targets[target]
	return ok
}
