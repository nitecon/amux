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
	"github.com/nitecon/amux/eventbus"
)

var (
	ErrConversationNotFound = errors.New("conversation not found")
	ErrConversationConflict = errors.New("conversation state conflict")
	ErrConversationInput    = errors.New("invalid conversation input")
)

// Conversation binds one task to one provider session. An instance change fences
// the previous worker's writes without changing the native session or transcript.
type Conversation struct {
	FleetHostname *string `json:"fleet_hostname,omitempty"`
	// OwnerHostname is the owner session's last reported machine, when known.
	OwnerHostname   *string     `json:"owner_hostname,omitempty"`
	ID              string      `json:"id"`
	ProjectIdent    string      `json:"project_ident"`
	TaskID          string      `json:"task_id"`
	UpdatedAt       int64       `json:"updated_at,omitempty"`
	TaskTitle       string      `json:"task_title,omitempty"`
	Provider        string      `json:"provider"`
	NativeSessionID string      `json:"native_session_id"`
	Owner           AgentOrigin `json:"owner"`
	CreatedAt       int64       `json:"created_at"`
}
type ConversationEvent struct {
	ID             string             `json:"id"`
	ConversationID string             `json:"conversation_id"`
	Sequence       int64              `json:"sequence"`
	Event          conversation.Event `json:"event"`
	CreatedAt      int64              `json:"created_at"`
}

func migrateConversations(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS amux_conversations(
 id TEXT PRIMARY KEY, project_ident TEXT NOT NULL REFERENCES projects(ident),
	 task_id TEXT REFERENCES tasks(id), provider TEXT NOT NULL,
 native_session_id TEXT NOT NULL DEFAULT '', owner_json TEXT NOT NULL,
 created_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS amux_conversations_task ON amux_conversations(project_ident,task_id,created_at,id);
 CREATE TABLE IF NOT EXISTS amux_conversation_events(
 conversation_id TEXT NOT NULL REFERENCES amux_conversations(id),
 sequence INTEGER NOT NULL, event_id TEXT NOT NULL,event_json TEXT NOT NULL,
 created_at INTEGER NOT NULL,PRIMARY KEY(conversation_id,sequence),
 UNIQUE(conversation_id,event_id));
 INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(5);`)
	return err
}
func readConversation(ctx context.Context, q queryer, project, id string) (*Conversation, error) {
	return scanConversation(q.QueryRowContext(ctx, `SELECT id,project_ident,task_id,provider,native_session_id,owner_json,created_at FROM amux_conversations WHERE project_ident=? AND id=?`, project, id), false)
}
func scanConversation(row interface{ Scan(...any) error }, withTitle bool, extra ...any) (*Conversation, error) {
	var c Conversation
	var owner string
	var taskID sql.NullString
	fields := []any{&c.ID, &c.ProjectIdent, &taskID, &c.Provider, &c.NativeSessionID, &owner, &c.CreatedAt}
	if withTitle {
		fields = append(fields, &c.TaskTitle, &c.UpdatedAt)
	}
	err := row.Scan(append(fields, extra...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, err
	}
	if taskID.Valid {
		c.TaskID = taskID.String
	}
	if err = json.Unmarshal([]byte(owner), &c.Owner); err != nil {
		return nil, fmt.Errorf("invalid stored conversation owner: %w", err)
	}
	return &c, nil
}
func (s *Store) Conversation(ctx context.Context, project, id string) (*Conversation, error) {
	info, err := readConversation(ctx, s.db, project, id)
	if err != nil {
		return nil, err
	}
	var hostname string
	err = s.db.QueryRowContext(ctx, "SELECT hostname FROM amux_fleet_conversations WHERE conversation_id=?", id).Scan(&hostname)
	if err == nil {
		info.FleetHostname = &hostname
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return info, nil
}

// TaskConversations pages in stable ID order. Use the last returned ID as afterID.
func (s *Store) TaskConversations(ctx context.Context, project, taskID, afterID string, limit int) ([]Conversation, error) {
	return s.listConversations(ctx, project, &taskID, afterID, limit)
}

// ProjectConversations includes every conversation in the selected project.
func (s *Store) ProjectConversations(ctx context.Context, project, afterID string, limit int) ([]Conversation, error) {
	return s.listConversations(ctx, project, nil, afterID, limit)
}

func (s *Store) listConversations(ctx context.Context, project string, taskID *string, afterID string, limit int) ([]Conversation, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("%w: invalid conversation list limit", ErrConversationInput)
	}
	if taskID != nil {
		var found bool
		if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM tasks WHERE project_ident=? AND id=?)", project, *taskID).Scan(&found); err != nil {
			return nil, err
		}
		if !found {
			return nil, ErrTaskNotFound
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,project_ident,task_id,provider,native_session_id,owner_json,created_at,
 COALESCE((SELECT title FROM tasks t WHERE t.id=c.task_id AND t.project_ident=c.project_ident), ''),
 COALESCE((SELECT created_at FROM amux_conversation_events e WHERE e.conversation_id=c.id ORDER BY sequence DESC LIMIT 1), c.created_at),
 COALESCE((SELECT COALESCE(NULLIF(a.hostname,''),NULLIF(s.hostname,'')) FROM amux_presence_agents a LEFT JOIN amux_presence_sources s ON s.source=a.source
   WHERE a.project_ident=c.project_ident AND a.session_id=json_extract(c.owner_json,'$.session_id') ORDER BY a.seen_at DESC LIMIT 1), '') FROM amux_conversations c WHERE project_ident=? AND id>? AND (? IS NULL OR task_id=?) ORDER BY id LIMIT ?`, project, afterID, taskID, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Conversation{}
	for rows.Next() {
		var hostname string
		c, err := scanConversation(rows, true, &hostname)
		if err != nil {
			return nil, err
		}
		if hostname != "" {
			c.OwnerHostname = &hostname
		}
		result = append(result, *c)
	}
	return result, rows.Err()
}
func (s *Store) CreateConversation(ctx context.Context, project, taskID string, origin *AgentOrigin) (*Conversation, error) {
	return s.CreateConversationWithGoal(ctx, project, taskID, origin, nil)
}

// CreateConversationWithGoal atomically claims queued work and creates its
// conversation. Tagged tasks require the caller to select their exact goal.
func (s *Store) CreateConversationWithGoal(ctx context.Context, project, taskID string, origin *AgentOrigin, goal *string) (*Conversation, error) {
	return s.CreateConversationWithPrincipal(ctx, project, taskID, origin, goal, "")
}

// CreateConversationWithPrincipal also records the server-authenticated
// enrolled client that created the conversation. Only that principal may bind
// its session or discover its pending follow-ups.
func (s *Store) CreateConversationWithPrincipal(ctx context.Context, project, taskID string, origin *AgentOrigin, goal *string, principal string) (*Conversation, error) {
	selectedGoal, err := NormalizeTaskGoal(goal)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid selected goal", ErrTaskConflict)
	}
	actor, err := NormalizeOrigin(origin)
	if err != nil {
		return nil, err
	}
	if actor == nil {
		return nil, ErrTaskOwnership
	}
	var c *Conversation
	err = s.taskMutation(ctx, actor, func(tx *sql.Tx, actor *AgentOrigin) error {
		conversationProject := project
		if taskID != "" {
			task, err := readTask(ctx, tx, project, taskID)
			if err != nil {
				return err
			}
			if task == nil {
				return ErrTaskNotFound
			}
			if task.Goal != nil && !sameOptionalString(task.Goal, selectedGoal) {
				return fmt.Errorf("%w: select the task's goal before creating its conversation", ErrTaskConflict)
			}
			if task.Status == "todo" && !isReviewAssignment(*task) {
				selected := ""
				if selectedGoal != nil {
					selected = *selectedGoal
				}
				if _, err = transitionActiveTask(ctx, tx, project, taskID, "in_progress", actor, TaskTransitionPolicy{CheckGoal: true, SelectedGoal: selected}); err != nil {
					return err
				}
			}
		} else {
			projectRecord, err := getProject(ctx, tx, project)
			if err != nil {
				return err
			}
			if projectRecord == nil {
				return ErrTaskNotFound
			}
			conversationProject = projectRecord.Ident
		}
		c = &Conversation{ID: uuid.NewString(), ProjectIdent: conversationProject, TaskID: taskID, Provider: actor.Provider, Owner: *actor, CreatedAt: time.Now().UnixMilli()}
		owner, err := json.Marshal(actor)
		if err != nil {
			return err
		}
		var storedTaskID any
		if taskID != "" {
			storedTaskID = taskID
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO amux_conversations(id,project_ident,task_id,provider,owner_json,created_at,principal) VALUES(?,?,?,?,?,?,?)`, c.ID, conversationProject, storedTaskID, c.Provider, string(owner), c.CreatedAt, principal)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.wakeConversations()
	return c, nil
}

// mutateConversation serializes ownership checks and mutations in the same
// SQLite write transaction, including across separate server connections.
func (s *Store) mutateConversation(ctx context.Context, project, id string, origin *AgentOrigin, change func(*sql.Tx, *Conversation, AgentOrigin) error) (*Conversation, error) {
	actor, err := NormalizeOrigin(origin)
	if err != nil {
		return nil, err
	}
	if actor == nil {
		return nil, ErrTaskOwnership
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	c, err := readConversation(ctx, tx, project, id)
	if err != nil {
		return nil, err
	}
	if err = change(tx, c, *actor); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.wakeConversations()
	return c, nil
}
func (s *Store) BindConversationSession(ctx context.Context, project, id, nativeID string, origin *AgentOrigin) (*Conversation, error) {
	return s.BindConversationSessionWithPrincipal(ctx, project, id, nativeID, origin, "")
}

// BindConversationSessionWithPrincipal rejects a principal other than the one
// recorded at creation. Binding never records a principal, so a conversation
// without one (legacy rows) stays withheld from pending discovery.
func (s *Store) BindConversationSessionWithPrincipal(ctx context.Context, project, id, nativeID string, origin *AgentOrigin, principal string) (*Conversation, error) {
	if nativeID == "" || len(nativeID) > 4096 || !utf8.ValidString(nativeID) || strings.ContainsRune(nativeID, 0) {
		return nil, fmt.Errorf("%w: native session ID must be a nonempty bounded string", ErrConversationInput)
	}
	return s.mutateConversation(ctx, project, id, origin, func(tx *sql.Tx, c *Conversation, actor AgentOrigin) error {
		if c.Owner != actor {
			return ErrTaskOwnership
		}
		if c.NativeSessionID != "" && c.NativeSessionID != nativeID {
			return ErrConversationConflict
		}
		var bound string
		if err := tx.QueryRowContext(ctx, "SELECT principal FROM amux_conversations WHERE id=?", id).Scan(&bound); err != nil {
			return err
		}
		if bound != "" && bound != principal {
			return ErrTaskOwnership
		}
		_, err := tx.ExecContext(ctx, "UPDATE amux_conversations SET native_session_id=? WHERE id=?", nativeID, id)
		if err == nil {
			c.NativeSessionID = nativeID
		}
		return err
	})
}

// ResumeConversation is an explicit compare-and-swap transfer to a new process
// instance of the same stable agent. A retry by the winning instance is harmless.
func (s *Store) ResumeConversation(ctx context.Context, project, id, previousInstance string, origin *AgentOrigin) (*Conversation, error) {
	taskOwnerChanged := false
	c, err := s.mutateConversation(ctx, project, id, origin, func(tx *sql.Tx, c *Conversation, actor AgentOrigin) error {
		if c.Owner.SessionID != actor.SessionID || c.Owner.Provider != actor.Provider || c.Owner.OS != actor.OS {
			return ErrTaskOwnership
		}
		if c.Owner == actor {
			return nil
		}
		if c.Owner.InstanceID != previousInstance {
			return ErrConversationConflict
		}
		// Recover the task claim only when it still belongs to this exact prior
		// conversation owner. Done, cancelled, unclaimed and foreign work stay put.
		var task *Task
		if c.TaskID != "" {
			var err error
			task, err = readTask(ctx, tx, c.ProjectIdent, c.TaskID)
			if err != nil {
				return err
			}
		}
		if task != nil && task.Status == "in_progress" && task.OwnerOrigin != nil && *task.OwnerOrigin == c.Owner {
			ownerJSON, err := json.Marshal(actor)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE task_mutation_context SET origin_json=? WHERE id=1", string(ownerJSON)); err != nil {
				return err
			}
			routing := task.Routing
			var route TaskRouting
			if err := json.Unmarshal(routing, &route); err == nil && route.Mode == "session" && route.Target != nil && route.Target.Origin == c.Owner {
				route.Target = &RegisteredTargetIdentity{Origin: actor, Generation: actor.InstanceID}
				routing, err = json.Marshal(route)
				if err != nil {
					return err
				}
			}
			if _, err = tx.ExecContext(ctx, `UPDATE tasks SET owner_origin_json=?,routing_json=?,updated_at=? WHERE id=? AND project_ident=? AND status='in_progress'`, string(ownerJSON), string(routing), time.Now().UnixMilli(), c.TaskID, c.ProjectIdent); err != nil {
				return err
			}
			if err = transferTargetRegistration(ctx, tx, c.ProjectIdent, c.Owner, actor, string(ownerJSON)); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE task_mutation_context SET origin_json=NULL WHERE id=1"); err != nil {
				return err
			}
			taskOwnerChanged = true
		}
		// Unclaimed prompts retain durable user intent. Claimed work has an
		// uncertain execution boundary; never repeat it on the new instance.
		if _, err := tx.ExecContext(ctx, `UPDATE amux_conversation_commands SET state='interrupted',updated_at=? WHERE conversation_id=? AND (state='claimed' OR (state='queued' AND kind!='prompt'))`, time.Now().UnixMilli(), id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE amux_conversation_commands SET target_instance_id=?,updated_at=? WHERE conversation_id=? AND state='queued' AND kind='prompt'`, actor.InstanceID, time.Now().UnixMilli(), id); err != nil {
			return err
		}
		previous := c.Owner
		owner, _ := json.Marshal(actor)
		if _, err := tx.ExecContext(ctx, "UPDATE amux_conversations SET owner_json=? WHERE id=?", string(owner), id); err != nil {
			return err
		}
		if c.NativeSessionID != "" {
			data, err := json.Marshal(struct {
				NativeSessionID string      `json:"native_session_id"`
				InstanceID      string      `json:"instance_id"`
				PreviousOrigin  AgentOrigin `json:"previous_origin"`
				CurrentOrigin   AgentOrigin `json:"current_origin"`
			}{c.NativeSessionID, actor.InstanceID, previous, actor})
			if err != nil {
				return err
			}
			event := conversation.Event{Kind: "owner_handoff", SessionID: c.NativeSessionID, Status: "owner_changed", Data: data}
			raw, err := encodeConversationEvent(event)
			if err != nil {
				return err
			}
			var sequence int64
			if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence),0)+1 FROM amux_conversation_events WHERE conversation_id=?", id).Scan(&sequence); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO amux_conversation_events(conversation_id,sequence,event_id,event_json,created_at,instance_id) VALUES(?,?,?,?,?,?)`, id, sequence, uuid.NewString(), string(raw), time.Now().UnixMilli(), actor.InstanceID)
			if err != nil {
				return err
			}
		}
		c.Owner = actor
		return nil
	})
	if err == nil && taskOwnerChanged {
		s.wakeTaskEvents()
	}
	return c, err
}

func (s *Store) AppendConversationEvent(ctx context.Context, project, id, eventID string, event conversation.Event, origin *AgentOrigin) (*ConversationEvent, error) {
	parsed, err := uuid.Parse(eventID)
	if err != nil || parsed == uuid.Nil {
		return nil, fmt.Errorf("%w: event ID must be a nonzero UUID", ErrConversationInput)
	}
	eventID = parsed.String()
	raw, err := encodeConversationEvent(event)
	if err != nil {
		return nil, err
	}
	var result *ConversationEvent
	_, err = s.mutateConversation(ctx, project, id, origin, func(tx *sql.Tx, c *Conversation, actor AgentOrigin) error {
		if c.Owner != actor {
			return ErrTaskOwnership
		}
		if c.NativeSessionID == "" || c.NativeSessionID != event.SessionID {
			return ErrConversationConflict
		}
		var stored string
		e := ConversationEvent{ID: eventID, ConversationID: id, Event: event}
		err := tx.QueryRowContext(ctx, "SELECT sequence,event_json,created_at FROM amux_conversation_events WHERE conversation_id=? AND event_id=?", id, eventID).Scan(&e.Sequence, &stored, &e.CreatedAt)
		if err == nil {
			if stored != string(raw) {
				return ErrConversationConflict
			}
			result = &e
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence),0)+1 FROM amux_conversation_events WHERE conversation_id=?", id).Scan(&e.Sequence); err != nil {
			return err
		}
		e.CreatedAt = time.Now().UnixMilli()
		_, err = tx.ExecContext(ctx, "INSERT INTO amux_conversation_events(conversation_id,sequence,event_id,event_json,created_at,instance_id) VALUES(?,?,?,?,?,?)", id, e.Sequence, eventID, string(raw), e.CreatedAt, actor.InstanceID)
		result = &e
		return err
	})
	return result, err
}
func (s *Store) ConversationEvents(ctx context.Context, project, id string, after int64, limit int) ([]ConversationEvent, error) {
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("%w: invalid conversation replay range", ErrConversationInput)
	}
	if _, err := s.Conversation(ctx, project, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT sequence,event_id,event_json,created_at FROM amux_conversation_events WHERE conversation_id=? AND sequence>? ORDER BY sequence LIMIT ?", id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ConversationEvent{}
	bytes := 0
	for rows.Next() {
		e := ConversationEvent{ConversationID: id}
		var raw string
		if err = rows.Scan(&e.Sequence, &e.ID, &raw, &e.CreatedAt); err != nil {
			return nil, err
		}
		// Bound a replay page by bytes as well as count. Resume from the last
		// returned sequence; an event is never split across pages.
		if bytes+len(raw) > 4<<20 && len(result) > 0 {
			break
		}
		bytes += len(raw)
		if err = json.Unmarshal([]byte(raw), &e.Event); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// ConversationChanges is a wake-up hint. Acquire it before replaying committed
// sequences; a slow reader always catches up from SQLite rather than a queue.
func (s *Store) ConversationChanges() <-chan struct{} {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	return s.conversationChanges
}
func (s *Store) wakeConversations() {
	s.eventMu.Lock()
	close(s.conversationChanges)
	s.conversationChanges = make(chan struct{})
	s.eventMu.Unlock()
	_ = s.bus.Publish(eventbus.Event{Topic: eventbus.ConversationChanged})
}

func encodeConversationEvent(event conversation.Event) ([]byte, error) {
	if event.Kind == "" || len(event.Kind) > 64 {
		return nil, fmt.Errorf("%w: event kind must be a nonempty bounded string", ErrConversationInput)
	}
	for _, value := range []string{event.Kind, event.SessionID, event.MessageID, event.ToolCallID, event.RequestID, event.Text, event.Title, event.Status} {
		if !utf8.ValidString(value) {
			return nil, fmt.Errorf("%w: conversation event must contain valid UTF-8", ErrConversationInput)
		}
	}
	if !utf8.Valid(event.Data) {
		return nil, fmt.Errorf("%w: event data must contain valid UTF-8", ErrConversationInput)
	}
	if event.Kind == "permission" {
		if event.RequestID == "" || len(event.RequestID) > 4096 || len(event.Options) == 0 || len(event.Options) > 128 {
			return nil, fmt.Errorf("%w: invalid permission request", ErrConversationInput)
		}
		seen := map[string]bool{}
		for _, option := range event.Options {
			if option.ID == "" || strings.ContainsRune(option.ID, 0) || seen[option.ID] || len(option.ID) > 4096 || len(option.Name) > 4096 || len(option.Kind) > 64 || !utf8.ValidString(option.ID+option.Name+option.Kind) {
				return nil, fmt.Errorf("%w: invalid permission options", ErrConversationInput)
			}
			seen[option.ID] = true
		}
	}
	raw, err := json.Marshal(event)
	if err != nil || len(raw) > 4<<20 {
		return nil, fmt.Errorf("%w: conversation event must be valid JSON within 4 MiB", ErrConversationInput)
	}
	return raw, nil
}
