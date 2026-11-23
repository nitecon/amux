package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/nitecon/amux/projects"
)

// ProjectMessageThreadFilter selects a page of project inbox threads.
type ProjectMessageThreadFilter struct {
	State   string
	Kinds   []string
	AgentID *string
	Limit   uint64
	Offset  uint64
}

// ProjectMessageThreadSummary is one root message with list-specific derived
// state and activity fields.
type ProjectMessageThreadSummary struct {
	ID             int64   `json:"id"`
	ProjectIdent   string  `json:"project_ident"`
	Source         string  `json:"source"`
	AuthorKind     string  `json:"author_kind"`
	AgentID        *string `json:"agent_id"`
	Hostname       *string `json:"hostname"`
	Subject        *string `json:"subject"`
	Preview        string  `json:"preview"`
	SentAt         int64   `json:"sent_at"`
	LastActivityAt int64   `json:"last_activity_at"`
	ReplyCount     int64   `json:"reply_count"`
	AckCount       int64   `json:"ack_count"`
	State          string  `json:"state"`
	ResolvedAt     *int64  `json:"resolved_at"`
	ResolvedBy     *string `json:"resolved_by"`
}

// ProjectMessageInboxCounts counts root messages independently of list
// filters and pagination.
type ProjectMessageInboxCounts struct {
	HumanOpen  int64 `json:"human_open"`
	AgentOpen  int64 `json:"agent_open"`
	AlertsOpen int64 `json:"alerts_open"`
	SystemOpen int64 `json:"system_open"`
	Resolved   int64 `json:"resolved"`
}

// ProjectMessageThreadList is the source-compatible response for the project
// message thread list endpoint.
type ProjectMessageThreadList struct {
	Threads []ProjectMessageThreadSummary `json:"threads"`
	Counts  ProjectMessageInboxCounts     `json:"counts"`
}

const (
	threadAuthorKindSQL = `COALESCE(m.author_kind, CASE m.source WHEN 'user' THEN 'human' ELSE m.source END)`
	threadStateSQL      = `CASE
		WHEN m.resolved_at IS NOT NULL THEN 'resolved'
		WHEN EXISTS (SELECT 1 FROM messages c WHERE c.project_ident=m.project_ident AND c.parent_message_id=m.id) THEN 'answered'
		WHEN m.source='user' AND (m.confirmed_at IS NOT NULL OR EXISTS (
			SELECT 1 FROM agent_confirmations ac WHERE ac.project_ident=m.project_ident AND ac.message_id=m.id
		)) THEN 'acknowledged'
		ELSE 'open' END`
)

// ListProjectMessageThreads returns an ordered page and project-wide inbox
// counts from one transaction snapshot. A zero limit uses the source database
// helper's default of 100; the HTTP route may apply its narrower default.
func (s *Store) ListProjectMessageThreads(ctx context.Context, project string, filter ProjectMessageThreadFilter) (*ProjectMessageThreadList, error) {
	project = projects.NormalizeIdent(project)
	if project == "" {
		return nil, fmt.Errorf("project %q not found: %w", project, ErrTaskNotFound)
	}
	if filter.Limit == 0 {
		filter.Limit = 100
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if p, err := getProject(ctx, tx, project); err != nil {
		return nil, err
	} else if p == nil {
		return nil, fmt.Errorf("project %q not found: %w", project, ErrTaskNotFound)
	}

	var conditions []string
	args := []any{project, filter.AgentID, filter.AgentID}
	switch filter.State {
	case "open", "acknowledged", "answered", "resolved":
		conditions = append(conditions, "state=?")
		args = append(args, filter.State)
	case "all":
	default:
		conditions = append(conditions, "state!='resolved'")
	}
	kinds := threadKindSQLValues(filter.Kinds)
	if len(kinds) != 0 {
		placeholders := make([]string, len(kinds))
		for i, kind := range kinds {
			placeholders[i] = "?"
			args = append(args, kind)
		}
		conditions = append(conditions, "author_kind IN ("+strings.Join(placeholders, ",")+")")
	}
	args = append(args, int64(filter.Limit), int64(filter.Offset))
	query := `SELECT id,project_ident,source,author_kind,agent_id,hostname,subject,preview,sent_at,
		last_activity_at,reply_count,ack_count,state,resolved_at,resolved_by FROM (
		SELECT m.id,m.project_ident,m.source,` + threadAuthorKindSQL + ` AS author_kind,
		m.agent_id,m.hostname,m.subject,substr(m.content,1,200) AS preview,m.sent_at,
		COALESCE((SELECT MAX(c.sent_at) FROM messages c WHERE c.project_ident=m.project_ident AND c.parent_message_id=m.id),m.sent_at) AS last_activity_at,
		(SELECT COUNT(*) FROM messages c WHERE c.project_ident=m.project_ident AND c.parent_message_id=m.id) AS reply_count,
		(SELECT COUNT(*) FROM agent_confirmations ac WHERE ac.project_ident=m.project_ident AND ac.message_id=m.id) AS ack_count,
		` + threadStateSQL + ` AS state,m.resolved_at,m.resolved_by
		FROM messages m WHERE m.project_ident=? AND m.parent_message_id IS NULL
		AND (? IS NULL OR m.agent_id=?)
	) t`
	if len(conditions) != 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY last_activity_at DESC LIMIT ? OFFSET ?"
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	result := &ProjectMessageThreadList{Threads: make([]ProjectMessageThreadSummary, 0)}
	for rows.Next() {
		var item ProjectMessageThreadSummary
		var agentID, hostname, subject, resolvedBy sql.NullString
		var resolvedAt sql.NullInt64
		if err := rows.Scan(&item.ID, &item.ProjectIdent, &item.Source, &item.AuthorKind, &agentID, &hostname, &subject,
			&item.Preview, &item.SentAt, &item.LastActivityAt, &item.ReplyCount, &item.AckCount, &item.State, &resolvedAt, &resolvedBy); err != nil {
			rows.Close()
			return nil, err
		}
		item.AgentID = optionalSQLString(agentID)
		item.Hostname = optionalSQLString(hostname)
		item.Subject = optionalSQLString(subject)
		item.ResolvedAt = optionalSQLInt64(resolvedAt)
		item.ResolvedBy = optionalSQLString(resolvedBy)
		result.Threads = append(result.Threads, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN m.resolved_at IS NULL AND `+threadAuthorKindSQL+`='human' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN m.resolved_at IS NULL AND `+threadAuthorKindSQL+`='agent' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN m.resolved_at IS NULL AND `+threadAuthorKindSQL+` IN ('bot','webhook') THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN m.resolved_at IS NULL AND `+threadAuthorKindSQL+`='system' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN m.resolved_at IS NOT NULL THEN 1 ELSE 0 END),0)
		FROM messages m WHERE m.project_ident=? AND m.parent_message_id IS NULL`, project).Scan(
		&result.Counts.HumanOpen, &result.Counts.AgentOpen, &result.Counts.AlertsOpen, &result.Counts.SystemOpen, &result.Counts.Resolved); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// ResolveProjectMessagesBefore resolves every matching open root sent strictly
// before beforeMS. An empty kinds slice selects every effective author kind.
func (s *Store) ResolveProjectMessagesBefore(ctx context.Context, project string, beforeMS int64, kinds []string, resolvedBy string) (int64, error) {
	project = projects.NormalizeIdent(project)
	query := `UPDATE messages SET resolved_at=?,resolved_by=? WHERE id IN (
		SELECT m.id FROM messages m WHERE m.project_ident=? AND m.resolved_at IS NULL
		AND m.parent_message_id IS NULL AND m.sent_at<?`
	args := []any{time.Now().UTC().UnixMilli(), resolvedBy, project, beforeMS}
	kinds = threadKindSQLValues(kinds)
	if len(kinds) != 0 {
		placeholders := make([]string, len(kinds))
		for i, kind := range kinds {
			placeholders[i] = "?"
			args = append(args, kind)
		}
		query += " AND " + threadAuthorKindSQL + " IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += ")"
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func threadKindSQLValues(input []string) []string {
	kinds := make([]string, len(input))
	for i, value := range input {
		kinds[i] = strings.ReplaceAll(value, "'", "")
	}
	return kinds
}
