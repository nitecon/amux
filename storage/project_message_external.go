package storage

import (
	"context"
	"fmt"
	"strings"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/projects"
)

// ProjectMessage returns the addressed row only when it belongs to project.
// It deliberately does not resolve replies to their thread root.
func (s *Store) ProjectMessage(ctx context.Context, project string, id int64) (*ProjectMessage, error) {
	project = projects.NormalizeIdent(project)
	message, err := readProjectMessageByID(ctx, s.db, project, id)
	if err != nil {
		return nil, err
	}
	if message == nil {
		return nil, ErrProjectMessageNotFound
	}
	return message, nil
}

// SetProjectMessageExternalID records the provider receipt after a human
// message has been sent. The addressed row must belong to the requested
// project; the provider ID is stored verbatim.
func (s *Store) SetProjectMessageExternalID(ctx context.Context, project string, id int64, externalID string) error {
	project = projects.NormalizeIdent(project)
	if project == "" || id <= 0 || strings.TrimSpace(externalID) == "" {
		return ErrProjectMessageInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE messages SET external_message_id=?
		WHERE project_ident=? AND id=? AND source='user'`, externalID, project, id)
	if err != nil {
		return fmt.Errorf("save external project message receipt: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return ErrProjectMessageNotFound
	}
	return tx.Commit()
}

// SetProjectRoom persists the provider room discovered while ensuring an
// external project channel exists.
func (s *Store) SetProjectRoom(ctx context.Context, project, room string) (*Project, error) {
	project = projects.NormalizeIdent(project)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "UPDATE projects SET room_id=? WHERE ident=?", room, project); err != nil {
		return nil, fmt.Errorf("save project room: %w", err)
	}
	p, err := getProject(ctx, tx, project)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("project %q not found: %w", project, ErrTaskNotFound)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	_ = s.bus.Publish(eventbus.Event{Topic: eventbus.ProjectChanged})
	return p, nil
}
