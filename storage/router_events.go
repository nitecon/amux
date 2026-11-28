package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
)

const routerDeliverySchema = `CREATE TABLE IF NOT EXISTS router_deliveries (
	event_seq INTEGER NOT NULL REFERENCES router_events(seq) ON DELETE CASCADE,
	plugin TEXT NOT NULL,
	state TEXT NOT NULL CHECK(state IN ('pending','running','retrying','completed','stopped','skipped','failed')),
	attempts INTEGER NOT NULL DEFAULT 0,
	next_attempt_at INTEGER NOT NULL,
	last_error TEXT,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY(event_seq, plugin)
)`

// migrateRouter establishes the Rust-compatible router journal. Delivery
// constraint widening is owned by the backup-protected legacy migration.
func migrateRouter(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS router_events (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	uid TEXT NOT NULL UNIQUE,
	id TEXT NOT NULL,
	version INTEGER NOT NULL,
	payload TEXT NOT NULL,
	source TEXT NOT NULL,
	emitted_at INTEGER NOT NULL,
	correlation TEXT
)`); err != nil {
		return fmt.Errorf("create router_events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, routerDeliverySchema); err != nil {
		return fmt.Errorf("create router_deliveries: %w", err)
	}
	var deliverySQL string
	if err := tx.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='router_deliveries'").Scan(&deliverySQL); err != nil {
		return fmt.Errorf("inspect router_deliveries schema: %w", err)
	}
	if !strings.Contains(strings.ToLower(deliverySQL), "'failed'") {
		return fmt.Errorf("router_deliveries state constraint does not include 'failed'; run the safe legacy delivery migration before router migration")
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_router_deliveries_open
	ON router_deliveries(state, next_attempt_at);
	CREATE INDEX IF NOT EXISTS idx_router_deliveries_plugin_open
	ON router_deliveries(plugin, event_seq)
	WHERE state IN ('pending','running','retrying');`); err != nil {
		return fmt.Errorf("create router delivery indexes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(15)"); err != nil {
		return fmt.Errorf("record router migration: %w", err)
	}
	return nil
}

// EmitRouterEvent appends the event and its pending deliveries atomically.
// SQLite is authoritative; the bus notification is sent only after commit.
func (s *Store) EmitRouterEvent(ctx context.Context, registry *fism.Registry, request fism.EmitRequest) (fism.EmitResult, error) {
	return s.publishEvent(ctx, registry, request, true)
}

// PublishBroadcast appends a WebSocket publication without admitting controller work.
func (s *Store) PublishBroadcast(ctx context.Context, registry *fism.Registry, request fism.EmitRequest) (fism.EmitResult, error) {
	return s.publishEvent(ctx, registry, request, false)
}

func (s *Store) publishEvent(ctx context.Context, registry *fism.Registry, request fism.EmitRequest, schedule bool) (fism.EmitResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fism.EmitResult{}, err
	}
	defer tx.Rollback()
	result, err := writeRouterEventIn(ctx, tx, registry, request, schedule)
	if err != nil {
		return fism.EmitResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return fism.EmitResult{}, err
	}
	if result.Stored {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	}
	return result, nil
}

// emitRouterEventIn persists to the caller's transaction under a savepoint.
// It never announces an event; only the outer transaction owner can do that.
func emitRouterEventIn(ctx context.Context, tx *sql.Tx, registry *fism.Registry, request fism.EmitRequest) (fism.EmitResult, error) {
	return writeRouterEventIn(ctx, tx, registry, request, true)
}

func writeRouterEventIn(ctx context.Context, tx *sql.Tx, registry *fism.Registry, request fism.EmitRequest, schedule bool) (fism.EmitResult, error) {
	payloadValue, err := validateRouterEmit(registry, &request)
	if err != nil {
		return fism.EmitResult{}, err
	}
	subscribers := append([]string(nil), registry.Subscribers(request.ID, request.Version)...)

	if existing, err := readRouterEventByUID(ctx, tx, request.UID); err == nil {
		storedPayload, decodeErr := semanticRouterJSON(existing.Payload)
		if decodeErr != nil {
			return fism.EmitResult{}, fmt.Errorf("decode stored router event payload: %w", decodeErr)
		}
		if existing.ID != request.ID || existing.Version != request.Version || existing.Source != request.Source || !sameOptionalString(existing.Correlation, request.Correlation) || !reflect.DeepEqual(storedPayload, payloadValue) {
			return fism.EmitResult{}, fism.ErrUIDConflict
		}
		return fism.EmitResult{Event: existing, Replayed: true}, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fism.EmitResult{}, err
	}

	event := fism.Event{UID: request.UID, ID: request.ID, Version: request.Version, Payload: append(json.RawMessage(nil), request.Payload...), Source: request.Source, Correlation: cloneString(request.Correlation)}
	if len(subscribers) == 0 {
		return fism.EmitResult{Event: event}, nil
	}

	now := time.Now().UTC().UnixMilli()
	event.EmittedAt = now
	if _, err := tx.ExecContext(ctx, "SAVEPOINT router_emit"); err != nil {
		return fism.EmitResult{}, fmt.Errorf("create router emit savepoint: %w", err)
	}
	insertResult, insertErr := tx.ExecContext(ctx, `INSERT INTO router_events(uid,id,version,payload,source,emitted_at,correlation)
	VALUES(?,?,?,?,?,?,?)`, request.UID, request.ID, request.Version, string(request.Payload), request.Source, now, nullableString(request.Correlation))
	if insertErr == nil {
		event.Seq, insertErr = insertResult.LastInsertId()
	}
	if insertErr == nil && schedule {
		for _, plugin := range subscribers {
			if _, insertErr = tx.ExecContext(ctx, `INSERT INTO router_deliveries(event_seq,plugin,state,next_attempt_at,updated_at)
			VALUES(?,?,'pending',?,?)`, event.Seq, plugin, now, now); insertErr != nil {
				break
			}
		}
	}
	if insertErr != nil {
		rollbackErr := cleanupRouterSavepoint(ctx, tx)
		return fism.EmitResult{}, errors.Join(fmt.Errorf("persist router event: %w", insertErr), rollbackErr)
	}
	if _, err := tx.ExecContext(ctx, "RELEASE router_emit"); err != nil {
		rollbackErr := cleanupRouterSavepoint(ctx, tx)
		return fism.EmitResult{}, errors.Join(fmt.Errorf("release router emit savepoint: %w", err), rollbackErr)
	}
	return fism.EmitResult{Event: event, Stored: true}, nil
}

func cleanupRouterSavepoint(ctx context.Context, tx *sql.Tx) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, rollbackErr := tx.ExecContext(cleanupCtx, "ROLLBACK TO router_emit")
	_, releaseErr := tx.ExecContext(cleanupCtx, "RELEASE router_emit")
	return errors.Join(wrapNonNil("rollback router emit savepoint", rollbackErr), wrapNonNil("release router emit savepoint after rollback", releaseErr))
}

func validateRouterEmit(registry *fism.Registry, request *fism.EmitRequest) (any, error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: registry is nil", fism.ErrInvalidEvent)
	}
	if !validRouterSource(request.Source) {
		return nil, fmt.Errorf("%w: source must be non-empty, trimmed, at most 256 bytes, and contain no control characters", fism.ErrInvalidEvent)
	}
	if request.UID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return nil, fmt.Errorf("generate router event UID: %w", err)
		}
		request.UID = id.String()
	} else if !canonicalUUID(request.UID) {
		return nil, fmt.Errorf("%w: UID must be a canonical UUID", fism.ErrInvalidEvent)
	}
	if request.Correlation != nil && !canonicalUUID(*request.Correlation) {
		return nil, fmt.Errorf("%w: correlation must be a canonical UUID", fism.ErrInvalidEvent)
	}
	if !json.Valid(request.Payload) {
		return nil, fmt.Errorf("%w: malformed payload JSON", fism.ErrInvalidEvent)
	}
	if err := registry.Validate(request.ID, request.Version, request.Payload); err != nil {
		return nil, err
	}
	value, err := semanticRouterJSON(request.Payload)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed payload JSON: %v", fism.ErrInvalidEvent, err)
	}
	return value, nil
}

func validRouterSource(source string) bool {
	if source == "" || source != strings.TrimSpace(source) || len(source) > 256 {
		return false
	}
	for _, r := range source {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func canonicalUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}

func semanticRouterJSON(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func sameOptionalString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func wrapNonNil(label string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", label, err)
}

// RouterEvents returns events with seq greater than after, in durable order.
func (s *Store) RouterEvents(ctx context.Context, after int64, limit int) ([]fism.Event, error) {
	if after < 0 {
		return nil, fmt.Errorf("router event cursor must be non-negative")
	}
	if limit < 1 || limit > 200 {
		return nil, fmt.Errorf("router event limit must be between 1 and 200")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq,uid,id,version,payload,source,emitted_at,correlation
	FROM router_events WHERE seq>? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]fism.Event, 0)
	for rows.Next() {
		var event fism.Event
		var payload []byte
		var correlation sql.NullString
		if err := rows.Scan(&event.Seq, &event.UID, &event.ID, &event.Version, &payload, &event.Source, &event.EmittedAt, &correlation); err != nil {
			return nil, err
		}
		event.Payload = append(json.RawMessage(nil), payload...)
		if correlation.Valid {
			event.Correlation = &correlation.String
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// RouterDeliveries returns every plugin delivery for one event sequence.
func (s *Store) RouterDeliveries(ctx context.Context, eventSeq int64) ([]fism.Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event_seq,plugin,state,attempts,next_attempt_at,last_error,updated_at
	FROM router_deliveries WHERE event_seq=? ORDER BY plugin`, eventSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deliveries := make([]fism.Delivery, 0)
	for rows.Next() {
		var delivery fism.Delivery
		var lastError sql.NullString
		if err := rows.Scan(&delivery.EventSeq, &delivery.Plugin, &delivery.State, &delivery.Attempts, &delivery.NextAttemptAt, &lastError, &delivery.UpdatedAt); err != nil {
			return nil, err
		}
		if lastError.Valid {
			delivery.LastError = &lastError.String
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, rows.Err()
}

func readRouterEventByUID(ctx context.Context, q queryer, uid string) (fism.Event, error) {
	var event fism.Event
	var payload []byte
	var correlation sql.NullString
	err := q.QueryRowContext(ctx, `SELECT seq,uid,id,version,payload,source,emitted_at,correlation
	FROM router_events WHERE uid=?`, uid).Scan(&event.Seq, &event.UID, &event.ID, &event.Version, &payload, &event.Source, &event.EmittedAt, &correlation)
	if err != nil {
		return fism.Event{}, err
	}
	event.Payload = append(json.RawMessage(nil), payload...)
	if correlation.Valid {
		event.Correlation = &correlation.String
	}
	return event, nil
}
