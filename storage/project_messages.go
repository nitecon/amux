package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nitecon/amux/projects"
)

// ProjectMessage is the Rust-compatible project inbox message record.
type ProjectMessage struct {
	ID                int64   `json:"id"`
	ProjectIdent      string  `json:"project_ident"`
	Source            string  `json:"source"`
	ExternalMessageID *string `json:"external_message_id"`
	Content           string  `json:"content"`
	SentAt            int64   `json:"sent_at"`
	ConfirmedAt       *int64  `json:"confirmed_at"`
	ParentMessageID   *int64  `json:"parent_message_id"`
	AgentID           *string `json:"agent_id"`
	MessageType       string  `json:"message_type"`
	Subject           *string `json:"subject"`
	Hostname          *string `json:"hostname"`
	EventAt           *int64  `json:"event_at"`
	DeliverToAgents   bool    `json:"deliver_to_agents"`
	AuthorKind        *string `json:"author_kind"`
	ResolvedAt        *int64  `json:"resolved_at"`
	ResolvedBy        *string `json:"resolved_by"`
}

// ErrProjectMessageNotFound means the message is not part of the requested
// project and therefore cannot be acknowledged in that project's inbox.
var ErrProjectMessageNotFound = errors.New("project message not found")

// GetUnconfirmedProjectMessages returns user messages and explicitly
// agent-deliverable messages not yet confirmed by this project agent.
func (s *Store) GetUnconfirmedProjectMessages(ctx context.Context, project, agentID string) ([]ProjectMessage, error) {
	project = projects.NormalizeIdent(project)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if p, err := getProject(ctx, tx, project); err != nil {
		return nil, err
	} else if p == nil {
		return nil, fmt.Errorf("project %q not found", project)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO agents(project_ident,agent_id,registered_at) VALUES(?,?,?)`, project, agentID, time.Now().UnixMilli()); err != nil {
		return nil, fmt.Errorf("register project agent: %w", err)
	}
	messages, err := readUnconfirmedProjectMessages(ctx, tx, project, agentID, -1, nil)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return messages, nil
}

const projectMessageProjection = `m.id,m.project_ident,m.source,m.external_message_id,m.content,m.sent_at,
	m.confirmed_at,m.parent_message_id,m.agent_id,m.message_type,m.subject,m.hostname,m.event_at,
	m.deliver_to_agents,m.author_kind,m.resolved_at,m.resolved_by`

// readUnconfirmedProjectMessages is shared by the public inbox API and native
// prompt delivery so both use the same unread predicate and row projection.
// A negative limit means unbounded (SQLite's LIMIT -1 behavior).
func readUnconfirmedProjectMessages(ctx context.Context, q queryer, project, agentID string, limit int, since *int64) ([]ProjectMessage, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+projectMessageProjection+` FROM messages m
		WHERE m.project_ident=? AND (m.source='user' OR m.deliver_to_agents=1)
		AND (? IS NULL OR COALESCE(m.event_at,m.sent_at)>=?)
		AND NOT EXISTS(SELECT 1 FROM agent_confirmations c
			WHERE c.agent_id=? AND c.project_ident=m.project_ident AND c.message_id=m.id)
		ORDER BY m.id ASC LIMIT ?`, project, since, since, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("query unconfirmed project messages: %w", err)
	}
	messages := make([]ProjectMessage, 0)
	for rows.Next() {
		message, err := scanProjectMessage(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		messages = append(messages, message)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil {
		return nil, fmt.Errorf("read unconfirmed project messages: %w", rowsErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return messages, nil
}

// ConfirmProjectMessage records an idempotent acknowledgement scoped to the
// exact project and agent. A message ID from another project is rejected so a
// malformed caller cannot create a cross-project confirmation row. This is a
// deliberate native integrity boundary: Rust's current helper stores the
// caller-supplied project tuple without checking the message's project.
func (s *Store) ConfirmProjectMessage(ctx context.Context, project, agentID string, messageID int64) (bool, error) {
	project = projects.NormalizeIdent(project)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if p, err := getProject(ctx, tx, project); err != nil {
		return false, err
	} else if p == nil {
		return false, fmt.Errorf("project %q not found", project)
	}
	var belongs bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM messages WHERE id=? AND project_ident=?)", messageID, project).Scan(&belongs); err != nil {
		return false, err
	}
	if !belongs {
		return false, ErrProjectMessageNotFound
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO agent_confirmations(agent_id,project_ident,message_id,confirmed_at) VALUES(?,?,?,?)`, agentID, project, messageID, time.Now().UnixMilli())
	if err != nil {
		return false, fmt.Errorf("confirm project message: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return inserted != 0, nil
}

func scanProjectMessage(row interface{ Scan(...any) error }) (ProjectMessage, error) {
	var message ProjectMessage
	var externalID, agentID, subject, hostname, authorKind, resolvedBy sql.NullString
	var confirmedAt, parentID, eventAt, resolvedAt sql.NullInt64
	if err := row.Scan(&message.ID, &message.ProjectIdent, &message.Source, &externalID, &message.Content, &message.SentAt,
		&confirmedAt, &parentID, &agentID, &message.MessageType, &subject, &hostname, &eventAt,
		&message.DeliverToAgents, &authorKind, &resolvedAt, &resolvedBy); err != nil {
		return ProjectMessage{}, err
	}
	message.ExternalMessageID = optionalSQLString(externalID)
	message.ConfirmedAt = optionalSQLInt64(confirmedAt)
	message.ParentMessageID = optionalSQLInt64(parentID)
	message.AgentID = optionalSQLString(agentID)
	message.Subject = optionalSQLString(subject)
	message.Hostname = optionalSQLString(hostname)
	message.EventAt = optionalSQLInt64(eventAt)
	message.AuthorKind = optionalSQLString(authorKind)
	message.ResolvedAt = optionalSQLInt64(resolvedAt)
	message.ResolvedBy = optionalSQLString(resolvedBy)
	return message, nil
}

func optionalSQLInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

const projectMessagesMigration = 35

// migrateProjectMessages creates the Rust-compatible agent registry table.
// IF NOT EXISTS preserves any pre-existing Rust table definition verbatim.
func migrateProjectMessages(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS agents (
		project_ident TEXT NOT NULL REFERENCES projects(ident),
		agent_id TEXT NOT NULL,
		registered_at INTEGER NOT NULL,
		PRIMARY KEY (project_ident, agent_id)
	)`); err != nil {
		return fmt.Errorf("create project agents table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)", projectMessagesMigration); err != nil {
		return fmt.Errorf("record project messages migration: %w", err)
	}
	return nil
}
