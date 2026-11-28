package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/identity/agent"
	projectid "github.com/nitecon/amux/identity/project"
)

// WorkerAssignment binds the server task to its durable native worker run.
type WorkerAssignment struct {
	RunID        string       `json:"run_id"`
	Task         Task         `json:"task"`
	Conversation Conversation `json:"conversation"`
}

func migrateAssignmentAdoption(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS amux_assignment_runs(
 run_id TEXT PRIMARY KEY,task_id TEXT NOT NULL REFERENCES tasks(id),session_id TEXT NOT NULL,
 conversation_id TEXT NOT NULL UNIQUE REFERENCES amux_conversations(id),prompt_id TEXT NOT NULL,
 UNIQUE(task_id,session_id));
 DROP TRIGGER IF EXISTS amux_assignment_run_claim_changed;
 CREATE TRIGGER amux_assignment_run_claim_changed AFTER UPDATE OF status ON tasks
 WHEN NEW.status IS NOT OLD.status AND NEW.status NOT IN ('in_progress','stalled') BEGIN
 DELETE FROM amux_assignment_runs WHERE task_id=NEW.id;
 END;
 INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(10);`)
	return err
}

// AdoptWorkerAssignment returns the durable native run for the exact task
// owner. Task, conversation, mapping and first prompt commit as one unit.
func (s *Store) AdoptWorkerAssignment(ctx context.Context, project string, origin *agent.Origin) (*WorkerAssignment, error) {
	return s.AdoptWorkerAssignmentWithGoal(ctx, project, origin, nil)
}

// AdoptWorkerAssignmentWithGoal adopts the exact task owned by origin after
// validating that a tagged task is selected in the same goal scope.
func (s *Store) AdoptWorkerAssignmentWithGoal(ctx context.Context, project string, origin *agent.Origin, goal *string) (*WorkerAssignment, error) {
	return s.AdoptWorkerTask(ctx, project, "", origin, goal, "")
}

// AdoptWorkerTask fences streamed adoption to the offered task and exact owner.
// principal is the server-authenticated enrolled client recorded on a newly
// created conversation (see CreateConversationWithPrincipal).
func (s *Store) AdoptWorkerTask(ctx context.Context, project, taskID string, origin *agent.Origin, goal *string, principal string) (*WorkerAssignment, error) {
	selectedGoal, err := NormalizeTaskGoal(goal)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid selected goal", ErrTaskConflict)
	}
	actor, err := agent.NormalizeOrigin(origin)
	if err != nil {
		return nil, err
	}
	if actor == nil {
		return nil, ErrTaskOwnership
	}
	project = projectid.NormalizeIdent(project)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var active bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM projects WHERE ident=? AND archived_at IS NULL)", project).Scan(&active); err != nil {
		return nil, err
	}
	if !active {
		return nil, fmt.Errorf("%w: project is missing or archived", ErrTaskNotFound)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM tasks WHERE project_ident=? AND status IN ('todo','in_progress','stalled') AND owner_origin_json IS NOT NULL AND (id=? OR ?='') ORDER BY id`, project, taskID, taskID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	var task *Task
	for _, id := range ids {
		candidate, err := readTask(ctx, tx, project, id)
		if err != nil {
			return nil, err
		}
		if candidate == nil || candidate.OwnerOrigin == nil {
			continue
		}
		owner, err := agent.NormalizeOrigin(candidate.OwnerOrigin)
		if err != nil {
			return nil, err
		}
		if owner == nil || *owner != *actor {
			continue
		}
		if task != nil {
			return nil, ErrConversationConflict
		}
		task = candidate
	}
	if task == nil {
		return nil, nil
	}
	if task.Goal != nil && !sameOptionalString(task.Goal, selectedGoal) {
		return nil, fmt.Errorf("%w: select the task's goal before adopting its worker run", ErrTaskConflict)
	}
	var runID, conversationID, promptID string
	err = tx.QueryRowContext(ctx, `SELECT run_id,conversation_id,prompt_id FROM amux_assignment_runs WHERE task_id=? AND session_id=?`, task.ID, actor.SessionID).Scan(&runID, &conversationID, &promptID)
	if err == nil {
		c, err := readConversation(ctx, tx, project, conversationID)
		if err != nil {
			return nil, err
		}
		if c.Owner != *actor {
			return nil, ErrTaskOwnership
		}
		var bound string
		if err := tx.QueryRowContext(ctx, "SELECT principal FROM amux_conversations WHERE id=?", conversationID).Scan(&bound); err != nil {
			return nil, err
		}
		if bound != "" && bound != principal {
			return nil, ErrTaskOwnership
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return &WorkerAssignment{RunID: runID, Task: *task, Conversation: *c}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	runID, conversationID, promptID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UnixMilli()
	owner, err := json.Marshal(actor)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO amux_conversations(id,project_ident,task_id,provider,owner_json,created_at,principal) VALUES(?,?,?,?,?,?,?)`, conversationID, project, task.ID, actor.Provider, string(owner), now, principal); err != nil {
		return nil, err
	}
	input := conversation.Command{Kind: "prompt", Text: AssignmentPrompt(task)}
	if err = validateConversationCommand(input); err != nil {
		return nil, err
	}
	commandJSON, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if len(commandJSON) > 1<<20 {
		return nil, fmt.Errorf("%w: encoded command exceeds 1 MiB", ErrConversationInput)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO amux_conversation_commands(conversation_id,id,sequence,kind,request_json,request_id,target_instance_id,state,created_at,updated_at) VALUES(?,?,1,'prompt',?,'',?,'queued',?,?)`, conversationID, promptID, string(commandJSON), actor.InstanceID, now, now); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO amux_assignment_runs(run_id,task_id,session_id,conversation_id,prompt_id) VALUES(?,?,?,?,?)`, runID, task.ID, actor.SessionID, conversationID, promptID); err != nil {
		return nil, err
	}
	if err = captureNativeDeliveryAdoptionIn(ctx, tx, task, *actor, runID, conversationID, promptID); err != nil {
		return nil, err
	}
	started := task.Status == "todo"
	if started {
		if _, err := tx.ExecContext(ctx, `UPDATE task_mutation_context SET origin_json=? WHERE id=1`, string(owner)); err != nil {
			return nil, err
		}
		task, err = transitionActiveTask(ctx, tx, project, task.ID, "in_progress", actor, TaskTransitionPolicy{})
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE task_mutation_context SET origin_json=NULL WHERE id=1`); err != nil {
			return nil, err
		}
	}
	c, err := readConversation(ctx, tx, project, conversationID)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.wakeConversations()
	if started {
		s.wakeTaskEvents()
	}
	return &WorkerAssignment{RunID: runID, Task: *task, Conversation: *c}, nil
}

const AssignmentMCPIntro = "Use the configured amux MCP server for task work."

// AssignmentPrompt describes the work assigned to a task worker.
func AssignmentPrompt(task *Task) string {
	var prompt strings.Builder
	prompt.WriteString(AssignmentMCPIntro + " Start with task_get to read this task and its comments. Use agent_identity for your current name, hostname, and provider. For a handoff, use task_agents to find the recipient and task_reassign to transfer this task. If amux MCP is unavailable, report that in this conversation.\n\n")
	if task.Kind == "delegated" && task.DelegationReady {
		fmt.Fprintf(&prompt, "[Amux delegated completion inspection]\nTask ID: %s\nTitle: %s\nTarget project: %s\nTarget task ID: %s\n\nThe delegated project has completed the linked target task. Inspect that target task, its comments, and acceptance evidence before deciding whether this source tracking task is complete. Keep this source task open until inspection confirms completion. Do not redo the delegated implementation. If it is incomplete, comment with concrete clarification or request the specific user decision still needed.\n", task.ID, task.Title, valueOrEmpty(task.DelegatedToProjectIdent), valueOrEmpty(task.DelegatedToTaskID))
	} else {
		fmt.Fprintf(&prompt, "Please evaluate this task and perform it, or ask the user for what you need.\n\nTask ID: %s\nTitle: %s", task.ID, task.Title)
		if !hasLabel(task.Labels, "task-review") {
			prompt.WriteString("\nRead the task and its latest comments before starting. If the work changes files, publish it with submit_work so a reviewer can inspect the changes. For a completed task with no file changes, use task_done with changeset null.")
		}
	}
	if task.Description != nil && *task.Description != "" {
		fmt.Fprintf(&prompt, "\n\nDescription:\n%s", *task.Description)
	}
	if task.Details != nil && *task.Details != "" {
		fmt.Fprintf(&prompt, "\n\nDetails:\n%s", *task.Details)
	}
	return prompt.String()
}
