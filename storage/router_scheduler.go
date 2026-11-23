package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

const routerRuntimeLeaseKey = "router-runtime-lease"
const routerClaimPageSize = 256
const routerRuntimeClaimLimit = 64

type routerActiveClaimKey struct {
	seq    int64
	plugin string
}

type routerActiveClaim struct {
	uid      string
	failures int64
}

type routerRuntimeLease struct {
	mu           sync.Mutex
	active       bool
	token        string
	activeClaims map[routerActiveClaimKey]routerActiveClaim
}

type routerDeliveryCandidate struct {
	seq      int64
	plugin   string
	eventID  string
	due      int64
	failures int64
}

func (s *Store) routerLease() *routerRuntimeLease {
	value, _ := s.workerRuns.LoadOrStore(routerRuntimeLeaseKey, &routerRuntimeLease{})
	return value.(*routerRuntimeLease)
}

func (s *Store) AcquireRouterRuntime(ctx context.Context, registry *router.Registry, nowMS int64) (string, error) {
	if registry == nil {
		return "", fmt.Errorf("router registry is nil")
	}
	lease := s.routerLease()
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if lease.active {
		return "", router.ErrRuntimeInactive
	}
	token, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate router runtime token: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	changed := false
	result, err := tx.ExecContext(ctx, `UPDATE router_deliveries SET state='pending',updated_at=? WHERE state='running'`, nowMS)
	if err != nil {
		return "", err
	}
	if n, _ := result.RowsAffected(); n != 0 {
		changed = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT d.event_seq,d.plugin,e.id,e.version
		FROM router_deliveries d JOIN router_events e ON e.seq=d.event_seq
		WHERE d.state IN ('pending','running','retrying') ORDER BY d.event_seq,d.plugin`)
	if err != nil {
		return "", err
	}
	type removed struct {
		seq    int64
		plugin string
	}
	var removedSubscriptions []removed
	for rows.Next() {
		var seq int64
		var plugin, eventID string
		var version uint32
		if err := rows.Scan(&seq, &plugin, &eventID, &version); err != nil {
			rows.Close()
			return "", err
		}
		if !registry.Subscribes(plugin, eventID, version) {
			removedSubscriptions = append(removedSubscriptions, removed{seq: seq, plugin: plugin})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	for _, item := range removedSubscriptions {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		result, err := tx.ExecContext(ctx, `UPDATE router_deliveries SET state='skipped',last_error='subscription is not registered',updated_at=?
			WHERE event_seq=? AND plugin=? AND state IN ('pending','running','retrying')`, nowMS, item.seq, item.plugin)
		if err != nil {
			return "", err
		}
		if n, _ := result.RowsAffected(); n != 0 {
			changed = true
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	lease.token = token.String()
	lease.active = true
	lease.activeClaims = make(map[routerActiveClaimKey]routerActiveClaim)
	if changed {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	}
	return lease.token, nil
}

func (s *Store) ClaimRouterDeliveries(ctx context.Context, token string, registry *router.Registry, nowMS int64, capacity int) (router.ClaimBatch, error) {
	batch := router.ClaimBatch{Runs: make([]router.ClaimedRun, 0)}
	if registry == nil {
		return batch, fmt.Errorf("router registry is nil")
	}
	lease := s.routerLease()
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if err := validRouterRuntime(ctx, lease, token); err != nil {
		return batch, err
	}
	if capacity <= 0 {
		return batch, nil
	}
	available := routerRuntimeClaimLimit - len(lease.activeClaims)
	if available <= 0 {
		return batch, nil
	}
	if capacity > available {
		capacity = available
	}
	plugins := registry.PluginIDs()
	if len(plugins) == 0 {
		return batch, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return batch, err
	}
	defer tx.Rollback()
	fifoHeads := make(map[string]*int64)
	parallel := make([]string, 0, len(plugins))
	for _, plugin := range plugins {
		if !registry.FIFO(plugin) {
			parallel = append(parallel, plugin)
			continue
		}
		var head sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MIN(event_seq) FROM router_deliveries
			WHERE plugin=? AND state IN ('pending','running','retrying')`, plugin).Scan(&head); err != nil {
			return batch, err
		}
		if head.Valid {
			value := head.Int64
			fifoHeads[plugin] = &value
		} else {
			fifoHeads[plugin] = nil
		}
	}
	fifoJSON, err := json.Marshal(fifoHeads)
	if err != nil {
		return batch, err
	}
	parallelJSON, err := json.Marshal(parallel)
	if err != nil {
		return batch, err
	}
	query := `SELECT d.event_seq,d.plugin,e.id,d.next_attempt_at,d.attempts
		FROM json_each(?) fifo
		JOIN router_deliveries d ON d.plugin=fifo.key AND d.event_seq=fifo.value
		JOIN router_events e ON e.seq=d.event_seq
		WHERE d.state IN ('pending','retrying') AND (d.event_seq,d.plugin)>(?,?)
		UNION ALL
		SELECT d.event_seq,d.plugin,e.id,d.next_attempt_at,d.attempts
		FROM json_each(?) parallel
		CROSS JOIN router_deliveries d INDEXED BY idx_router_deliveries_plugin_open
		JOIN router_events e ON e.seq=d.event_seq
		WHERE d.plugin=parallel.value AND d.state IN ('pending','running','retrying') AND d.state!='running'
		AND (d.event_seq,d.plugin)>(?,?)
		ORDER BY 1,2 LIMIT ` + fmt.Sprint(routerClaimPageSize)
	var afterSeq int64 = -1
	afterPlugin := ""
	changed := false
	for len(batch.Runs) < capacity {
		rows, err := tx.QueryContext(ctx, query, string(fifoJSON), afterSeq, afterPlugin, string(parallelJSON), afterSeq, afterPlugin)
		if err != nil {
			return batch, err
		}
		page := make([]routerDeliveryCandidate, 0, routerClaimPageSize)
		for rows.Next() {
			var candidate routerDeliveryCandidate
			if err := rows.Scan(&candidate.seq, &candidate.plugin, &candidate.eventID, &candidate.due, &candidate.failures); err != nil {
				rows.Close()
				return batch, err
			}
			page = append(page, candidate)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return batch, err
		}
		if err := rows.Close(); err != nil {
			return batch, err
		}
		if len(page) == 0 {
			break
		}
		for _, candidate := range page {
			afterSeq, afterPlugin = candidate.seq, candidate.plugin
			ready, blocked := true, false
			for _, dependency := range registry.Dependencies(candidate.plugin, candidate.eventID) {
				var state string
				err := tx.QueryRowContext(ctx, `SELECT state FROM router_deliveries WHERE event_seq=? AND plugin=?`, candidate.seq, dependency).Scan(&state)
				if errors.Is(err, sql.ErrNoRows) {
					blocked = true
					continue
				}
				if err != nil {
					return batch, err
				}
				switch state {
				case "completed":
				case "stopped", "skipped", "failed":
					blocked = true
				default:
					ready = false
				}
			}
			if blocked {
				if err := ctx.Err(); err != nil {
					return batch, err
				}
				result, err := tx.ExecContext(ctx, `UPDATE router_deliveries SET state='skipped',last_error='dependency did not complete',updated_at=?
					WHERE event_seq=? AND plugin=? AND state IN ('pending','retrying')`, nowMS, candidate.seq, candidate.plugin)
				if err != nil {
					return batch, err
				}
				if n, _ := result.RowsAffected(); n != 0 {
					batch.Skipped, changed = true, true
				}
				continue
			}
			if !ready {
				continue
			}
			if candidate.due > nowMS {
				if batch.NextAttemptAt == nil || candidate.due < *batch.NextAttemptAt {
					due := candidate.due
					batch.NextAttemptAt = &due
				}
				continue
			}
			if err := ctx.Err(); err != nil {
				return batch, err
			}
			result, err := tx.ExecContext(ctx, `UPDATE router_deliveries SET state='running',updated_at=?
				WHERE event_seq=? AND plugin=? AND state IN ('pending','retrying') AND next_attempt_at<=?`, nowMS, candidate.seq, candidate.plugin, nowMS)
			if err != nil {
				return batch, err
			}
			n, _ := result.RowsAffected()
			if n == 0 {
				continue
			}
			event, err := readRouterEventBySeq(ctx, tx, candidate.seq)
			if err != nil {
				return batch, err
			}
			batch.Runs = append(batch.Runs, router.ClaimedRun{Event: event, Plugin: candidate.plugin, Failures: candidate.failures})
			changed = true
			if len(batch.Runs) == capacity {
				break
			}
		}
		if len(page) < routerClaimPageSize || len(batch.Runs) == capacity {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return router.ClaimBatch{Runs: make([]router.ClaimedRun, 0)}, err
	}
	if err := tx.Commit(); err != nil {
		return router.ClaimBatch{Runs: make([]router.ClaimedRun, 0)}, err
	}
	if lease.activeClaims == nil {
		lease.activeClaims = make(map[routerActiveClaimKey]routerActiveClaim)
	}
	for _, run := range batch.Runs {
		lease.activeClaims[routerActiveClaimKey{seq: run.Event.Seq, plugin: run.Plugin}] = routerActiveClaim{uid: run.Event.UID, failures: run.Failures}
	}
	if changed {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	}
	return batch, nil
}

func (s *Store) FinishRouterDelivery(ctx context.Context, token string, registry *router.Registry, run router.ClaimedRun, outcome router.Outcome, nowMS int64) (returnErr error) {
	if registry == nil {
		return fmt.Errorf("router registry is nil")
	}
	lease := s.routerLease()
	lease.mu.Lock()
	defer lease.mu.Unlock()
	defer func() {
		if returnErr != nil {
			return
		}
		key := routerActiveClaimKey{seq: run.Event.Seq, plugin: run.Plugin}
		claim, ok := lease.activeClaims[key]
		if ok && claim.uid == run.Event.UID && claim.failures == run.Failures {
			delete(lease.activeClaims, key)
		}
	}()
	if err := validRouterRuntime(ctx, lease, token); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	var attempts int64
	var uid string
	var eventID string
	err = tx.QueryRowContext(ctx, `SELECT d.state,d.attempts,e.uid,e.id FROM router_deliveries d
		JOIN router_events e ON e.seq=d.event_seq WHERE d.event_seq=? AND d.plugin=?`, run.Event.Seq, run.Plugin).Scan(&state, &attempts, &uid, &eventID)
	if err != nil {
		return err
	}
	if uid != run.Event.UID {
		return fmt.Errorf("router delivery event identity changed")
	}
	if outcome.Kind == router.Fail && attempts > run.Failures {
		return nil
	}
	if state != "running" {
		if outcome.Kind == router.Continue && state == "completed" && attempts == run.Failures {
			return nil
		}
		if outcome.Kind == router.Stop && state == "stopped" && attempts == run.Failures {
			return nil
		}
		return fmt.Errorf("router delivery is not running")
	}
	if attempts != run.Failures {
		return fmt.Errorf("router delivery failure count changed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	changed := false
	switch outcome.Kind {
	case router.Continue, router.Stop:
		nextState := "completed"
		if outcome.Kind == router.Stop {
			nextState = "stopped"
		}
		result, err := tx.ExecContext(ctx, `UPDATE router_deliveries SET state=?,last_error=NULL,updated_at=?
			WHERE event_seq=? AND plugin=? AND state='running' AND attempts=?`, nextState, nowMS, run.Event.Seq, run.Plugin, run.Failures)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			return fmt.Errorf("router delivery finish lost its compare-and-swap")
		}
		changed = n != 0
	case router.Fail:
		if eventID != run.Event.ID {
			return fmt.Errorf("router delivery event identity changed")
		}
		failures := run.Failures + 1
		delay := router.RetryDelay(failures).Milliseconds()
		state := "retrying"
		if max := registry.MaxAttempts(run.Plugin); max != 0 && failures >= int64(max) {
			state = "failed"
		}
		var result sql.Result
		if state == "failed" {
			result, err = tx.ExecContext(ctx, `UPDATE router_deliveries SET state='failed',attempts=?,last_error=?,updated_at=?
				WHERE event_seq=? AND plugin=? AND state='running' AND attempts=?`, failures, outcome.Error, nowMS, run.Event.Seq, run.Plugin, run.Failures)
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE router_deliveries SET state='retrying',attempts=?,next_attempt_at=?,last_error=?,updated_at=?
				WHERE event_seq=? AND plugin=? AND state='running' AND attempts=?`, failures, nowMS+delay, outcome.Error, nowMS, run.Event.Seq, run.Plugin, run.Failures)
		}
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		changed = n != 0
		if n == 0 {
			return fmt.Errorf("router delivery finish lost its compare-and-swap")
		}
		if state == "retrying" && delay == router.RetryDelay(6).Milliseconds() && router.RetryDelay(failures-1).Milliseconds() < delay && eventID != "router.retrying" {
			payload, err := json.Marshal(struct {
				EventUID string `json:"event_uid"`
				Event    string `json:"event"`
				Plugin   string `json:"plugin"`
				Attempts int64  `json:"attempts"`
				Error    string `json:"error"`
			}{EventUID: uid, Event: eventID, Plugin: run.Plugin, Attempts: failures, Error: outcome.Error})
			if err != nil {
				return fmt.Errorf("encode router retry notification: %w", err)
			}
			correlation := uid
			if _, err := emitRouterEventIn(ctx, tx, registry, router.EmitRequest{ID: "router.retrying", Version: 1, Source: "router", Payload: payload, Correlation: &correlation}); err != nil {
				return fmt.Errorf("emit router retry notification: %w", err)
			}
		}
	default:
		return fmt.Errorf("unknown router outcome kind %d", outcome.Kind)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if changed {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	}
	return nil
}

func (s *Store) ReleaseRouterRuntime(ctx context.Context, token string) error {
	lease := s.routerLease()
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if !lease.active || lease.token != token {
		return nil
	}
	lease.active = false
	lease.token = ""
	lease.activeClaims = nil
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE router_deliveries SET state='pending',updated_at=? WHERE state='running'`, time.Now().UTC().UnixMilli())
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if err := tx.Commit(); err != nil {
		return err
	}
	if changed != 0 {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	}
	return nil
}

func validRouterRuntime(ctx context.Context, lease *routerRuntimeLease, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !lease.active || token == "" || lease.token != token {
		return router.ErrRuntimeInactive
	}
	return ctx.Err()
}

func readRouterEventBySeq(ctx context.Context, q queryer, seq int64) (router.Event, error) {
	var event router.Event
	var payload []byte
	var correlation sql.NullString
	err := q.QueryRowContext(ctx, `SELECT seq,uid,id,version,payload,source,emitted_at,correlation FROM router_events WHERE seq=?`, seq).
		Scan(&event.Seq, &event.UID, &event.ID, &event.Version, &payload, &event.Source, &event.EmittedAt, &correlation)
	if err != nil {
		return router.Event{}, err
	}
	event.Payload = append(event.Payload[:0], payload...)
	if correlation.Valid {
		event.Correlation = &correlation.String
	}
	return event, nil
}
