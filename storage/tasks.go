package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/taskplatform"
)

var (
	ErrTaskOwnership   = errors.New("task ownership requires the exact claiming agent session and instance")
	ErrTaskNotFound    = errors.New("task not found")
	ErrTaskConflict    = errors.New("task state conflict")
	ErrTaskUnsupported = errors.New("task workflow is not yet ported")
	ErrTaskChangeset   = errors.New("task transition requires an explicit file-change declaration; the task has not moved")
)

// IsActiveTaskStatus reports whether status represents active work with a retained claim.
func IsActiveTaskStatus(status string) bool {
	return status == "in_progress" || status == "stalled"
}

// IsReorderableTaskStatus reports whether status is a board column whose tasks can be reordered.
func IsReorderableTaskStatus(status string) bool {
	return status == "todo" || IsActiveTaskStatus(status) || status == "user_review" || status == "done"
}

// IsValidTaskStatus reports whether status is a known task status.
func IsValidTaskStatus(status string) bool {
	return IsReorderableTaskStatus(status) || status == "cancelled"
}

// OpenTaskStatuses returns the statuses listed when no status filter is given.
func OpenTaskStatuses() []string {
	return []string{"todo", "in_progress", "stalled"}
}

type TaskParent struct {
	ID           string `json:"id"`
	ProjectIdent string `json:"project_ident"`
	Title        string `json:"title"`
}
type Task struct {
	Changeset               []string        `json:"changeset"`
	TaskType                string          `json:"-"`
	Parent                  *TaskParent     `json:"parent"`
	Routing                 json.RawMessage `json:"routing"`
	Requester               json.RawMessage `json:"requester"`
	ID                      string          `json:"id"`
	ProjectIdent            string          `json:"project_ident"`
	Title                   string          `json:"title"`
	Goal                    *string         `json:"goal"`
	Description             *string         `json:"description"`
	ReviewAction            *string         `json:"review_action"`
	Details                 *string         `json:"details"`
	Status                  string          `json:"status"`
	Rank                    int64           `json:"rank"`
	Labels                  []string        `json:"labels"`
	Hostname                *string         `json:"hostname"`
	OwnerAgentID            *string         `json:"owner_agent_id"`
	OwnerOrigin             *AgentOrigin    `json:"owner_origin"`
	CompletionOwner         *AgentOrigin    `json:"completion_owner"`
	Reporter                string          `json:"reporter"`
	CreatedAt               int64           `json:"created_at"`
	UpdatedAt               int64           `json:"updated_at"`
	StartedAt               *int64          `json:"started_at"`
	DoneAt                  *int64          `json:"done_at"`
	Kind                    string          `json:"kind"`
	DelegatedToProjectIdent *string         `json:"delegated_to_project_ident"`
	DelegatedToTaskID       *string         `json:"delegated_to_task_id"`
	DelegationReady         bool            `json:"delegation_ready"`
	// WaitingOnUser reports an unanswered task_ask question; ownership is unchanged.
	WaitingOnUser bool `json:"waiting_on_user"`
}
type TaskComment struct {
	ID         string       `json:"id"`
	TaskID     string       `json:"task_id"`
	Author     string       `json:"author"`
	AuthorType string       `json:"author_type"`
	Content    string       `json:"content"`
	CreatedAt  int64        `json:"created_at"`
	Hostname   *string      `json:"hostname"`
	Origin     *AgentOrigin `json:"origin"`
}
type TaskDetail struct {
	Task         Task          `json:"task"`
	Comments     []TaskComment `json:"comments"`
	CommentCount int64         `json:"-"`
}
type TaskInsert struct {
	Project, Title, Reporter                                   string
	Description, Details, Hostname, Goal, ParentID, RootReason *string
	CanonicalRemote                                            *string
	Labels                                                     []string
	Requester                                                  *RegisteredTargetIdentity
	Routing                                                    *TaskRouting
}

func NormalizeOrigin(origin *AgentOrigin) (*AgentOrigin, error) {
	if origin == nil {
		return nil, nil
	}
	copy := *origin
	session, err := uuid.Parse(copy.SessionID)
	if err != nil {
		return nil, fmt.Errorf("agent session and instance IDs must be UUIDs")
	}
	instance, err := uuid.Parse(copy.InstanceID)
	if err != nil {
		return nil, fmt.Errorf("agent session and instance IDs must be UUIDs")
	}
	if (copy.Provider != "codex" && copy.Provider != "claude" && copy.Provider != "antigravity") || (copy.OS != "linux" && copy.OS != "windows" && copy.OS != "macos") {
		return nil, fmt.Errorf("invalid agent provider or OS")
	}
	copy.SessionID = session.String()
	copy.InstanceID = instance.String()
	return &copy, nil
}
func NormalizeTaskGoal(goal *string) (*string, error) {
	if goal == nil {
		return nil, nil
	}
	clean := strings.TrimSpace(*goal)
	if clean == "" {
		return nil, nil
	}
	if len(clean) > 128 || strings.IndexFunc(clean, unicode.IsControl) >= 0 {
		return nil, fmt.Errorf("goal must be at most 128 UTF-8 bytes with no control characters")
	}
	return &clean, nil
}

func (s *Store) taskMutation(ctx context.Context, origin *AgentOrigin, mutate func(*sql.Tx, *AgentOrigin) error) error {
	origin, err := NormalizeOrigin(origin)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if fence, ok := ctx.Value(conversationFenceKey{}).(conversationFence); ok {
		if origin == nil {
			return ErrTaskOwnership
		}
		conversation, err := readConversation(ctx, tx, fence.project, fence.conversation)
		if err != nil {
			if errors.Is(err, ErrConversationNotFound) {
				return ErrTaskOwnership
			}
			return err
		}
		if conversation.Owner != *origin {
			return ErrTaskOwnership
		}
		revoked, err := conversationRevoked(ctx, tx, conversation.ID)
		if err != nil {
			return err
		}
		if revoked {
			return ErrTaskOwnership
		}
	}
	var raw any
	if origin != nil {
		b, err := json.Marshal(origin)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE task_mutation_context SET origin_json=? WHERE id=1", raw); err != nil {
		return err
	}
	if err := mutate(tx, origin); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE task_mutation_context SET origin_json=NULL WHERE id=1"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.wakeTaskEvents()
	return nil
}

const taskSelect = `t.id,t.project_ident,t.title,t.description,t.details,t.status,t.rank,t.labels,t.hostname,t.owner_agent_id,t.reporter,t.created_at,t.updated_at,t.started_at,t.done_at,t.kind,t.delegated_to_project_ident,t.delegated_to_task_id,t.owner_origin_json,t.goal,t.routing_json,t.requester_json,
 (SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title) FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id=t.id),t.review_action,t.task_type,t.changeset_json,
 (SELECT origin_json FROM task_completion_owners WHERE task_id=t.id),
 (t.kind='delegated' AND EXISTS(SELECT 1 FROM task_delegations d JOIN tasks target ON target.id=d.target_task_id AND target.project_ident=d.target_project_ident WHERE d.source_project_ident=t.project_ident AND d.source_task_id=t.id AND target.status='done')),
 EXISTS(SELECT 1 FROM task_questions q WHERE q.task_id=t.id AND q.answered_at IS NULL)`

func scanTask(row scanner) (*Task, error) {
	var t Task
	var labels, origin, routing, requester, parent, changeset, completion *string
	err := row.Scan(&t.ID, &t.ProjectIdent, &t.Title, &t.Description, &t.Details, &t.Status, &t.Rank, &labels, &t.Hostname, &t.OwnerAgentID, &t.Reporter, &t.CreatedAt, &t.UpdatedAt, &t.StartedAt, &t.DoneAt, &t.Kind, &t.DelegatedToProjectIdent, &t.DelegatedToTaskID, &origin, &t.Goal, &routing, &requester, &parent, &t.ReviewAction, &t.TaskType, &changeset, &completion, &t.DelegationReady, &t.WaitingOnUser)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Labels = []string{}
	if labels != nil {
		if err := json.Unmarshal([]byte(*labels), &t.Labels); err != nil {
			t.Labels = []string{}
		}
	}
	if t.Labels == nil {
		t.Labels = []string{}
	}
	for _, field := range []struct {
		raw   *string
		value any
	}{{origin, &t.OwnerOrigin}, {parent, &t.Parent}, {completion, &t.CompletionOwner}, {changeset, &t.Changeset}} {
		if field.raw != nil {
			if err := json.Unmarshal([]byte(*field.raw), field.value); err != nil {
				return nil, err
			}
		}
	}
	if routing == nil || !json.Valid([]byte(*routing)) {
		return nil, fmt.Errorf("invalid task routing")
	}
	t.Routing = json.RawMessage(*routing)
	if requester != nil {
		if !json.Valid([]byte(*requester)) {
			return nil, fmt.Errorf("invalid task requester")
		}
		t.Requester = json.RawMessage(*requester)
	}
	return &t, nil
}
func readTask(ctx context.Context, q queryer, project, id string) (*Task, error) {
	return scanTask(q.QueryRowContext(ctx, "SELECT "+taskSelect+" FROM tasks t WHERE t.id=? AND t.project_ident=?", id, projects.NormalizeIdent(project)))
}

func (s *Store) InsertTask(ctx context.Context, input TaskInsert, origin *AgentOrigin) (*Task, error) {
	if input.Requester != nil {
		normalized, err := NormalizeOrigin(&input.Requester.Origin)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid requester origin", ErrTaskDelegation)
		}
		requester := *input.Requester
		requester.Origin = *normalized
		input.Requester = &requester
	}
	var result *Task
	linkedConversation := false
	err := s.taskMutation(ctx, origin, func(tx *sql.Tx, actor *AgentOrigin) error {
		var err error
		result, err = insertTaskRecord(ctx, tx, input, actor, "general")
		if err != nil {
			return err
		}
		fence, fenced := ctx.Value(conversationFenceKey{}).(conversationFence)
		if !fenced || fence.project != result.ProjectIdent {
			return nil
		}
		// taskMutation already verified the current owner in this transaction.
		link, err := tx.ExecContext(ctx, `UPDATE amux_conversations SET task_id=? WHERE id=? AND project_ident=? AND task_id IS NULL`, result.ID, fence.conversation, fence.project)
		if err != nil {
			return err
		}
		affected, err := link.RowsAffected()
		linkedConversation = affected != 0
		return err
	})
	if err == nil && linkedConversation {
		s.wakeConversations()
	}
	return result, err
}

func insertTaskRecord(ctx context.Context, tx *sql.Tx, input TaskInsert, actor *AgentOrigin, taskType string) (*Task, error) {

	input.Project = projects.NormalizeIdent(input.Project)
	if strings.TrimSpace(input.Title) == "" {
		return nil, fmt.Errorf("title must be non-empty")
	}
	if _, err := taskplatform.RequiredOS(input.Labels); err != nil {
		return nil, err
	}
	goal, err := NormalizeTaskGoal(input.Goal)
	if err != nil {
		return nil, err
	}
	if input.RootReason != nil {
		if (*input.RootReason != "user-directed" && *input.RootReason != "principle") || input.ParentID != nil || input.Details == nil || strings.TrimSpace(*input.Details) == "" || hasLabel(input.Labels, "autoreview-finding") {
			return nil, fmt.Errorf("root_reason requires an explanatory specification and cannot accompany a parent or review finding")
		}
	}
	p, err := getProject(ctx, tx, input.Project)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrTaskNotFound
	}
	routingJSON, routedOwner, err := taskCreationRouting(ctx, tx, p, input.CanonicalRemote, input.Labels, input.Routing)
	if err != nil {
		return nil, err
	}
	var requesterJSON any
	if input.Requester != nil {
		if err := validateTaskRequester(ctx, tx, input.Project, input.Labels, input.Requester, actor); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(input.Requester)
		if err != nil {
			return nil, err
		}
		requesterJSON = string(encoded)
	}
	var parent *Task
	if input.ParentID != nil {
		parent, err = readTask(ctx, tx, input.Project, *input.ParentID)
		if err != nil {
			return nil, err
		}
		if parent == nil {
			return nil, fmt.Errorf("parent %w", ErrTaskNotFound)
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	var rank int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(rank),0)+1 FROM tasks WHERE project_ident=? AND status='todo'", input.Project).Scan(&rank); err != nil {
		return nil, err
	}
	var ownerID, ownerJSON any
	if routedOwner != nil {
		ownerID = routedOwner.SessionID
		encoded, err := json.Marshal(routedOwner)
		if err != nil {
			return nil, err
		}
		ownerJSON = string(encoded)
	} else if (input.Routing == nil || input.Routing.Mode == "auto") && actor != nil && taskplatform.Matches(input.Labels, actor.OS) {
		ownerID = actor.SessionID
		encoded, _ := json.Marshal(actor)
		ownerJSON = string(encoded)
	}
	var labels any
	if len(input.Labels) > 0 {
		encoded, _ := json.Marshal(input.Labels)
		labels = string(encoded)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tasks(id,project_ident,title,description,details,status,rank,labels,hostname,owner_agent_id,owner_origin_json,reporter,created_at,updated_at,task_type,goal,routing_json,requester_json) VALUES(?,?,?,?,?,'todo',?,?,?,?,?,?,?,?,?,?,?,?)`, id.String(), input.Project, input.Title, input.Description, input.Details, rank, labels, input.Hostname, ownerID, ownerJSON, input.Reporter, now, now, taskType, goal, string(routingJSON), requesterJSON)
	if err != nil {
		return nil, err
	}
	if parent != nil {
		if err := attachTaskParent(ctx, tx, parent, id.String()); err != nil {
			return nil, err
		}
	}
	if input.RootReason != nil {
		authorType := "user"
		if actor != nil {
			authorType = "agent"
		}
		if _, err := insertTaskComment(ctx, tx, id.String(), input.Reporter, authorType, "Root task created ("+*input.RootReason+").\n\n"+*input.Details, nil, actor); err != nil {
			return nil, err
		}
	}
	return readTask(ctx, tx, input.Project, id.String())
}

func hasLabel(labels []string, needle string) bool {
	for _, label := range labels {
		if label == needle {
			return true
		}
	}
	return false
}

// TransitionTask currently handles ordinary active claims and releases. Closing,
// parking, dispatch and delegated transitions require their full workflows.
type TaskTransitionPolicy struct {
	submissionReviewVerdict bool
	HumanRequest            bool
	RequireChangeset        bool
	ConfirmedNoChanges      bool
	CheckGoal               bool
	SelectedGoal            string
}

func (s *Store) TransitionTask(ctx context.Context, project, id, status string, origin *AgentOrigin) (*Task, error) {
	return s.ApplyTaskTransition(ctx, project, id, status, origin, TaskTransitionPolicy{})
}

// ApplyTaskTransition validates client queue context and confirmation in the
// same transaction as ownership and status, rather than using a stale pre-read.
func (s *Store) ApplyTaskTransition(ctx context.Context, project, id, status string, origin *AgentOrigin, policy TaskTransitionPolicy) (*Task, error) {
	if status != "todo" && status != "in_progress" {
		return nil, ErrTaskUnsupported
	}
	var result *Task
	err := s.taskMutation(ctx, origin, func(tx *sql.Tx, actor *AgentOrigin) error {
		var err error
		result, err = transitionActiveTask(ctx, tx, project, id, status, actor, policy)
		return err
	})
	return result, err
}

func transitionActiveTask(ctx context.Context, tx *sql.Tx, project, id, status string, actor *AgentOrigin, policy TaskTransitionPolicy) (*Task, error) {

	t, err := readTask(ctx, tx, project, id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, ErrTaskNotFound
	}
	if policy.HumanRequest && actor != nil {
		return nil, fmt.Errorf("%w: user controls require an ordinary human task request", ErrTaskConflict)
	}
	if t.Status != "todo" && !IsActiveTaskStatus(t.Status) {
		return nil, ErrTaskUnsupported
	}
	if err := ordinaryWorkflowTask(ctx, tx, t); err != nil {
		return nil, err
	}
	if status == "in_progress" && !IsActiveTaskStatus(t.Status) && t.Kind == "delegated" && actor != nil {
		ready, err := taskDelegationReady(ctx, tx, t.ProjectIdent, t.ID)
		if err != nil {
			return nil, err
		}
		if !ready {
			return nil, fmt.Errorf("%w: delegated target must be complete before source inspection", ErrTaskConflict)
		}
	}

	enforceOwner := !policy.HumanRequest || status == "in_progress"
	if enforceOwner && t.OwnerOrigin == nil && (t.OwnerAgentID != nil || IsActiveTaskStatus(t.Status)) {
		return nil, ErrTaskOwnership
	}
	if enforceOwner && t.OwnerOrigin != nil && (actor == nil || actor.SessionID != t.OwnerOrigin.SessionID || actor.InstanceID != t.OwnerOrigin.InstanceID) {
		return nil, ErrTaskOwnership
	}
	if actor != nil && policy.CheckGoal && status == "in_progress" && t.Goal != nil && strings.TrimSpace(policy.SelectedGoal) != *t.Goal {
		return nil, fmt.Errorf("%w: select the task's goal with X-Task-Goal before claiming", ErrTaskConflict)
	}
	if actor != nil && policy.RequireChangeset && status == "todo" && (t.Status != "todo" || t.OwnerOrigin != nil) && !policy.ConfirmedNoChanges {
		return nil, ErrTaskChangeset
	}
	var ownerID, ownerJSON, started any
	if status == "in_progress" {
		if !IsActiveTaskStatus(t.Status) {
			if actor != nil && !taskplatform.Matches(t.Labels, actor.OS) {
				return nil, fmt.Errorf("%w: claiming agent OS does not match the task os: label", ErrTaskConflict)
			}
			if actor == nil {
				required, _ := taskplatform.RequiredOS(t.Labels)
				if required != "" {
					return nil, fmt.Errorf("%w: claiming agent OS is unknown", ErrTaskConflict)
				}
			}
			if err := ensureReviewer(ctx, tx, t, actor); err != nil {
				return nil, err
			}
			started = time.Now().UnixMilli()
			if actor != nil {
				ownerID = actor.SessionID
				encoded, _ := json.Marshal(actor)
				ownerJSON = string(encoded)
			}
		} else {
			started = t.StartedAt
			ownerID = t.OwnerAgentID
			if t.OwnerOrigin != nil {
				encoded, _ := json.Marshal(t.OwnerOrigin)
				ownerJSON = string(encoded)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status=?,owner_agent_id=?,owner_origin_json=?,started_at=?,updated_at=? WHERE id=? AND project_ident=?", status, ownerID, ownerJSON, started, time.Now().UnixMilli(), id, t.ProjectIdent); err != nil {
		return nil, err
	}
	// Imported release triggers predate stalled. Preserve those triggers while
	// applying the same exact-execution guard to this new active state.
	if status == "todo" && t.Status == "stalled" && actor != nil {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO assignment_releases(task_id,session_id,instance_id) VALUES(?,?,?)", t.ID, actor.SessionID, actor.InstanceID); err != nil {
			return nil, err
		}
	}
	updated, err := readTask(ctx, tx, project, id)
	if err != nil {
		return nil, err
	}
	if !IsActiveTaskStatus(t.Status) && status == "in_progress" && t.Kind == "delegated" {
		if err := recordDelegationInspection(ctx, tx, t, actor); err != nil {
			return nil, err
		}
	}
	return updated, nil
}

func (s *Store) Task(ctx context.Context, project, id string) (*TaskDetail, error) {
	return s.TaskRecent(ctx, project, id, nil)
}

func (s *Store) TaskRecent(ctx context.Context, project, id string, limit *uint64) (*TaskDetail, error) {
	// A read transaction keeps the task and its comments in one consistent view.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	t, err := readTask(ctx, tx, project, id)
	if err != nil || t == nil {
		return nil, err
	}
	comments, err := readTaskComments(ctx, tx, id, limit)
	if err != nil {
		return nil, err
	}
	var count int64
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_comments WHERE task_id=?", id).Scan(&count); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &TaskDetail{Task: *t, Comments: comments, CommentCount: count}, nil
}
func readTaskComments(ctx context.Context, q queryer, id string, limit *uint64) ([]TaskComment, error) {
	count := int64(-1)
	if limit != nil {
		if *limit > uint64(1<<63-1) {
			count = 1<<63 - 1
		} else {
			count = int64(*limit)
		}
	}
	rows, err := q.QueryContext(ctx, "SELECT id,task_id,author,author_type,content,created_at,hostname,origin_json FROM task_comments WHERE task_id=? ORDER BY created_at DESC,rowid DESC LIMIT ?", id, count)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []TaskComment{}
	for rows.Next() {
		var c TaskComment
		var raw *string
		if err := rows.Scan(&c.ID, &c.TaskID, &c.Author, &c.AuthorType, &c.Content, &c.CreatedAt, &c.Hostname, &raw); err != nil {
			return nil, err
		}
		if raw != nil {
			if err := json.Unmarshal([]byte(*raw), &c.Origin); err != nil {
				return nil, err
			}
		}
		if limit != nil {
			c.Content = capTaskComment(c.Content)
		}
		result = append(result, c)
	}
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result, rows.Err()
}
func capTaskComment(content string) string {
	const limit = 65536
	const marker = "\n…[truncated]"
	if len(content) <= limit {
		return content
	}
	end := limit - len(marker)
	for end > 0 && !utf8.RuneStart(content[end]) {
		end--
	}
	return content[:end] + marker
}
func insertTaskComment(ctx context.Context, q queryer, id, author, authorType, content string, hostname *string, origin *AgentOrigin) (*TaskComment, error) {
	if authorType != "agent" && authorType != "user" && authorType != "system" {
		return nil, fmt.Errorf("invalid author_type")
	}
	if hostname != nil {
		clean := strings.TrimSpace(*hostname)
		hostname = &clean
		if clean == "" {
			hostname = nil
		} else if len(clean) > 255 || strings.IndexFunc(clean, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("invalid comment hostname")
		}
	}
	commentID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	content = capTaskComment(content)
	var raw any
	if origin != nil {
		encoded, _ := json.Marshal(origin)
		raw = string(encoded)
	}
	if _, err := q.ExecContext(ctx, "INSERT INTO task_comments(id,task_id,author,author_type,content,created_at,hostname,origin_json) VALUES(?,?,?,?,?,?,?,?)", commentID.String(), id, author, authorType, content, now, hostname, raw); err != nil {
		return nil, err
	}
	if _, err := q.ExecContext(ctx, "UPDATE tasks SET updated_at=? WHERE id=?", now, id); err != nil {
		return nil, err
	}
	if authorType == "agent" {
		if _, err := resumeStalledTask(ctx, q, id, origin); err != nil {
			return nil, err
		}
	}
	return &TaskComment{ID: commentID.String(), TaskID: id, Author: author, AuthorType: authorType, Content: content, CreatedAt: now, Hostname: hostname, Origin: origin}, nil
}
func (s *Store) AddTaskComment(ctx context.Context, project, id, author, authorType, content string, hostname *string, origin *AgentOrigin) (*TaskComment, error) {
	var result *TaskComment
	err := s.taskMutation(ctx, origin, func(tx *sql.Tx, actor *AgentOrigin) error {
		t, err := readTask(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if t == nil {
			return ErrTaskNotFound
		}
		if t.Status == "user_review" && authorType == "user" && actor == nil && strings.TrimSpace(content) != "" {
			if err := ordinaryWorkflowTask(ctx, tx, t); err == nil {
				if err := resumeTaskOnUserComment(ctx, tx, t); err != nil {
					return err
				}
			} else if !errors.Is(err, ErrTaskUnsupported) {
				return err
			}
		}
		result, err = insertTaskComment(ctx, tx, id, author, authorType, content, hostname, actor)
		if err != nil || authorType == "system" {
			return err
		}
		if authorType == "user" {
			if err := answerTaskQuestions(ctx, tx, id, result.ID, result.CreatedAt); err != nil {
				return err
			}
		}
		if delegation, err := delegationBySource(ctx, tx, project, id); err != nil {
			return err
		} else if delegation != nil {
			body := fmt.Sprintf("Sender project `%s`, ticket `%s`:\n\n%s: %s", delegation.SourceProjectIdent, delegation.SourceTaskID, author, content)
			if _, err := insertTaskComment(ctx, tx, delegation.TargetTaskID, "agent-gateway", "system", capTaskComment(body), nil, actor); err != nil {
				return err
			}
		}
		if delegation, err := delegationByTarget(ctx, tx, project, id); err != nil {
			return err
		} else if delegation != nil {
			body := fmt.Sprintf("New comment on delegated task `%s` from project `%s`:\n\n%s: %s", delegation.SourceTaskID, delegation.TargetProjectIdent, author, content)
			if _, err := insertDelegationNudge(ctx, tx, delegation.SourceProjectIdent, "Delegated task comment added", body); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}
