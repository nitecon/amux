package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayorigin"
	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/projects"
)

var (
	ErrWorkerPoolInput    = errors.New("invalid worker pool input")
	ErrWorkerPoolConflict = errors.New("worker pool state conflict")
	ErrWorkerPoolNotFound = errors.New("worker pool not found")
)

type WorkerPool struct {
	ID        string   `json:"id"`
	Server    string   `json:"server"`
	Project   string   `json:"project"`
	Directory string   `json:"directory"`
	Provider  string   `json:"provider"`
	OS        string   `json:"os"`
	Command   []string `json:"command,omitempty"`
	SlotCount int      `json:"slot_count"`
	Goal      *string  `json:"goal,omitempty"`
}

type WorkerPoolSlot struct {
	ID             string      `json:"id"`
	PoolID         string      `json:"pool_id"`
	Owner          AgentOrigin `json:"owner"`
	RunID          string      `json:"run_id,omitempty"`
	ReportSequence int64       `json:"report_sequence"`
	LastActivityAt int64       `json:"last_activity_at"`
}

func migrateWorkerPool(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS amux_worker_pools(
 id TEXT PRIMARY KEY,server TEXT NOT NULL,project TEXT NOT NULL,directory TEXT NOT NULL,
 provider TEXT NOT NULL,os TEXT NOT NULL,command_json TEXT NOT NULL,slot_count INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS amux_worker_pool_slots(
 id TEXT PRIMARY KEY,pool_id TEXT NOT NULL REFERENCES amux_worker_pools(id) ON DELETE CASCADE,
 owner_json TEXT NOT NULL,run_id TEXT NOT NULL DEFAULT '',report_sequence INTEGER NOT NULL DEFAULT 0,
 last_activity_at INTEGER NOT NULL);
 CREATE UNIQUE INDEX IF NOT EXISTS amux_worker_pool_slot_run ON amux_worker_pool_slots(run_id) WHERE run_id!='';
 CREATE INDEX IF NOT EXISTS amux_worker_pool_slots_pool ON amux_worker_pool_slots(pool_id,id);
 INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(11);`)
	return err
}

func localWorkerOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	default:
		return runtime.GOOS
	}
}

func validateWorkerPool(pool WorkerPool) error {
	if _, err := uuid.Parse(pool.ID); err != nil {
		return fmt.Errorf("%w: pool ID must be a UUID", ErrWorkerPoolInput)
	}
	if !validWorkerPoolServer(pool.Server) || strings.TrimSpace(pool.Project) == "" || !filepath.IsAbs(pool.Directory) || filepath.Clean(pool.Directory) != pool.Directory {
		return fmt.Errorf("%w: server, project and an absolute checkout directory are required", ErrWorkerPoolInput)
	}
	info, err := os.Stat(pool.Directory)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: checkout directory must exist", ErrWorkerPoolInput)
	}
	if pool.SlotCount < 1 || pool.SlotCount > 32 {
		return fmt.Errorf("%w: slot count must be between 1 and 32", ErrWorkerPoolInput)
	}
	if pool.Provider != "codex" && pool.Provider != "claude" && pool.Provider != "antigravity" {
		return fmt.Errorf("%w: unsupported provider", ErrWorkerPoolInput)
	}
	if pool.OS != "linux" && pool.OS != "windows" && pool.OS != "macos" || pool.OS != localWorkerOS() {
		return fmt.Errorf("%w: worker pool OS must match this host", ErrWorkerPoolInput)
	}
	command, err := json.Marshal(pool.Command)
	if err != nil || len(pool.Command) > 64 || len(command) > 65536 || len(pool.Command) > 0 && pool.Command[0] == "" {
		return fmt.Errorf("%w: invalid worker command", ErrWorkerPoolInput)
	}
	return nil
}

func validWorkerPoolServer(raw string) bool {
	u, err := gatewayorigin.Parse(raw)
	if err != nil || u.Hostname() == "" || u.ForceQuery || u.Path != "" || u.RawPath != "" || u.Opaque != "" {
		return false
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return false
		}
	}
	return true
}

func (s *Store) CreateWorkerPool(ctx context.Context, pool WorkerPool) (*WorkerPool, error) {
	pool.Project = projects.NormalizeIdent(pool.Project)
	goal, err := NormalizeTaskGoal(pool.Goal)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWorkerPoolInput, err)
	}
	pool.Goal = goal
	if err := validateWorkerPool(pool); err != nil {
		return nil, err
	}
	checkout, err := projectidentity.Resolve(ctx, pool.Directory)
	if err != nil {
		return nil, fmt.Errorf("resolve worker pool checkout: %w", err)
	}
	if checkout.Upstream != "" {
		pool.Directory = checkout.Directory
	}
	command, _ := json.Marshal(pool.Command)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO amux_worker_pools(id,server,project,directory,provider,os,command_json,slot_count,goal) VALUES(?,?,?,?,?,?,?,?,?)`, pool.ID, pool.Server, pool.Project, pool.Directory, pool.Provider, pool.OS, string(command), pool.SlotCount, nullableString(pool.Goal))
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	for range pool.SlotCount {
		owner := AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: pool.Provider, OS: pool.OS}
		ownerJSON, _ := json.Marshal(owner)
		if _, err = tx.ExecContext(ctx, `INSERT INTO amux_worker_pool_slots(id,pool_id,owner_json,last_activity_at) VALUES(?,?,?,?)`, uuid.NewString(), pool.ID, string(ownerJSON), now); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return readWorkerPool(ctx, s.db, pool.ID)
}

func readWorkerPool(ctx context.Context, q queryer, id string) (*WorkerPool, error) {
	var pool WorkerPool
	var command string
	var goal sql.NullString
	err := q.QueryRowContext(ctx, `SELECT id,server,project,directory,provider,os,command_json,slot_count,goal FROM amux_worker_pools WHERE id=?`, id).Scan(&pool.ID, &pool.Server, &pool.Project, &pool.Directory, &pool.Provider, &pool.OS, &command, &pool.SlotCount, &goal)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWorkerPoolNotFound
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(command), &pool.Command); err != nil {
		return nil, err
	}
	pool.Goal = optionalSQLString(goal)
	return &pool, nil
}

// WorkerPools lists the durable pools bound to this gateway.
func (s *Store) WorkerPools(ctx context.Context, endpoint string) ([]WorkerPool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM amux_worker_pools WHERE server=? ORDER BY project,directory,id", endpoint)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	pools := make([]WorkerPool, 0, len(ids))
	for _, id := range ids {
		pool, err := s.WorkerPool(ctx, id)
		if err != nil {
			return nil, err
		}
		pools = append(pools, *pool)
	}
	return pools, nil
}

func (s *Store) WorkerPool(ctx context.Context, id string) (*WorkerPool, error) {
	return readWorkerPool(ctx, s.db, id)
}

func scanWorkerPoolSlot(row interface{ Scan(...any) error }) (*WorkerPoolSlot, error) {
	var slot WorkerPoolSlot
	var owner string
	err := row.Scan(&slot.ID, &slot.PoolID, &owner, &slot.RunID, &slot.ReportSequence, &slot.LastActivityAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWorkerPoolNotFound
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal([]byte(owner), &slot.Owner)
	return &slot, err
}

func readWorkerPoolSlot(ctx context.Context, q queryer, poolID, slotID string) (*WorkerPoolSlot, error) {
	return scanWorkerPoolSlot(q.QueryRowContext(ctx, `SELECT id,pool_id,owner_json,run_id,report_sequence,last_activity_at FROM amux_worker_pool_slots WHERE pool_id=? AND id=?`, poolID, slotID))
}

func (s *Store) WorkerPoolSlots(ctx context.Context, poolID string) ([]WorkerPoolSlot, error) {
	if _, err := readWorkerPool(ctx, s.db, poolID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,pool_id,owner_json,run_id,report_sequence,last_activity_at FROM amux_worker_pool_slots WHERE pool_id=? ORDER BY id`, poolID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []WorkerPoolSlot{}
	for rows.Next() {
		slot, err := scanWorkerPoolSlot(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *slot)
	}
	return result, rows.Err()
}

func (s *Store) NextWorkerPoolReport(ctx context.Context, poolID, slotID string) (*WorkerPoolSlot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	slot, err := readWorkerPoolSlot(ctx, tx, poolID, slotID)
	if err != nil {
		return nil, err
	}
	if slot.RunID != "" {
		run, err := readWorkerRun(ctx, tx, slot.RunID)
		if errors.Is(err, ErrConversationNotFound) {
			return nil, ErrWorkerPoolConflict
		}
		if err != nil {
			return nil, err
		}
		slot.Owner = run.Owner
	}
	if slot.ReportSequence == 1<<63-1 {
		return nil, ErrWorkerPoolConflict
	}
	slot.ReportSequence++
	owner, _ := json.Marshal(slot.Owner)
	if _, err = tx.ExecContext(ctx, `UPDATE amux_worker_pool_slots SET owner_json=?,report_sequence=? WHERE pool_id=? AND id=?`, string(owner), slot.ReportSequence, poolID, slotID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return slot, nil
}

func sameWorkerPoolRunConfig(pool *WorkerPool, run WorkerRun) bool {
	return run.Server == pool.Server && run.Project == pool.Project && run.Directory == pool.Directory && run.Owner.Provider == pool.Provider && run.Owner.OS == pool.OS && slices.Equal(run.Command, pool.Command) && sameOptionalString(run.Goal, pool.Goal)
}

func sameWorkerRunConfig(a, b WorkerRun) bool {
	return a.ID == b.ID && a.Server == b.Server && a.Project == b.Project && a.ConversationID == b.ConversationID && a.Directory == b.Directory && a.Owner.SessionID == b.Owner.SessionID && a.Owner.Provider == b.Owner.Provider && a.Owner.OS == b.Owner.OS && slices.Equal(a.Command, b.Command) && sameOptionalString(a.Goal, b.Goal)
}

func (s *Store) AttachWorkerPoolRun(ctx context.Context, poolID, slotID string, run WorkerRun) error {
	goal, err := NormalizeTaskGoal(run.Goal)
	if err != nil {
		return fmt.Errorf("%w: invalid run goal", ErrWorkerPoolInput)
	}
	run.Goal = goal
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	pool, err := readWorkerPool(ctx, tx, poolID)
	if err != nil {
		return err
	}
	slot, err := readWorkerPoolSlot(ctx, tx, poolID, slotID)
	if err != nil {
		return err
	}
	if !sameWorkerPoolRunConfig(pool, run) {
		return ErrWorkerPoolConflict
	}
	if _, err = uuid.Parse(run.ID); err != nil {
		return ErrWorkerPoolInput
	}
	if slot.RunID != "" {
		if slot.RunID != run.ID {
			return ErrWorkerPoolConflict
		}
		saved, err := readWorkerRun(ctx, tx, run.ID)
		if err != nil || !sameWorkerRunConfig(*saved, run) {
			return ErrWorkerPoolConflict
		}
		return tx.Commit()
	}
	if run.Owner != slot.Owner {
		return ErrWorkerPoolConflict
	}
	if run.NativeSessionID != "" || run.PendingInstanceID != "" || run.CommandCursor != 0 || run.EventCursor != 0 || run.OutboxCursor != 0 {
		return ErrWorkerPoolInput
	}
	if _, err = readWorkerRun(ctx, tx, run.ID); err == nil {
		return ErrWorkerPoolConflict
	} else if !errors.Is(err, ErrConversationNotFound) {
		return err
	}
	if err = insertWorkerRun(ctx, tx, run); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE amux_worker_pool_slots SET run_id=? WHERE pool_id=? AND id=? AND run_id=''`, run.ID, poolID, slotID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrWorkerPoolConflict
	}
	return tx.Commit()
}

func (s *Store) FinishWorkerPoolRun(ctx context.Context, poolID, slotID, runID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	slot, err := readWorkerPoolSlot(ctx, tx, poolID, slotID)
	if err != nil {
		return err
	}
	if slot.RunID != runID || runID == "" {
		return ErrWorkerPoolConflict
	}
	run, err := readWorkerRun(ctx, tx, runID)
	if errors.Is(err, ErrConversationNotFound) {
		return ErrWorkerPoolConflict
	}
	if err != nil {
		return err
	}
	owner, _ := json.Marshal(run.Owner)
	_, err = tx.ExecContext(ctx, `UPDATE amux_worker_pool_slots SET owner_json=?,run_id='',last_activity_at=? WHERE pool_id=? AND id=? AND run_id=?`, string(owner), now.UnixMilli(), poolID, slotID, runID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
