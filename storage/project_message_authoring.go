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

// ErrProjectMessageInput reports invalid human-authored message content.
var ErrProjectMessageInput = errors.New("invalid project message")

// HumanProjectMessageInput contains only trusted server-derived resolution
// identity. The caller must never fill ResolvedBy from an untrusted request.
type HumanProjectMessageInput struct {
	Content         string
	ParentMessageID *int64
	Resolve         bool
	ResolvedBy      string
}

// CreateHumanProjectMessage stores a browser-authored project message in the
// same durable inbox used by native sessions. Replies attach to the thread root;
// optional root resolution commits atomically with the reply.
func (s *Store) CreateHumanProjectMessage(ctx context.Context, project string, input HumanProjectMessageInput) (*ProjectMessage, error) {
	project = projects.NormalizeIdent(project)
	content := strings.TrimSpace(input.Content)
	if content == "" {
		return nil, ErrProjectMessageInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if p, err := getProject(ctx, tx, project); err != nil {
		return nil, err
	} else if p == nil {
		// Match the storage task mutation convention for a missing project.
		return nil, fmt.Errorf("project %q not found: %w", project, ErrTaskNotFound)
	}

	var root *ProjectMessage
	if input.ParentMessageID != nil && *input.ParentMessageID > 0 {
		parent, err := readProjectMessageByID(ctx, tx, project, *input.ParentMessageID)
		if err != nil {
			return nil, err
		}
		if parent == nil {
			return nil, ErrProjectMessageNotFound
		}
		root = parent
		if parent.ParentMessageID != nil {
			root, err = readProjectMessageByID(ctx, tx, project, *parent.ParentMessageID)
			if err != nil {
				return nil, err
			}
		}
	}

	now := time.Now().UTC().UnixMilli()
	var parentID any
	messageType := "message"
	if root != nil {
		parentID = root.ID
		messageType = "reply"
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO messages(
		project_ident,source,external_message_id,content,sent_at,confirmed_at,parent_message_id,agent_id,
		message_type,subject,hostname,event_at,deliver_to_agents,author_kind,resolved_at,resolved_by)
		VALUES(?,'user',NULL,?,?,NULL,?,NULL,?,NULL,NULL,NULL,1,'human',NULL,NULL)`,
		project, content, now, parentID, messageType)
	if err != nil {
		return nil, fmt.Errorf("insert human project message: %w", err)
	}
	messageID, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	if input.Resolve && root != nil {
		resolvedBy := strings.TrimSpace(input.ResolvedBy)
		if resolvedBy == "" {
			resolvedBy = "user"
		}
		if _, err := updateProjectMessageResolution(ctx, tx, project, root.ID, now, resolvedBy, true); err != nil {
			return nil, fmt.Errorf("resolve human project message thread: %w", err)
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
	_ = s.bus.Publish(eventbus.Event{Topic: eventbus.ProjectChanged})
	return message, nil
}
