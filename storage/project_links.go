package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	projectid "github.com/nitecon/amux/identity/project"
)

const projectLinksMigration = 42

var ErrProjectLinkProjectNotFound = errors.New("project not found")
var ErrInvalidProjectLink = errors.New("invalid project link")

// ProjectLink is a user-managed shortcut attached to one project.
type ProjectLink struct {
	ID           int64  `json:"id"`
	ProjectIdent string `json:"project_ident"`
	Label        string `json:"label"`
	URL          string `json:"url"`
	Rank         int64  `json:"rank"`
}

// migrateProjectLinks preserves the Rust table when present and creates it for
// databases that have not yet used project shortcuts.
func migrateProjectLinks(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS project_links (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 project_ident TEXT NOT NULL REFERENCES projects(ident) ON DELETE CASCADE,
 label TEXT NOT NULL,
 url TEXT NOT NULL,
 rank INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL
);
	CREATE INDEX IF NOT EXISTS idx_project_links_project ON project_links(project_ident, rank);`); err != nil {
		return fmt.Errorf("migrate project links: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)`, projectLinksMigration); err != nil {
		return fmt.Errorf("record project links migration: %w", err)
	}
	return nil
}

func (s *Store) ProjectLinks(ctx context.Context, projectIdent string) ([]ProjectLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,project_ident,label,url,rank FROM project_links WHERE project_ident=? ORDER BY rank ASC,id ASC`, projectid.NormalizeIdent(projectIdent))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []ProjectLink{}
	for rows.Next() {
		var link ProjectLink
		if err := rows.Scan(&link.ID, &link.ProjectIdent, &link.Label, &link.URL, &link.Rank); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

func (s *Store) AddProjectLink(ctx context.Context, projectIdent, label, rawURL string) (*ProjectLink, error) {
	projectIdent = projectid.NormalizeIdent(projectIdent)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE ident=?`, projectIdent).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProjectLinkProjectNotFound
	}
	if err != nil {
		return nil, err
	}
	label, rawURL = strings.TrimSpace(label), strings.TrimSpace(rawURL)
	if label == "" || rawURL == "" {
		return nil, fmt.Errorf("%w: label and url are required", ErrInvalidProjectLink)
	}
	if !strings.HasPrefix(rawURL, "https://") && !strings.HasPrefix(rawURL, "http://") {
		return nil, fmt.Errorf("%w: url must start with http:// or https://", ErrInvalidProjectLink)
	}
	var rank int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(rank),0)+1 FROM project_links WHERE project_ident=?`, projectIdent).Scan(&rank); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO project_links(project_ident,label,url,rank,created_at) VALUES(?,?,?,?,?)`, projectIdent, label, rawURL, rank, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ProjectLink{ID: id, ProjectIdent: projectIdent, Label: label, URL: rawURL, Rank: rank}, nil
}

func (s *Store) DeleteProjectLink(ctx context.Context, projectIdent string, id int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM project_links WHERE project_ident=? AND id=?`, projectid.NormalizeIdent(projectIdent), id)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}
