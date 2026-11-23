package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nitecon/amux/projects"
)

var ErrProjectMessageUnsupportedChannel = errors.New("agent project messages require an internal project or provider receipt")

// AgentProjectMessageInput contains the post-delivery message fields. A nil
// ExternalMessageID is the provider receipt for an already-sent external
// message. Internal projects may omit it and receive a native receipt.
type AgentProjectMessageInput struct {
	Content           string
	ExternalMessageID *string
	ParentMessageID   *int64
	MessageType       string
	Subject           *string
	Hostname          *string
	EventAt           *int64
}

// CreateAgentProjectMessage stores a successfully sent agent message,
// registers and confirms the sender atomically. External projects require a
// nonblank provider receipt; internal projects create a stable native receipt
// when no external transport ID was supplied.
func (s *Store) CreateAgentProjectMessage(ctx context.Context, project string, origin AgentOrigin, input AgentProjectMessageInput) (*ProjectMessage, error) {
	project = projects.NormalizeIdent(project)
	normalized, err := NormalizeOrigin(&origin)
	if err != nil {
		return nil, fmt.Errorf("invalid agent origin: %w", ErrProjectMessageInput)
	}
	if project == "" {
		return nil, ErrProjectMessageInput
	}
	switch input.MessageType {
	case "message":
		if input.ParentMessageID != nil {
			return nil, ErrProjectMessageInput
		}
	case "reply", "action":
		if input.ParentMessageID == nil {
			return nil, ErrProjectMessageInput
		}
	default:
		return nil, ErrProjectMessageInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	currentProject, err := getProject(ctx, tx, project)
	if err != nil {
		return nil, err
	}
	if currentProject == nil {
		return nil, fmt.Errorf("project %q not found: %w", project, ErrTaskNotFound)
	}
	var parentID any
	if input.ParentMessageID != nil {
		if *input.ParentMessageID <= 0 {
			return nil, ErrProjectMessageNotFound
		}
		parent, err := readProjectMessageByID(ctx, tx, project, *input.ParentMessageID)
		if err != nil {
			return nil, err
		}
		if parent == nil {
			return nil, ErrProjectMessageNotFound
		}
		parentID = *input.ParentMessageID
	}
	if strings.TrimSpace(input.Content) == "" {
		return nil, ErrProjectMessageInput
	}
	if currentProject.ChannelName != "internal" &&
		(input.ExternalMessageID == nil || strings.TrimSpace(*input.ExternalMessageID) == "") {
		return nil, ErrProjectMessageUnsupportedChannel
	}

	now := time.Now().UTC().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO agents(project_ident,agent_id,registered_at) VALUES(?,?,?)`, project, normalized.SessionID, now); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO messages(
		project_ident,source,external_message_id,content,sent_at,confirmed_at,parent_message_id,agent_id,
		message_type,subject,hostname,event_at,deliver_to_agents,author_kind,resolved_at,resolved_by)
		VALUES(?,'agent',?,?,?,?,?,?,?,?,?,?,0,'agent',NULL,NULL)`, project, nullableString(input.ExternalMessageID), input.Content, now, now,
		parentID, normalized.SessionID, input.MessageType, nullableString(input.Subject), nullableString(input.Hostname), input.EventAt)
	if err != nil {
		return nil, fmt.Errorf("insert agent project message: %w", err)
	}
	messageID, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO agent_confirmations(agent_id,project_ident,message_id,confirmed_at) VALUES(?,?,?,?)`,
		normalized.SessionID, project, messageID, now); err != nil {
		return nil, fmt.Errorf("confirm agent project message: %w", err)
	}
	if input.ExternalMessageID == nil {
		receipt := fmt.Sprintf("internal:%d", messageID)
		if _, err := tx.ExecContext(ctx, "UPDATE messages SET external_message_id=? WHERE id=?", receipt, messageID); err != nil {
			return nil, fmt.Errorf("write internal project message receipt: %w", err)
		}
	}
	message, err := readProjectMessageByID(ctx, tx, project, messageID)
	if err != nil {
		return nil, err
	}
	if message == nil {
		return nil, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return message, nil
}
