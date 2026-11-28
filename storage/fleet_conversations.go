package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/identity/agent"
	projectid "github.com/nitecon/amux/identity/project"
)

var ErrFleetAgentUnavailable = errors.New("agent is unavailable for fleet conversations")

type FleetConversation struct {
	Conversation
	Hostname string `json:"hostname"`
}

func migrateFleetConversations(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS amux_fleet_conversations(
		conversation_id TEXT PRIMARY KEY REFERENCES amux_conversations(id) ON DELETE CASCADE,
		project_ident TEXT NOT NULL REFERENCES projects(ident) ON DELETE CASCADE,
		session_id TEXT NOT NULL,
		principal TEXT NOT NULL,
		hostname TEXT NOT NULL,
		UNIQUE(project_ident,session_id)
	);
	CREATE INDEX IF NOT EXISTS amux_fleet_conversations_principal ON amux_fleet_conversations(principal);`)
	return err
}

// OpenFleetConversation returns the default conversation for a stable agent
// session. Its owning node comes from authenticated worker presence.
func (s *Store) OpenFleetConversation(ctx context.Context, project, session string, now time.Time) (*FleetConversation, error) {
	project = projectid.NormalizeIdent(project)
	if id, err := uuid.Parse(session); err != nil || id == uuid.Nil || id.String() != session {
		return nil, fmt.Errorf("%w: agent session must be a UUID", ErrConversationInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	record, err := getProject(ctx, tx, project)
	if err != nil {
		return nil, err
	}
	if record == nil || record.ArchivedAt != nil {
		return nil, ErrTaskNotFound
	}
	var id, hostname string
	err = tx.QueryRowContext(ctx, `SELECT conversation_id,hostname FROM amux_fleet_conversations WHERE project_ident=? AND session_id=?`,
		project, session).Scan(&id, &hostname)
	if err == nil {
		info, err := readConversation(ctx, tx, project, id)
		if err != nil {
			return nil, err
		}
		info.FleetHostname = &hostname
		return &FleetConversation{Conversation: *info, Hostname: hostname}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	owner := agent.Origin{SessionID: session}
	var principal string
	err = tx.QueryRowContext(ctx, `SELECT a.instance_id,a.provider,a.os,s.principal,COALESCE(a.hostname,s.hostname,'')
		FROM amux_presence_agents a JOIN amux_presence_sources s ON s.source=a.source
		WHERE a.project_ident=? AND a.session_id=? AND a.state!='gone' AND a.seen_at>=?
		  AND a.report_kind='native' AND a.sequence=s.sequence AND a.sequence=s.principal_sequence
		  AND s.principal IS NOT NULL AND s.principal!='api-key'
		ORDER BY a.seen_at DESC, a.instance_id LIMIT 1`, project, session, now.UnixMilli()-presenceGoneAfter).
		Scan(&owner.InstanceID, &owner.Provider, &owner.OS, &principal, &hostname)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrFleetAgentUnavailable
	}
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(owner)
	if err != nil {
		return nil, err
	}
	info := Conversation{ID: uuid.NewString(), FleetHostname: &hostname, ProjectIdent: project, Provider: owner.Provider, Owner: owner, CreatedAt: now.UnixMilli()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO amux_conversations(id,project_ident,provider,owner_json,created_at)
		VALUES(?,?,?,?,?)`, info.ID, project, owner.Provider, string(encoded), info.CreatedAt); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO amux_fleet_conversations(conversation_id,project_ident,session_id,principal,hostname)
		VALUES(?,?,?,?,?)`, info.ID, project, session, principal, hostname); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.wakeConversations()
	_ = s.bus.Publish(eventbus.Event{Topic: eventbus.FleetConversationChanged})
	return &FleetConversation{Conversation: info, Hostname: hostname}, nil
}

func (s *Store) FleetConversations(ctx context.Context, principal string) ([]FleetConversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.project_ident,c.provider,c.native_session_id,c.owner_json,c.created_at,f.hostname
		FROM amux_fleet_conversations f JOIN amux_conversations c ON c.id=f.conversation_id
		JOIN projects p ON p.ident=f.project_ident
		WHERE f.principal=? AND p.archived_at IS NULL ORDER BY c.created_at,c.id`, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]FleetConversation, 0)
	for rows.Next() {
		var item FleetConversation
		var native sql.NullString
		var owner string
		if err := rows.Scan(&item.ID, &item.ProjectIdent, &item.Provider, &native, &owner, &item.CreatedAt, &item.Hostname); err != nil {
			return nil, err
		}
		item.NativeSessionID = native.String
		item.FleetHostname = &item.Hostname
		if err := json.Unmarshal([]byte(owner), &item.Owner); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// FleetConversationWriteAllowed limits runner actions to the owning peer.
// Existing task and ordinary conversations retain their ownership rules.
func (s *Store) FleetConversationWriteAllowed(ctx context.Context, id, principal string) (bool, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, "SELECT principal FROM amux_fleet_conversations WHERE conversation_id=?", id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return err == nil && principal != "" && owner == principal, err
}
