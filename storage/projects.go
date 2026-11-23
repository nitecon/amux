package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/projects"
)

// Project preserves the Rust gateway's project JSON and nullable metadata.
type Project struct {
	Ident           string  `json:"ident"`
	ChannelName     string  `json:"channel_name"`
	RoomID          string  `json:"room_id"`
	LastMsgID       *string `json:"last_msg_id"`
	CreatedAt       int64   `json:"created_at"`
	RepoProvider    *string `json:"repo_provider"`
	RepoNamespace   *string `json:"repo_namespace"`
	RepoName        *string `json:"repo_name"`
	RepoFullName    *string `json:"repo_full_name"`
	Kind            string  `json:"kind"`
	ArchivedAt      *int64  `json:"archived_at"`
	CanonicalRemote *string `json:"canonical_remote"`
}

const projectColumns = "ident,channel_name,room_id,last_msg_id,created_at,repo_provider,repo_namespace,repo_name,repo_full_name,COALESCE(kind,'repo'),archived_at,canonical_remote"

type scanner interface{ Scan(...any) error }

func scanProject(row scanner) (*Project, error) {
	var p Project
	err := row.Scan(&p.Ident, &p.ChannelName, &p.RoomID, &p.LastMsgID, &p.CreatedAt, &p.RepoProvider, &p.RepoNamespace, &p.RepoName, &p.RepoFullName, &p.Kind, &p.ArchivedAt, &p.CanonicalRemote)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}
func getProject(ctx context.Context, q queryer, ident string) (*Project, error) {
	return scanProject(q.QueryRowContext(ctx, "SELECT "+projectColumns+" FROM projects WHERE ident=?", projects.NormalizeIdent(ident)))
}
func (s *Store) Project(ctx context.Context, ident string) (*Project, error) {
	return getProject(ctx, s.db, ident)
}
func readProjects(ctx context.Context, q queryer, hideGlobal bool) ([]Project, error) {
	statement := "SELECT " + projectColumns + " FROM projects"
	if hideGlobal {
		statement += " WHERE ident!='__global__'"
	}
	rows, err := q.QueryContext(ctx, statement)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *p)
	}
	return result, rows.Err()
}
func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	return readProjects(ctx, s.db, true)
}

// AllProjects includes the reserved global project for server transport setup.
func (s *Store) AllProjects(ctx context.Context) ([]Project, error) {
	return readProjects(ctx, s.db, false)
}
func normalizeOptional(p *string) *string {
	if p == nil {
		return nil
	}
	s := projects.NormalizeIdent(*p)
	if s == "" {
		return nil
	}
	return &s
}
func ptr(s string) *string { return &s }

func mappingAvailable(ctx context.Context, q queryer, ident string, provider, fullName *string) error {
	if fullName == nil {
		return nil
	}
	p := ""
	if provider != nil {
		p = *provider
	}
	var conflict string
	err := q.QueryRowContext(ctx, "SELECT ident FROM projects WHERE ident!=? AND COALESCE(repo_provider,'')=? AND repo_full_name=? LIMIT 1", ident, p, *fullName).Scan(&conflict)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("repository already mapped to project %q", conflict)
}

// InsertProject atomically inserts its normalized identity and initial cursor.
// Re-registering an existing identity does not overwrite its stored data.
func (s *Store) InsertProject(ctx context.Context, p Project) error {
	p.Ident = projects.NormalizeIdent(p.Ident)
	p.RepoProvider = normalizeOptional(p.RepoProvider)
	p.RepoNamespace = normalizeOptional(p.RepoNamespace)
	p.RepoName = normalizeOptional(p.RepoName)
	p.RepoFullName = normalizeOptional(p.RepoFullName)
	if p.RepoFullName == nil && p.RepoNamespace != nil && p.RepoName != nil {
		p.RepoFullName = ptr(*p.RepoNamespace + "/" + *p.RepoName)
	}
	if p.Kind == "" {
		p.Kind = "repo"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := mappingAvailable(ctx, tx, p.Ident, p.RepoProvider, p.RepoFullName); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO projects(ident,channel_name,room_id,last_msg_id,created_at,repo_provider,repo_namespace,repo_name,repo_full_name,kind,archived_at,canonical_remote) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(ident) DO NOTHING`, p.Ident, p.ChannelName, p.RoomID, p.LastMsgID, p.CreatedAt, p.RepoProvider, p.RepoNamespace, p.RepoName, p.RepoFullName, p.Kind, p.ArchivedAt, p.CanonicalRemote); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO cursors(project_ident,last_read_id,updated_at) VALUES(?,0,?) ON CONFLICT(project_ident) DO NOTHING", p.Ident, p.CreatedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_ = s.bus.Publish(eventbus.Event{Topic: eventbus.ProjectChanged})
	return nil
}

func (s *Store) UpdateRepoMapping(ctx context.Context, ident, provider, namespace, name string) (*Project, error) {
	ident = projects.NormalizeIdent(ident)
	provider = projects.NormalizeIdent(provider)
	namespace = projects.NormalizeIdent(namespace)
	name = projects.NormalizeIdent(name)
	if (namespace == "") != (name == "") {
		return nil, fmt.Errorf("namespace and repo_name must be supplied together")
	}
	var p, ns, n, full *string
	kind := "adhoc"
	if namespace != "" {
		if provider == "" {
			provider = "github"
		}
		p = ptr(provider)
		ns = ptr(namespace)
		n = ptr(name)
		full = ptr(namespace + "/" + name)
		kind = "repo"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := mappingAvailable(ctx, tx, ident, p, full); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE projects SET repo_provider=?,repo_namespace=?,repo_name=?,repo_full_name=?,kind=? WHERE ident=?", p, ns, n, full, kind, ident); err != nil {
		return nil, err
	}
	result, err := getProject(ctx, tx, ident)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	_ = s.bus.Publish(eventbus.Event{Topic: eventbus.ProjectChanged})
	return result, nil
}

// BackfillRepo fills absent metadata, without changing a prior repository mapping.
func (s *Store) BackfillRepo(ctx context.Context, ident string, repo projects.RepoRef) (bool, error) {
	ident = projects.NormalizeIdent(ident)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	p, err := getProject(ctx, tx, ident)
	if err != nil || p == nil {
		return false, err
	}
	changed := false
	if p.CanonicalRemote == nil {
		p.CanonicalRemote = ptr(repo.Canonical)
		changed = true
	}
	full := repo.FullName()
	if p.RepoFullName == nil && mappingAvailable(ctx, tx, ident, &repo.Provider, &full) == nil {
		p.RepoProvider = ptr(repo.Provider)
		p.RepoNamespace = ptr(repo.Namespace)
		p.RepoName = ptr(repo.Name)
		p.RepoFullName = ptr(full)
		p.Kind = "repo"
		changed = true
	} else if p.Kind != "repo" && p.RepoFullName != nil {
		p.Kind = "repo"
		changed = true
	}
	if !changed {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, "UPDATE projects SET canonical_remote=?,repo_provider=?,repo_namespace=?,repo_name=?,repo_full_name=?,kind=? WHERE ident=?", p.CanonicalRemote, p.RepoProvider, p.RepoNamespace, p.RepoName, p.RepoFullName, p.Kind, ident); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	_ = s.bus.Publish(eventbus.Event{Topic: eventbus.ProjectChanged})
	return true, nil
}

func (s *Store) SetProjectArchived(ctx context.Context, ident string, archived bool) (*Project, error) {
	var at *int64
	if archived {
		now := time.Now().UnixMilli()
		at = &now
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE projects SET archived_at=? WHERE ident=?", at, strings.ToLower(strings.TrimSpace(ident))); err != nil {
		return nil, err
	}
	p, err := getProject(ctx, tx, ident)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	_ = s.bus.Publish(eventbus.Event{Topic: eventbus.ProjectChanged})
	return p, nil
}
