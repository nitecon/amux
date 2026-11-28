package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/nitecon/amux/identity/agent"
	projectid "github.com/nitecon/amux/identity/project"
)

const targetRegistrationLease = int64(120_000)

var ErrTargetBinding = errors.New("target registration conflicts with project binding")
var ErrTargetInput = errors.New("invalid target registration")

// TargetRegistration is an explicit manual routing advertisement. Presence
// reports do not create or renew this lease.
type TargetRegistration struct {
	Origin          agent.Origin `json:"origin"`
	Generation      string       `json:"generation"`
	CanonicalRemote *string      `json:"canonical_remote,omitempty"`
	DisplayName     *string      `json:"display_name,omitempty"`
	Hostname        *string      `json:"hostname,omitempty"`
}

// RegisteredTarget is a validated directory entry and its independent lease.
type RegisteredTarget struct {
	Target          RegisteredTargetIdentity `json:"target"`
	SeenAt          int64                    `json:"seen_at"`
	ExpiresAt       int64                    `json:"expires_at"`
	CanonicalRemote *string                  `json:"canonical_remote"`
	DisplayName     *string                  `json:"display_name"`
	Hostname        *string                  `json:"hostname"`
}

// RegisteredTargetIdentity is the routing identity returned in the Rust wire shape.
type RegisteredTargetIdentity struct {
	Origin     agent.Origin `json:"origin"`
	Generation string       `json:"generation"`
}

// TargetDirectory is one coherent target snapshot and its canonical project binding.
type TargetDirectory struct {
	Targets         []RegisteredTarget `json:"targets"`
	CanonicalRemote *string            `json:"canonical_remote"`
}

// migrateTargetRegistration adds the retained Rust target directory without
// rebuilding it, so its event observation columns and rows remain untouched.
func migrateTargetRegistration(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS task_target_presence (
 project_ident TEXT NOT NULL, instance_id TEXT NOT NULL, session_id TEXT NOT NULL,
 origin_json TEXT NOT NULL, generation TEXT NOT NULL,
 seen_at INTEGER NOT NULL DEFAULT 0, canonical_remote TEXT NOT NULL DEFAULT '',
 display_name TEXT, hostname TEXT, advertised INTEGER NOT NULL DEFAULT 1,
 source TEXT, agent_json TEXT, report_seen_at INTEGER, output_at INTEGER,
 draft INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL DEFAULT 'unknown' CHECK(state IN ('unknown','busy','idle')),
 identity_seq INTEGER NOT NULL DEFAULT 0, activity_seq INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(project_ident,instance_id,session_id)
);`); err != nil {
		return fmt.Errorf("create retained target directory: %w", err)
	}
	columns, err := retentionTableColumns(ctx, tx, "task_target_presence")
	if err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"origin_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"generation", "TEXT NOT NULL DEFAULT ''"},
		{"seen_at", "INTEGER NOT NULL DEFAULT 0"},
		{"canonical_remote", "TEXT NOT NULL DEFAULT ''"},
		{"display_name", "TEXT"},
		{"hostname", "TEXT"},
		{"advertised", "INTEGER NOT NULL DEFAULT 1"},
		{"source", "TEXT"},
		{"agent_json", "TEXT"},
		{"report_seen_at", "INTEGER"},
		{"output_at", "INTEGER"},
		{"draft", "INTEGER NOT NULL DEFAULT 0"},
		{"state", "TEXT NOT NULL DEFAULT 'unknown'"},
		{"identity_seq", "INTEGER NOT NULL DEFAULT 0"},
		{"activity_seq", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if columns[column.name] {
			continue
		}
		if _, err := tx.ExecContext(ctx, "ALTER TABLE task_target_presence ADD COLUMN "+column.name+" "+column.definition); err != nil {
			return fmt.Errorf("add target directory column %s: %w", column.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS presence_source ON task_target_presence(source);
 CREATE INDEX IF NOT EXISTS presence_session ON task_target_presence(session_id,instance_id);
 CREATE TRIGGER IF NOT EXISTS presence_project_deleted AFTER DELETE ON projects BEGIN
   DELETE FROM task_target_presence WHERE project_ident=OLD.ident;
 END;
 INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(25);`); err != nil {
		return fmt.Errorf("finish target directory migration: %w", err)
	}
	return nil
}

func validateTargetRegistration(request TargetRegistration) (TargetRegistration, error) {
	origin, err := agent.NormalizeOrigin(&request.Origin)
	if err != nil {
		return TargetRegistration{}, fmt.Errorf("%w: origin: %v", ErrTargetInput, err)
	}
	request.Origin = *origin
	generation, err := uuid.Parse(request.Generation)
	if err != nil {
		return TargetRegistration{}, fmt.Errorf("%w: generation must be a UUID", ErrTargetInput)
	}
	request.Generation = generation.String()
	for _, value := range []**string{&request.DisplayName, &request.Hostname} {
		if *value == nil {
			continue
		}
		clean := strings.TrimSpace(**value)
		if clean == "" {
			*value = nil
			continue
		}
		if len(clean) > 128 || strings.IndexFunc(clean, unicode.IsControl) >= 0 {
			return TargetRegistration{}, fmt.Errorf("%w: display_name and hostname must be at most 128 UTF-8 bytes with no control characters", ErrTargetInput)
		}
		*value = &clean
	}
	if request.CanonicalRemote != nil {
		remote := strings.TrimSpace(*request.CanonicalRemote)
		if remote == "" {
			request.CanonicalRemote = nil
		} else {
			repo, err := projectid.ParseRemote(remote)
			if err != nil {
				return TargetRegistration{}, ErrTargetBinding
			}
			request.CanonicalRemote = &repo.Canonical
		}
	}
	return request, nil
}

func validateTargetProjectBinding(project *Project, remote *string) (*string, error) {
	if project == nil || project.ArchivedAt != nil {
		return nil, ErrTaskNotFound
	}
	if project.CanonicalRemote == nil {
		if project.Kind == "adhoc" && remote == nil {
			return nil, nil
		}
		return nil, ErrTargetBinding
	}
	if remote == nil {
		return nil, ErrTargetBinding
	}
	expected, expectedErr := projectid.ParseRemote(*project.CanonicalRemote)
	actual, actualErr := projectid.ParseRemote(*remote)
	if expectedErr != nil || actualErr != nil || expected.Canonical != actual.Canonical {
		return nil, ErrTargetBinding
	}
	return &expected.Canonical, nil
}

func (s *Store) RegisterTarget(ctx context.Context, project string, request TargetRegistration, now time.Time) (RegisteredTarget, error) {
	request, err := validateTargetRegistration(request)
	if err != nil {
		return RegisteredTarget{}, err
	}
	project = projectid.NormalizeIdent(project)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RegisteredTarget{}, err
	}
	defer tx.Rollback()
	p, err := getProject(ctx, tx, project)
	if err != nil {
		return RegisteredTarget{}, err
	}
	remote, err := validateTargetProjectBinding(p, request.CanonicalRemote)
	if err != nil {
		return RegisteredTarget{}, err
	}
	originJSON, err := json.Marshal(request.Origin)
	if err != nil {
		return RegisteredTarget{}, err
	}
	seenAt := now.UnixMilli()
	remoteValue := ""
	if remote != nil {
		remoteValue = *remote
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO task_target_presence
 (project_ident,instance_id,session_id,origin_json,generation,seen_at,canonical_remote,display_name,hostname,advertised)
 VALUES(?,?,?,?,?,?,?,?,?,1)
 ON CONFLICT(project_ident,instance_id,session_id) DO UPDATE SET
 origin_json=excluded.origin_json,generation=excluded.generation,seen_at=excluded.seen_at,
 canonical_remote=excluded.canonical_remote,display_name=excluded.display_name,hostname=excluded.hostname,advertised=1`,
		project, request.Origin.InstanceID, request.Origin.SessionID, string(originJSON), request.Generation,
		seenAt, remoteValue, nullableString(request.DisplayName), nullableString(request.Hostname))
	if err != nil {
		return RegisteredTarget{}, err
	}
	if err := tx.Commit(); err != nil {
		return RegisteredTarget{}, err
	}
	return RegisteredTarget{Target: RegisteredTargetIdentity{Origin: request.Origin, Generation: request.Generation}, SeenAt: seenAt,
		ExpiresAt: seenAt + targetRegistrationLease, CanonicalRemote: remote,
		DisplayName: request.DisplayName, Hostname: request.Hostname}, nil
}

// transferTargetRegistration replaces a known predecessor after an exact
// conversation takeover. The existing lease remains bounded by its old expiry.
func transferTargetRegistration(ctx context.Context, tx *sql.Tx, project string, previous, current agent.Origin, originJSON string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_target_presence
		(project_ident,instance_id,session_id,origin_json,generation,seen_at,canonical_remote,display_name,hostname,advertised)
		SELECT project_ident,?,session_id,?,?,seen_at,canonical_remote,display_name,hostname,1
		FROM task_target_presence WHERE project_ident=? AND session_id=? AND instance_id=? AND advertised=1
		ON CONFLICT(project_ident,instance_id,session_id) DO NOTHING`,
		current.InstanceID, originJSON, current.InstanceID, project, previous.SessionID, previous.InstanceID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE task_target_presence SET advertised=0
		WHERE project_ident=? AND session_id=? AND instance_id=?`, project, previous.SessionID, previous.InstanceID)
	return err
}

func (s *Store) Targets(ctx context.Context, project string, now time.Time) ([]RegisteredTarget, error) {
	directory, err := s.TargetDirectory(ctx, project, now)
	if err != nil {
		return nil, err
	}
	return directory.Targets, nil
}

func (s *Store) TargetDirectory(ctx context.Context, project string, now time.Time) (TargetDirectory, error) {
	project = projectid.NormalizeIdent(project)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return TargetDirectory{}, err
	}
	defer tx.Rollback()
	directory, err := readTargetDirectory(ctx, tx, project, now)
	if err != nil {
		return TargetDirectory{}, err
	}
	return directory, tx.Commit()
}

func readTargetDirectory(ctx context.Context, tx *sql.Tx, project string, now time.Time) (TargetDirectory, error) {
	p, err := getProject(ctx, tx, project)
	if err != nil {
		return TargetDirectory{}, err
	}
	if p == nil {
		return TargetDirectory{}, ErrTaskNotFound
	}
	disabled, err := disabledProviders(ctx, tx)
	if err != nil {
		return TargetDirectory{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT origin_json,generation,seen_at,canonical_remote,display_name,hostname
 FROM task_target_presence WHERE project_ident=? AND advertised=1 AND seen_at>=?
 ORDER BY instance_id,session_id`, project, now.UnixMilli()-targetRegistrationLease)
	if err != nil {
		return TargetDirectory{}, err
	}
	targets := []RegisteredTarget{}
	for rows.Next() {
		var originJSON, generation, storedRemote string
		var seenAt int64
		var displayName, hostname *string
		if err := rows.Scan(&originJSON, &generation, &seenAt, &storedRemote, &displayName, &hostname); err != nil {
			_ = rows.Close()
			return TargetDirectory{}, err
		}
		remote := (*string)(nil)
		if storedRemote != "" {
			remote = &storedRemote
		}
		boundRemote, err := validateTargetProjectBinding(p, remote)
		if errors.Is(err, ErrTaskNotFound) || errors.Is(err, ErrTargetBinding) {
			continue
		}
		if err != nil {
			_ = rows.Close()
			return TargetDirectory{}, err
		}
		var origin agent.Origin
		if err := json.Unmarshal([]byte(originJSON), &origin); err != nil {
			_ = rows.Close()
			return TargetDirectory{}, err
		}
		if disabled[origin.Provider] {
			continue
		}
		targets = append(targets, RegisteredTarget{Target: RegisteredTargetIdentity{Origin: origin, Generation: generation},
			SeenAt: seenAt, ExpiresAt: seenAt + targetRegistrationLease, CanonicalRemote: boundRemote,
			DisplayName: displayName, Hostname: hostname})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return TargetDirectory{}, err
	}
	if err := rows.Close(); err != nil {
		return TargetDirectory{}, err
	}
	return TargetDirectory{Targets: targets, CanonicalRemote: p.CanonicalRemote}, nil
}
