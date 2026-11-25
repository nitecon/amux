package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
)

// ConversationCommand records durable intent independently from delivery. A
// claimed command must be reconciled by its worker, not automatically repeated.
type ConversationCommand struct {
	Revision         int64                `json:"revision,omitempty"`
	ID               string               `json:"id"`
	ConversationID   string               `json:"conversation_id"`
	Sequence         int64                `json:"sequence"`
	Command          conversation.Command `json:"command"`
	RequestID        string               `json:"request_id,omitempty"`
	TargetInstanceID string               `json:"target_instance_id"`
	State            string               `json:"state"`
	CreatedAt        int64                `json:"created_at"`
	UpdatedAt        int64                `json:"updated_at"`
}

func migrateConversationCommands(ctx context.Context, tx *sql.Tx) error {
	var present bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info('amux_conversation_events') WHERE name='instance_id')").Scan(&present); err != nil {
		return err
	}
	if !present {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE amux_conversation_events ADD COLUMN instance_id TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	// principal records the server-authenticated enrolled client that created
	// the conversation, so session binding and pending follow-up discovery is not decided by caller-supplied headers.
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info('amux_conversations') WHERE name='principal')").Scan(&present); err != nil {
		return err
	}
	if !present {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE amux_conversations ADD COLUMN principal TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS amux_conversation_commands(
 conversation_id TEXT NOT NULL REFERENCES amux_conversations(id),id TEXT NOT NULL,
 sequence INTEGER NOT NULL,kind TEXT NOT NULL,request_json TEXT NOT NULL,
 permission_event_id TEXT,request_id TEXT NOT NULL,target_instance_id TEXT NOT NULL,
 state TEXT NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,
 PRIMARY KEY(conversation_id,id),UNIQUE(conversation_id,sequence));
 CREATE UNIQUE INDEX IF NOT EXISTS amux_conversation_permission_reply ON amux_conversation_commands(conversation_id,permission_event_id) WHERE permission_event_id IS NOT NULL;
 CREATE INDEX IF NOT EXISTS amux_pending_conversation_prompts ON amux_conversation_commands(conversation_id) WHERE kind='prompt' AND state IN ('queued','claimed');
 CREATE TABLE IF NOT EXISTS amux_conversation_command_changes(
 revision INTEGER PRIMARY KEY AUTOINCREMENT,conversation_id TEXT NOT NULL,
 command_id TEXT NOT NULL,FOREIGN KEY(conversation_id,command_id) REFERENCES amux_conversation_commands(conversation_id,id));
 CREATE INDEX IF NOT EXISTS amux_command_change_replay ON amux_conversation_command_changes(conversation_id,revision);
 CREATE TRIGGER IF NOT EXISTS amux_command_insert AFTER INSERT ON amux_conversation_commands BEGIN
 INSERT INTO amux_conversation_command_changes(conversation_id,command_id) VALUES(NEW.conversation_id,NEW.id); END;
 CREATE TRIGGER IF NOT EXISTS amux_command_update AFTER UPDATE ON amux_conversation_commands BEGIN
 INSERT INTO amux_conversation_command_changes(conversation_id,command_id) VALUES(NEW.conversation_id,NEW.id); END;
 INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(6);`)
	return err
}
func scanConversationCommand(row interface{ Scan(...any) error }, extra ...any) (*ConversationCommand, error) {
	var c ConversationCommand
	var raw string
	args := []any{&c.ID, &c.ConversationID, &c.Sequence, &raw, &c.RequestID, &c.TargetInstanceID, &c.State, &c.CreatedAt, &c.UpdatedAt}
	err := row.Scan(append(args, extra...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(raw), &c.Command); err != nil {
		return nil, err
	}
	return &c, nil
}
func readConversationCommand(ctx context.Context, q queryer, id, commandID string) (*ConversationCommand, error) {
	return scanConversationCommand(q.QueryRowContext(ctx, `SELECT id,conversation_id,sequence,request_json,request_id,target_instance_id,state,created_at,updated_at FROM amux_conversation_commands WHERE conversation_id=? AND id=?`, id, commandID))
}
func validateConversationCommand(input conversation.Command) error {
	for _, value := range []string{input.Kind, input.Text, input.PermissionEventID, input.OptionID, input.PromptCommandID} {
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("%w: command must contain valid text", ErrConversationInput)
		}
	}
	switch input.Kind {
	case "prompt":
		if strings.TrimSpace(input.Text) == "" || len(input.Text) > 1<<20 || input.PermissionEventID != "" || input.OptionID != "" || input.PromptCommandID != "" {
			return fmt.Errorf("%w: prompt requires only nonempty text within 1 MiB", ErrConversationInput)
		}
	case "permission":
		id, err := uuid.Parse(input.PermissionEventID)
		if err != nil || id == uuid.Nil || input.Text != "" || len(input.OptionID) > 4096 || input.PromptCommandID != "" {
			return fmt.Errorf("%w: permission requires an event UUID and optional offered option ID", ErrConversationInput)
		}
	case "cancel":
		prompt, err := uuid.Parse(input.PromptCommandID)
		if err != nil || prompt == uuid.Nil || input.Text != "" || input.PermissionEventID != "" || input.OptionID != "" {
			return fmt.Errorf("%w: cancel requires only a prompt command UUID", ErrConversationInput)
		}
	default:
		return fmt.Errorf("%w: unsupported command kind", ErrConversationInput)
	}
	return nil
}
func (s *Store) EnqueueConversationCommand(ctx context.Context, project, id, commandID string, input conversation.Command) (*ConversationCommand, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	command, err := enqueueConversationCommandIn(ctx, tx, project, id, commandID, input)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.wakeConversations()
	return command, nil
}

// enqueueConversationCommandIn shares validation and queue limits with user
// intent. The caller owns commit and the subsequent conversation wake.
func enqueueConversationCommandIn(ctx context.Context, tx *sql.Tx, project, id, commandID string, input conversation.Command) (*ConversationCommand, error) {
	parsed, err := uuid.Parse(commandID)
	if err != nil || parsed == uuid.Nil {
		return nil, fmt.Errorf("%w: command ID must be a nonzero UUID", ErrConversationInput)
	}
	commandID = parsed.String()
	if err = validateConversationCommand(input); err != nil {
		return nil, err
	}
	if input.PermissionEventID != "" {
		parsed, _ := uuid.Parse(input.PermissionEventID)
		input.PermissionEventID = parsed.String()
	}
	if input.PromptCommandID != "" {
		parsed, _ := uuid.Parse(input.PromptCommandID)
		input.PromptCommandID = parsed.String()
	}
	raw, _ := json.Marshal(input)
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("%w: encoded command exceeds 1 MiB", ErrConversationInput)
	}
	c, err := readConversation(ctx, tx, project, id)
	if err != nil {
		return nil, err
	}
	previous, err := readConversationCommand(ctx, tx, id, commandID)
	if err == nil {
		if previous.Command != input {
			return nil, ErrConversationConflict
		}
		return previous, nil
	}
	if !errors.Is(err, ErrConversationNotFound) {
		return nil, err
	}
	var count int
	// Prompt backlog must not consume permission/cancellation capacity while
	// the running prompt is waiting for a decision.
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_conversation_commands WHERE conversation_id=? AND state IN ('queued','claimed') AND (kind='prompt')=?", id, input.Kind == "prompt").Scan(&count); err != nil {
		return nil, err
	}
	if count >= 128 {
		return nil, ErrConversationConflict
	}
	command := &ConversationCommand{ID: commandID, ConversationID: id, Command: input, TargetInstanceID: c.Owner.InstanceID, State: "queued", CreatedAt: time.Now().UnixMilli()}
	command.UpdatedAt = command.CreatedAt
	var permissionID any
	if input.Kind == "permission" {
		event, err := pendingConversationPermission(ctx, tx, c, input.PermissionEventID)
		if err != nil {
			return nil, err
		}
		var ended bool
		offered := input.OptionID == "" // Empty means cancel the permission request.
		for _, option := range event.Options {
			if option.ID == input.OptionID {
				offered = true
			}
		}
		if !offered {
			return nil, fmt.Errorf("%w: permission option was not offered", ErrConversationInput)
		}
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM amux_conversation_commands WHERE conversation_id=? AND permission_event_id=?)", id, input.PermissionEventID).Scan(&ended); err != nil {
			return nil, err
		}
		if ended {
			return nil, ErrConversationConflict
		}
		command.RequestID = event.RequestID
		permissionID = input.PermissionEventID
	}
	if input.Kind == "cancel" {
		if err := pendingConversationCancel(ctx, tx, c, input.PromptCommandID); err != nil {
			return nil, err
		}
	}
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence),0)+1 FROM amux_conversation_commands WHERE conversation_id=?", id).Scan(&command.Sequence); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO amux_conversation_commands(conversation_id,id,sequence,kind,request_json,permission_event_id,request_id,target_instance_id,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, commandID, command.Sequence, input.Kind, string(raw), permissionID, command.RequestID, command.TargetInstanceID, command.State, command.CreatedAt, command.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return command, nil
}

// Start with the small pending-prompt index, even when the project has many
// historical conversations. CROSS JOIN keeps SQLite from reversing the lookup.
const pendingTaskConversationsQuery = `SELECT c.id
		FROM (SELECT DISTINCT conversation_id FROM amux_conversation_commands
			WHERE kind='prompt' AND state IN ('queued','claimed')) q
		CROSS JOIN amux_conversations c
		WHERE c.project_ident=? AND c.task_id IS NOT NULL AND c.native_session_id!=''
		  AND c.id=q.conversation_id
		  AND json_extract(c.owner_json,'$.session_id')=?
		  AND c.provider=? AND json_extract(c.owner_json,'$.os')=? AND c.principal=?
		ORDER BY c.created_at,c.id`

// PendingTaskConversations finds task threads with unfinished prompts for a
// stable worker session. The worker still needs its local recovery journal.
// Only conversations created under the caller's authenticated principal are
// listed; origin headers alone reveal nothing, and conversations
// without a recorded principal are withheld.
func (s *Store) PendingTaskConversations(ctx context.Context, project string, owner AgentOrigin, principal string) ([]string, error) {
	if principal == "" {
		return []string{}, nil
	}
	rows, err := s.db.QueryContext(ctx, pendingTaskConversationsQuery, project, owner.SessionID, owner.Provider, owner.OS, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) ConversationCommands(ctx context.Context, project, id string, after int64, limit int) ([]ConversationCommand, error) {
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("%w: invalid command replay range", ErrConversationInput)
	}
	if _, err := s.Conversation(ctx, project, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,conversation_id,sequence,request_json,request_id,target_instance_id,state,created_at,updated_at FROM amux_conversation_commands WHERE conversation_id=? AND sequence>? ORDER BY sequence LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	return collectConversationCommands(rows, false)
}

// ConversationCommandChanges replays change notifications with the latest
// committed command snapshot. Revisions can repeat a snapshot, never intent.
func (s *Store) ConversationCommandChanges(ctx context.Context, project, id string, after int64, limit int) ([]ConversationCommand, error) {
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("%w: invalid command change range", ErrConversationInput)
	}
	if _, err := s.Conversation(ctx, project, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.conversation_id,c.sequence,c.request_json,c.request_id,c.target_instance_id,c.state,c.created_at,c.updated_at,h.revision FROM amux_conversation_command_changes h JOIN amux_conversation_commands c ON c.conversation_id=h.conversation_id AND c.id=h.command_id WHERE h.conversation_id=? AND h.revision>? ORDER BY h.revision LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	return collectConversationCommands(rows, true)
}
func collectConversationCommands(rows *sql.Rows, withRevision bool) ([]ConversationCommand, error) {
	defer rows.Close()
	result := []ConversationCommand{}
	bytes := 0
	for rows.Next() {
		var revision int64
		var extra []any
		if withRevision {
			extra = []any{&revision}
		}
		c, err := scanConversationCommand(rows, extra...)
		if err != nil {
			return nil, err
		}
		c.Revision = revision
		encoded, err := json.Marshal(c)
		if err != nil {
			return nil, err
		}
		if bytes+len(encoded) > 4<<20 && len(result) > 0 {
			break
		}
		bytes += len(encoded)
		result = append(result, *c)
	}
	return result, rows.Err()
}

func (s *Store) ClaimConversationCommand(ctx context.Context, project, id, commandID string, origin *AgentOrigin) (*ConversationCommand, error) {
	return s.transitionConversationCommand(ctx, project, id, commandID, "claimed", origin)
}
func (s *Store) FinishConversationCommand(ctx context.Context, project, id, commandID, state string, origin *AgentOrigin) (*ConversationCommand, error) {
	if state != "done" && state != "rejected" && state != "interrupted" {
		return nil, fmt.Errorf("%w: invalid command outcome", ErrConversationInput)
	}
	return s.transitionConversationCommand(ctx, project, id, commandID, state, origin)
}
func (s *Store) transitionConversationCommand(ctx context.Context, project, id, commandID, state string, origin *AgentOrigin) (*ConversationCommand, error) {
	var result *ConversationCommand
	_, err := s.mutateConversation(ctx, project, id, origin, func(tx *sql.Tx, c *Conversation, actor AgentOrigin) error {
		if c.Owner != actor {
			return ErrTaskOwnership
		}
		command, err := readConversationCommand(ctx, tx, id, commandID)
		if err != nil {
			return err
		}
		if command.TargetInstanceID != actor.InstanceID {
			return ErrConversationConflict
		}
		if state == "claimed" && command.State == "queued" && command.Command.Kind == "prompt" {
			var blocked bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM amux_conversation_commands WHERE conversation_id=? AND kind='prompt' AND (state='claimed' OR (state='queued' AND sequence<?)))`, id, command.Sequence).Scan(&blocked); err != nil {
				return err
			}
			if blocked {
				return ErrConversationConflict
			}
		}
		if state == "claimed" && (command.State == "queued" || command.State == "claimed") {
			var expired error
			if command.Command.Kind == "permission" {
				_, expired = pendingConversationPermission(ctx, tx, c, command.Command.PermissionEventID)
			}
			if command.Command.Kind == "cancel" {
				expired = pendingConversationCancel(ctx, tx, c, command.Command.PromptCommandID)
			}
			if expired != nil {
				if !errors.Is(expired, ErrConversationConflict) {
					return expired
				}
				command.State = "rejected"
				command.UpdatedAt = time.Now().UnixMilli()
				_, err = tx.ExecContext(ctx, "UPDATE amux_conversation_commands SET state='rejected',updated_at=? WHERE conversation_id=? AND id=?", command.UpdatedAt, id, commandID)
				result = command
				return err
			}
		}
		if command.State == state {
			result = command
			return nil
		}
		if (state == "claimed" && command.State != "queued") || (state != "claimed" && command.State != "claimed") {
			return ErrConversationConflict
		}
		command.State = state
		command.UpdatedAt = time.Now().UnixMilli()
		_, err = tx.ExecContext(ctx, "UPDATE amux_conversation_commands SET state=?,updated_at=? WHERE conversation_id=? AND id=?", state, command.UpdatedAt, id, commandID)
		result = command
		return err
	})
	return result, err
}

// Revalidate at dispatch as well as enqueue: provider request IDs can be reused,
// and a queued response must never answer a later turn's request.
func pendingConversationPermission(ctx context.Context, tx *sql.Tx, c *Conversation, eventID string) (*conversation.Event, error) {
	var raw, instance string
	var sequence int64
	err := tx.QueryRowContext(ctx, "SELECT event_json,instance_id,sequence FROM amux_conversation_events WHERE conversation_id=? AND event_id=?", c.ID, eventID).Scan(&raw, &instance, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConversationConflict
	}
	if err != nil {
		return nil, err
	}
	var event conversation.Event
	if err = json.Unmarshal([]byte(raw), &event); err != nil {
		return nil, err
	}
	if event.Kind != "permission" || event.RequestID == "" || instance != c.Owner.InstanceID || event.SessionID != c.NativeSessionID {
		return nil, ErrConversationConflict
	}
	var ended bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM amux_conversation_events WHERE conversation_id=? AND sequence>? AND
 (json_extract(event_json,'$.kind')='turn_completed' OR
 (json_extract(event_json,'$.kind') IN ('permission','permission_resolved') AND json_extract(event_json,'$.request_id')=?)))`, c.ID, sequence, event.RequestID).Scan(&ended)
	if err != nil {
		return nil, err
	}
	if ended {
		return nil, ErrConversationConflict
	}
	return &event, nil
}
func pendingConversationCancel(ctx context.Context, tx *sql.Tx, c *Conversation, promptID string) error {
	prompt, err := readConversationCommand(ctx, tx, c.ID, promptID)
	if errors.Is(err, ErrConversationNotFound) {
		return ErrConversationConflict
	}
	if err != nil {
		return err
	}
	if prompt.Command.Kind != "prompt" || prompt.State != "claimed" || prompt.TargetInstanceID != c.Owner.InstanceID {
		return ErrConversationConflict
	}
	return nil
}
