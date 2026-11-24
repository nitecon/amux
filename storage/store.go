// Package storage owns durable SQLite data and translates database values at its boundary.
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nitecon/amux/eventbus"
	_ "modernc.org/sqlite"
)

const maxGoMigrationVersion = conversationTaskOptionalMigration

// Store owns its database connections. Callers must close it after stopping workers.
type Store struct {
	db                  *sql.DB
	eventMu             sync.Mutex
	eventChanges        chan struct{}
	conversationChanges chan struct{}
	workerTaskChanges   chan struct{}
	bus                 *eventbus.Bus
	workerRuns          sync.Map
	routerReviews       atomic.Bool
}

// Open opens an explicit local database path and transactionally applies the
// ported migrations. It preserves unrelated Rust tables and never changes the
// database's user_version. Additional domain migrations are added as they port.
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" || path == ":memory:" {
		return nil, fmt.Errorf("a durable SQLite file path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("database must be a regular file")
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect database: %w", err)
	}
	if os.IsNotExist(err) {
		f, err := os.OpenFile(absolute, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, fmt.Errorf("create database: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	filePath := filepath.ToSlash(absolute)
	if !strings.HasPrefix(filePath, "/") {
		filePath = "/" + filePath
	}
	u := url.URL{Scheme: "file", Path: filePath}
	query := url.Values{}
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Set("_txlock", "immediate")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	// Match the Rust single-writer connection boundary. Transactions serialize
	// read/check/write invariants; separate server instances still use SQLite locks.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// Reject newer profiles before any legacy table rebuild or backup.
	var hasVersions bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='amux_go_migrations')").Scan(&hasVersions); err != nil {
		db.Close()
		return nil, err
	}
	if hasVersions {
		var latest int
		if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM amux_go_migrations").Scan(&latest); err != nil {
			db.Close()
			return nil, err
		}
		if latest > maxGoMigrationVersion {
			db.Close()
			return nil, fmt.Errorf("database requires a newer Go migration version: %d", latest)
		}
	}
	if err := migrateLegacyTaskStatus(ctx, db, absolute); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade legacy task statuses: %w", err)
	}
	if err := migrateLegacyRouterDeliveries(ctx, db, absolute); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade legacy router deliveries: %w", err)
	}
	if err := migrateLegacyPresenceAuthority(ctx, db, absolute); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade presence authority: %w", err)
	}
	if err := migrateLegacyConversationTaskOptional(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade optional conversation task binding: %w", err)
	}
	s := &Store{db: db, eventChanges: make(chan struct{}), conversationChanges: make(chan struct{}), workerTaskChanges: make(chan struct{}), bus: eventbus.New()}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate SQLite: %w", err)
	}
	if err := retireLegacyExecutionRuns(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("retire legacy execution runs: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	s.bus.Close()
	return s.db.Close()
}

// UseRouterReviews selects durable router delivery as the production owner of
// automatic review generation. Call before serving task mutations.
func (s *Store) UseRouterReviews() { s.routerReviews.Store(true) }

// EventBus is the shared in-process wake bus. Durable SQLite state remains
// authoritative; subscribers reconcile after receiving events or ticks.
func (s *Store) EventBus() *eventbus.Bus { return s.bus }

type queryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS amux_go_migrations(version INTEGER PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS projects(ident TEXT PRIMARY KEY,channel_name TEXT NOT NULL,room_id TEXT NOT NULL,last_msg_id TEXT,created_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS cursors(project_ident TEXT PRIMARY KEY REFERENCES projects(ident),last_read_id INTEGER NOT NULL DEFAULT 0,updated_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);`); err != nil {
		return err
	}
	var latest int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM amux_go_migrations").Scan(&latest); err != nil {
		return err
	}
	if latest > maxGoMigrationVersion {
		return fmt.Errorf("database requires a newer Go migration version: %d", latest)
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info(projects)")
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"repo_provider", "TEXT"}, {"repo_namespace", "TEXT"}, {"repo_name", "TEXT"}, {"repo_full_name", "TEXT"},
		{"kind", "TEXT NOT NULL DEFAULT 'repo'"}, {"archived_at", "INTEGER"}, {"canonical_remote", "TEXT"},
	} {
		if !columns[column.name] {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE projects ADD COLUMN "+column.name+" "+column.definition); err != nil {
				return err
			}
		}
	}
	// Match Rust's fail-closed migration behavior: do not silently rename legacy
	// projects, which would disconnect foreign keys, task identities and history.
	existing, err := readProjects(ctx, tx, false)
	if err != nil {
		return err
	}
	mappings := map[string]string{}
	for _, p := range existing {
		if p.Ident != strings.ToLower(strings.TrimSpace(p.Ident)) {
			return fmt.Errorf("projects.ident %q must be lowercase", p.Ident)
		}
		for _, value := range []*string{p.RepoProvider, p.RepoNamespace, p.RepoName, p.RepoFullName} {
			if value != nil && *value != strings.ToLower(strings.TrimSpace(*value)) {
				return fmt.Errorf("project repository metadata must be lowercase")
			}
		}
		if p.RepoFullName != nil {
			provider := ""
			if p.RepoProvider != nil {
				provider = *p.RepoProvider
			}
			key := provider + "\x00" + *p.RepoFullName
			if prior, ok := mappings[key]; ok && prior != p.Ident {
				return fmt.Errorf("repository already mapped to another project")
			}
			mappings[key] = p.Ident
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_ident_nocase ON projects(ident COLLATE NOCASE);
 CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_repo_mapping_nocase ON projects(repo_provider COLLATE NOCASE,repo_full_name COLLATE NOCASE) WHERE repo_provider IS NOT NULL AND repo_full_name IS NOT NULL;
 INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(1);`); err != nil {
		return err
	}
	if err := migrateTaskEvents(ctx, tx); err != nil {
		return err
	}
	if err := migrateTasks(ctx, tx); err != nil {
		return err
	}
	// Task snapshot triggers refer to delegation readiness; install the table
	// before later workflow migrations can fire those triggers on legacy tasks.
	if err := ensureTaskDelegationSchema(ctx, tx); err != nil {
		return err
	}
	if err := migrateTaskWorkflow(ctx, tx); err != nil {
		return err
	}
	if err := migrateConversations(ctx, tx); err != nil {
		return err
	}
	if err := recordConversationTaskOptionalMigration(ctx, tx); err != nil {
		return err
	}
	if err := migrateConversationCommands(ctx, tx); err != nil {
		return err
	}
	if err := migrateWorker(ctx, tx); err != nil {
		return err
	}
	if err := migratePresence(ctx, tx); err != nil {
		return err
	}
	if err := migrateAssignments(ctx, tx); err != nil {
		return err
	}
	if err := migrateAssignmentAdoption(ctx, tx); err != nil {
		return err
	}
	if err := migrateWorkerPool(ctx, tx); err != nil {
		return err
	}
	if err := migrateWorkerTaskInbox(ctx, tx); err != nil {
		return err
	}
	if err := migrateReviewPresence(ctx, tx); err != nil {
		return err
	}
	if err := migrateReviewAssignment(ctx, tx); err != nil {
		return err
	}
	if err := migrateTaskCancellations(ctx, tx); err != nil {
		return err
	}
	if err := migrateRouter(ctx, tx); err != nil {
		return err
	}
	if err := migrateRouterTaskOutbox(ctx, tx); err != nil {
		return err
	}
	if err := migrateRouterPresence(ctx, tx); err != nil {
		return err
	}
	if err := migratePresenceHostname(ctx, tx); err != nil {
		return err
	}
	if err := migrateRouterHeartbeat(ctx, tx); err != nil {
		return err
	}
	if err := migratePresenceSummary(ctx, tx); err != nil {
		return err
	}
	if err := migrateRouterReminders(ctx, tx); err != nil {
		return err
	}
	if err := migratePresenceRouting(ctx, tx); err != nil {
		return err
	}
	if err := migrateTargetRegistration(ctx, tx); err != nil {
		return err
	}
	if err := migrateRouterNotifier(ctx, tx); err != nil {
		return err
	}
	if err := migrateRetentionMessages(ctx, tx); err != nil {
		return err
	}
	if err := migrateRouterRetention(ctx, tx); err != nil {
		return err
	}
	if err := migratePresenceObservations(ctx, tx); err != nil {
		return err
	}
	if err := migrateRouterInbound(ctx, tx); err != nil {
		return err
	}
	if err := migrateWorkerGoals(ctx, tx); err != nil {
		return err
	}
	if err := migrateRouterAssignmentFeedback(ctx, tx); err != nil {
		return err
	}
	if err := migrateNativeSourcePrincipals(ctx, tx); err != nil {
		return err
	}
	if err := migrateNativeDelivery(ctx, tx); err != nil {
		return err
	}
	if err := migrateWorkerPresence(ctx, tx); err != nil {
		return err
	}
	if err := migrateTaskDelegations(ctx, tx); err != nil {
		return err
	}
	if err := migrateProjectMessages(ctx, tx); err != nil {
		return err
	}
	if err := migrateMachineCheckouts(ctx, tx); err != nil {
		return err
	}
	if err := migrateArtifactBase(ctx, tx); err != nil {
		return err
	}
	if err := migrateArtifactChunks(ctx, tx); err != nil {
		return err
	}
	if err := migrateArtifactWorkflowRuns(ctx, tx); err != nil {
		return err
	}
	if err := migrateArtifactDiscourse(ctx, tx); err != nil {
		return err
	}
	if err := migrateArtifactLinks(ctx, tx); err != nil {
		return err
	}
	if err := migrateProjectLinks(ctx, tx); err != nil {
		return err
	}
	if err := migratePatterns(ctx, tx); err != nil {
		return err
	}
	if err := migrateSkills(ctx, tx); err != nil {
		return err
	}
	if err := migrateGatewayMemories(ctx, tx); err != nil {
		return err
	}
	if err := migrateUserAccounts(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
