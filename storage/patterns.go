package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const patternRepositoryMigration = 43

type Pattern struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Slug         string   `json:"slug"`
	Summary      *string  `json:"summary"`
	Body         string   `json:"body"`
	Labels       []string `json:"labels"`
	Version      string   `json:"version"`
	State        string   `json:"state"`
	SupersededBy *string  `json:"superseded_by"`
	Author       string   `json:"author"`
	CreatedAt    int64    `json:"created_at"`
	UpdatedAt    int64    `json:"updated_at"`
}

type PatternSummary struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Slug         string   `json:"slug"`
	Summary      *string  `json:"summary"`
	Labels       []string `json:"labels"`
	Version      string   `json:"version"`
	State        string   `json:"state"`
	SupersededBy *string  `json:"superseded_by"`
	Author       string   `json:"author"`
	CommentCount int64    `json:"comment_count"`
	CreatedAt    int64    `json:"created_at"`
	UpdatedAt    int64    `json:"updated_at"`
}

type PatternComment struct {
	ID         string `json:"id"`
	PatternID  string `json:"pattern_id"`
	Author     string `json:"author"`
	AuthorType string `json:"author_type"`
	Content    string `json:"content"`
	CreatedAt  int64  `json:"created_at"`
}

type PatternInsert struct {
	Title        string
	Slug         *string
	Summary      *string
	Body         string
	Labels       []string
	Version      string
	State        string
	SupersededBy *string
	Author       string
}

// PatternUpdate uses pointer-to-pointer fields where null clears the stored value.
type PatternUpdate struct {
	Title        *string
	Slug         *string
	Summary      **string
	Body         *string
	Labels       *[]string
	Version      *string
	State        *string
	SupersededBy **string
}

type PatternFilters struct {
	Query        *string
	Label        *string
	Version      *string
	State        *string
	SupersededBy *string
}

const patternSelectColumns = "id,title,slug,summary,body,labels,version,state,superseded_by,author,created_at,updated_at"

func migratePatterns(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS patterns (
 id TEXT PRIMARY KEY,title TEXT NOT NULL,slug TEXT NOT NULL UNIQUE,summary TEXT,body TEXT NOT NULL,labels TEXT,
 version TEXT NOT NULL DEFAULT 'draft' CHECK(version IN ('draft','latest','superseded')),
 state TEXT NOT NULL DEFAULT 'active' CHECK(state IN ('active','archived')),superseded_by TEXT,
 author TEXT NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	for _, column := range []struct{ name, ddl string }{
		{"version", "TEXT NOT NULL DEFAULT 'draft'"},
		{"state", "TEXT NOT NULL DEFAULT 'active'"},
		{"superseded_by", "TEXT"},
	} {
		present, err := sqliteTableHasColumn(ctx, tx, "patterns", column.name)
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE patterns ADD COLUMN "+column.name+" "+column.ddl); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_patterns_updated ON patterns(updated_at DESC);
CREATE TABLE IF NOT EXISTS pattern_comments (
 id TEXT PRIMARY KEY,pattern_id TEXT NOT NULL REFERENCES patterns(id) ON DELETE CASCADE,
 author TEXT NOT NULL,author_type TEXT NOT NULL CHECK(author_type IN ('agent','user','system')),
 content TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_pattern_comments_pattern ON pattern_comments(pattern_id,created_at)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE patterns
 SET superseded_by=substr(state,length('superseded-by:')+1),state='active'
 WHERE superseded_by IS NULL AND state LIKE 'superseded-by:%'`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)`, patternRepositoryMigration)
	return err
}

func scanPattern(row interface{ Scan(...any) error }) (*Pattern, error) {
	var item Pattern
	var labels sql.NullString
	if err := row.Scan(&item.ID, &item.Title, &item.Slug, &item.Summary, &item.Body, &labels, &item.Version, &item.State, &item.SupersededBy, &item.Author, &item.CreatedAt, &item.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	item.Labels = parsePatternLabels(labels)
	return &item, nil
}

func parsePatternLabels(raw sql.NullString) []string {
	var labels []string
	if raw.Valid && json.Unmarshal([]byte(raw.String), &labels) == nil && labels != nil {
		return labels
	}
	return []string{}
}

func (s *Store) GetPattern(ctx context.Context, idOrSlug string) (*Pattern, error) {
	return scanPattern(s.db.QueryRowContext(ctx, "SELECT "+patternSelectColumns+" FROM patterns WHERE id=? OR slug=?", idOrSlug, idOrSlug))
}

func (s *Store) InsertPattern(ctx context.Context, input PatternInsert) (*Pattern, error) {
	if err := validatePatternVersion(input.Version); err != nil {
		return nil, err
	}
	input.State = strings.TrimSpace(input.State)
	if err := validatePatternState(input.State); err != nil {
		return nil, err
	}
	input.SupersededBy = cleanOptionalString(input.SupersededBy)
	if err := validatePatternSupersession(input.Version, input.SupersededBy); err != nil {
		return nil, err
	}
	slug := ""
	if input.Slug != nil {
		slug = strings.TrimSpace(*input.Slug)
	}
	if slug == "" {
		slug = slugifyPatternTitle(input.Title)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	labels, err := encodePatternLabels(input.Labels)
	if err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO patterns(id,title,slug,summary,body,labels,version,state,superseded_by,author,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id.String(), input.Title, slug, input.Summary, input.Body, labels, input.Version, input.State, input.SupersededBy, input.Author, now, now)
	if err != nil {
		return nil, err
	}
	return s.GetPattern(ctx, id.String())
}

func (s *Store) UpdatePattern(ctx context.Context, idOrSlug string, update PatternUpdate) (*Pattern, error) {
	current, err := s.GetPattern(ctx, idOrSlug)
	if err != nil || current == nil {
		return current, err
	}
	version := current.Version
	if update.Version != nil {
		version = *update.Version
		if err := validatePatternVersion(version); err != nil {
			return nil, err
		}
	}
	state := current.State
	if update.State != nil {
		state = strings.TrimSpace(*update.State)
		if err := validatePatternState(state); err != nil {
			return nil, err
		}
	}
	supersededBy := current.SupersededBy
	if update.SupersededBy != nil {
		supersededBy = cleanOptionalString(*update.SupersededBy)
	}
	if version == "superseded" && supersededBy == nil {
		return nil, fmt.Errorf("superseded_by is required when version is superseded")
	}
	if version != "superseded" && update.SupersededBy != nil && supersededBy != nil {
		return nil, fmt.Errorf("superseded_by can only be set when version is superseded")
	}
	assignments := []string{}
	args := []any{}
	add := func(column string, value any) {
		assignments = append(assignments, column+"=?")
		args = append(args, value)
	}
	if update.Title != nil {
		add("title", *update.Title)
	}
	if update.Slug != nil {
		add("slug", *update.Slug)
	}
	if update.Summary != nil {
		add("summary", *update.Summary)
	}
	if update.Body != nil {
		add("body", *update.Body)
	}
	if update.Labels != nil {
		labels, err := encodePatternLabels(*update.Labels)
		if err != nil {
			return nil, err
		}
		add("labels", labels)
	}
	if update.Version != nil {
		add("version", version)
	}
	if update.State != nil {
		add("state", state)
	}
	if update.Version != nil && version != "superseded" && current.SupersededBy != nil && update.SupersededBy == nil {
		add("superseded_by", nil)
	} else if update.SupersededBy != nil {
		add("superseded_by", supersededBy)
	}
	add("updated_at", time.Now().UnixMilli())
	args = append(args, current.ID)
	if _, err := s.db.ExecContext(ctx, "UPDATE patterns SET "+strings.Join(assignments, ",")+" WHERE id=?", args...); err != nil {
		return nil, err
	}
	return s.GetPattern(ctx, current.ID)
}

func (s *Store) DeletePattern(ctx context.Context, idOrSlug string) (bool, error) {
	pattern, err := s.GetPattern(ctx, idOrSlug)
	if err != nil || pattern == nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, "DELETE FROM patterns WHERE id=?", pattern.ID)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

func (s *Store) ListPatterns(ctx context.Context, filters PatternFilters) ([]PatternSummary, error) {
	sqlText := `SELECT p.id,p.title,p.slug,p.summary,p.labels,p.version,p.state,p.superseded_by,p.author,
(SELECT COUNT(*) FROM pattern_comments pc WHERE pc.pattern_id=p.id),p.created_at,p.updated_at FROM patterns p`
	var clauses []string
	var args []any
	add := func(value any) int { args = append(args, value); return len(args) }
	if filters.Query != nil && strings.TrimSpace(*filters.Query) != "" {
		query := "%" + strings.TrimSpace(*filters.Query) + "%"
		n := add(query)
		clauses = append(clauses, fmt.Sprintf(`(p.title LIKE ?%d OR p.slug LIKE ?%d OR COALESCE(p.summary,'') LIKE ?%d OR p.body LIKE ?%d OR COALESCE(p.labels,'') LIKE ?%d OR p.version LIKE ?%d OR p.state LIKE ?%d OR COALESCE(p.superseded_by,'') LIKE ?%d)`, n, n, n, n, n, n, n, n))
	}
	if filters.Label != nil && strings.TrimSpace(*filters.Label) != "" {
		clauses = append(clauses, fmt.Sprintf("COALESCE(p.labels,'') LIKE ?%d", add("%\""+strings.TrimSpace(*filters.Label)+"\"%")))
	}
	if filters.Version != nil && strings.TrimSpace(*filters.Version) != "" {
		value := strings.TrimSpace(*filters.Version)
		if err := validatePatternVersion(value); err != nil {
			return nil, err
		}
		clauses = append(clauses, fmt.Sprintf("p.version=?%d", add(value)))
	}
	if filters.State != nil && strings.TrimSpace(*filters.State) != "" {
		value := strings.TrimSpace(*filters.State)
		if err := validatePatternState(value); err != nil {
			return nil, err
		}
		clauses = append(clauses, fmt.Sprintf("p.state=?%d", add(value)))
	}
	if filters.SupersededBy != nil && strings.TrimSpace(*filters.SupersededBy) != "" {
		value := strings.TrimSpace(*filters.SupersededBy)
		n := add(value)
		clauses = append(clauses, fmt.Sprintf("(p.superseded_by=?%d OR p.superseded_by IN (SELECT id FROM patterns WHERE slug=?%d))", n, n))
	}
	if len(clauses) > 0 {
		sqlText += " WHERE " + strings.Join(clauses, " AND ")
	}
	sqlText += " ORDER BY p.updated_at DESC,p.title ASC"
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []PatternSummary{}
	for rows.Next() {
		var item PatternSummary
		var labels sql.NullString
		if err := rows.Scan(&item.ID, &item.Title, &item.Slug, &item.Summary, &labels, &item.Version, &item.State, &item.SupersededBy, &item.Author, &item.CommentCount, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.Labels = parsePatternLabels(labels)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ListPatternComments(ctx context.Context, idOrSlug string) ([]PatternComment, bool, error) {
	pattern, err := s.GetPattern(ctx, idOrSlug)
	if err != nil || pattern == nil {
		return nil, pattern != nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,pattern_id,author,author_type,content,created_at FROM pattern_comments WHERE pattern_id=? ORDER BY created_at`, pattern.ID)
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	items := []PatternComment{}
	for rows.Next() {
		var item PatternComment
		if err := rows.Scan(&item.ID, &item.PatternID, &item.Author, &item.AuthorType, &item.Content, &item.CreatedAt); err != nil {
			return nil, true, err
		}
		items = append(items, item)
	}
	return items, true, rows.Err()
}

func (s *Store) InsertPatternComment(ctx context.Context, idOrSlug, author, authorType, content string) (*PatternComment, error) {
	if authorType != "agent" && authorType != "user" && authorType != "system" {
		return nil, fmt.Errorf("invalid author_type '%s': must be agent|user|system", authorType)
	}
	pattern, err := s.GetPattern(ctx, idOrSlug)
	if err != nil || pattern == nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO pattern_comments(id,pattern_id,author,author_type,content,created_at) VALUES(?,?,?,?,?,?)`, id.String(), pattern.ID, author, authorType, content, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE patterns SET updated_at=? WHERE id=?`, now, pattern.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &PatternComment{ID: id.String(), PatternID: pattern.ID, Author: author, AuthorType: authorType, Content: content, CreatedAt: now}, nil
}

func validatePatternVersion(value string) error {
	if !IsPatternVersion(value) {
		return fmt.Errorf("invalid pattern version '%s': must be draft|latest|superseded", value)
	}
	return nil
}

func validatePatternState(value string) error {
	if !IsPatternState(value) {
		return fmt.Errorf("invalid pattern state '%s': must be active|archived", value)
	}
	return nil
}

func IsPatternVersion(value string) bool {
	return value == "draft" || value == "latest" || value == "superseded"
}

func IsPatternState(value string) bool {
	return value == "active" || value == "archived"
}

func validatePatternSupersession(version string, target *string) error {
	if version == "superseded" && target == nil {
		return fmt.Errorf("superseded_by is required when version is superseded")
	}
	if version != "superseded" && target != nil {
		return fmt.Errorf("superseded_by can only be set when version is superseded")
	}
	return nil
}

func slugifyPatternTitle(title string) string {
	var out strings.Builder
	previousDash := false
	for _, char := range strings.ToLower(title) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			out.WriteRune(char)
			previousDash = false
		} else if !previousDash && out.Len() > 0 {
			out.WriteByte('-')
			previousDash = true
		}
	}
	slug := strings.Trim(out.String(), "-")
	if slug == "" {
		return "pattern"
	}
	return slug
}

func encodePatternLabels(labels []string) (any, error) {
	if len(labels) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(labels)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

func cleanOptionalString(value *string) *string {
	if value == nil {
		return nil
	}
	cleaned := strings.TrimSpace(*value)
	if cleaned == "" {
		return nil
	}
	return &cleaned
}
