package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/nitecon/amux/projects"
)

// ProjectMessageConfirmation preserves Rust's JSON tuple wire shape:
// [agent_id, confirmed_at].
type ProjectMessageConfirmation struct {
	AgentID     string
	ConfirmedAt int64
}

func (c ProjectMessageConfirmation) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]any{c.AgentID, c.ConfirmedAt})
}

func (c *ProjectMessageConfirmation) UnmarshalJSON(raw []byte) error {
	var tuple []json.RawMessage
	if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) != 2 {
		return fmt.Errorf("project message confirmation must be a two-item tuple")
	}
	if len(tuple[0]) == 0 || tuple[0][0] != '"' || string(tuple[0]) == "null" {
		return fmt.Errorf("project message confirmation agent ID must be a string")
	}
	var agentID string
	if err := json.Unmarshal(tuple[0], &agentID); err != nil {
		return fmt.Errorf("project message confirmation agent ID must be a string")
	}
	if len(tuple[1]) == 0 || tuple[1][0] == '"' || string(tuple[1]) == "null" {
		return fmt.Errorf("project message confirmation timestamp must be an integer")
	}
	var confirmedAt int64
	if err := json.Unmarshal(tuple[1], &confirmedAt); err != nil {
		return fmt.Errorf("project message confirmation timestamp must be an integer")
	}
	c.AgentID, c.ConfirmedAt = agentID, confirmedAt
	return nil
}

// ProjectMessageThread contains the root, its direct replies, and root
// confirmations. Replies are ordered by message ID.
type ProjectMessageThread struct {
	Root          ProjectMessage               `json:"root"`
	State         string                       `json:"state"`
	Replies       []ProjectMessage             `json:"replies"`
	Confirmations []ProjectMessageConfirmation `json:"confirmations"`
}

// GetProjectMessageThread resolves either a root or one of its messages to a
// project-scoped thread view. A missing root for a reply falls back to that
// reply, matching the source's one-hop lookup behavior.
func (s *Store) GetProjectMessageThread(ctx context.Context, project string, messageID int64) (*ProjectMessageThread, error) {
	project = projects.NormalizeIdent(project)
	if project == "" || messageID <= 0 {
		return nil, ErrProjectMessageNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	root, err := readProjectMessageByID(ctx, tx, project, messageID)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, ErrProjectMessageNotFound
	}
	if root.ParentMessageID != nil {
		parent, err := readProjectMessageByID(ctx, tx, project, *root.ParentMessageID)
		if err != nil {
			return nil, err
		}
		if parent != nil {
			root = parent
		}
	}

	replies := make([]ProjectMessage, 0)
	rows, err := tx.QueryContext(ctx, `SELECT `+projectMessageProjection+`
		FROM messages m WHERE m.project_ident=? AND m.parent_message_id=? ORDER BY m.id ASC`, project, root.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		message, scanErr := scanProjectMessage(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		replies = append(replies, message)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	confirmations := make([]ProjectMessageConfirmation, 0)
	rows, err = tx.QueryContext(ctx, `SELECT agent_id,confirmed_at FROM agent_confirmations
		WHERE project_ident=? AND message_id=? ORDER BY confirmed_at ASC`, project, root.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var confirmation ProjectMessageConfirmation
		if err := rows.Scan(&confirmation.AgentID, &confirmation.ConfirmedAt); err != nil {
			rows.Close()
			return nil, err
		}
		confirmations = append(confirmations, confirmation)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	state := "open"
	switch {
	case root.ResolvedAt != nil:
		state = "resolved"
	case len(replies) != 0:
		state = "answered"
	case root.Source == "user" && (root.ConfirmedAt != nil || len(confirmations) != 0):
		state = "acknowledged"
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ProjectMessageThread{Root: *root, State: state, Replies: replies, Confirmations: confirmations}, nil
}

// readProjectMessageByID loads a message only from the requested project.
// Keeping this query shared makes reply and root lookups use the same boundary.
func readProjectMessageByID(ctx context.Context, q queryer, project string, id int64) (*ProjectMessage, error) {
	project = projects.NormalizeIdent(project)
	if project == "" || id <= 0 {
		return nil, nil
	}
	message, err := scanProjectMessage(q.QueryRowContext(ctx, `SELECT `+projectMessageProjection+`
		FROM messages m WHERE m.project_ident=? AND m.id=?`, project, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &message, nil
}
