package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/taskplatform"
)

var routerNotifierKinds = map[string]string{
	"task.created":     "task_created",
	"task.commented":   "task_commented",
	"task.user_review": "task_user_review",
	"task.completed":   "task_completed",
}

func migrateRouterNotifier(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS notifier_processed (
		event_seq INTEGER PRIMARY KEY REFERENCES router_events(seq) ON DELETE CASCADE
	);
	INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(22);`); err != nil {
		return fmt.Errorf("create router notifier receipt table: %w", err)
	}
	return nil
}

// ReconcileRouterNotifiers stores task notices and their cause receipt as one
// transaction. Recipients always come from the current, TTL-filtered snapshot.
func (s *Store) ReconcileRouterNotifiers(ctx context.Context, registry *fism.Registry, event fism.Event) error {
	if _, ok := routerNotifierKinds[event.ID]; !ok || event.Version != 1 || event.Seq <= 0 || !canonicalUUID(event.UID) {
		return fmt.Errorf("notifier reconciliation requires a supported persisted task v1 event with a canonical UID")
	}
	if registry == nil {
		return fmt.Errorf("notifier reconciliation requires a router registry")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	enabled, err := pluginEnabled(ctx, tx, "notifier")
	if err != nil {
		return err
	}
	if !enabled {
		return tx.Commit()
	}
	result, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO notifier_processed(event_seq) VALUES(?)", event.Seq)
	if err != nil {
		return fmt.Errorf("record notifier event receipt: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect notifier event receipt: %w", err)
	}
	if inserted == 0 {
		return tx.Commit()
	}
	stored, err := s.announceRouterTask(ctx, tx, registry, event)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if stored {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	}
	return nil
}

func (s *Store) announceRouterTask(ctx context.Context, tx *sql.Tx, registry *fism.Registry, event fism.Event) (bool, error) {
	var payload struct {
		Project string          `json:"project"`
		Task    json.RawMessage `json:"task"`
		Comment json.RawMessage `json:"comment"`
		Actor   json.RawMessage `json:"actor"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return false, fmt.Errorf("decode notifier event payload: %w", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &object); err != nil || object == nil || payload.Project == "" || len(payload.Task) == 0 || !json.Valid(payload.Task) {
		return false, fmt.Errorf("decode notifier event payload: expected project and task snapshot")
	}
	if len(payload.Comment) == 0 {
		payload.Comment = json.RawMessage("null")
	}
	if len(payload.Actor) == 0 {
		payload.Actor = json.RawMessage("null")
	}
	agents, _, err := livePresenceSnapshot(ctx, tx, time.Now().UTC().UnixMilli())
	if err != nil {
		return false, fmt.Errorf("read live notifier presence: %w", err)
	}
	kind := routerNotifierKinds[event.ID]
	stored := false
	for _, presence := range agents {
		if presence.ProjectIdent != payload.Project {
			continue
		}
		agent := typedAgentPresence(presence)
		if !routerNotifierRecipient(kind, payload.Task, payload.Actor, agent) {
			continue
		}
		newTask := routerNotifierNewTask(event.ID, payload.Task)
		requesterResult := routerNotifierRequesterResult(kind, payload.Task, agent)
		heading, instructions := delegatedInspectionGuidance(payload.Task)
		if kind == "task_user_review" {
			heading = "Task ready for user review"
			instructions = "The task is waiting for a user decision. Read its review action and submitted result."
		}
		if kind == "task_commented" && !requesterResult && userComment(payload.Comment) && routerNotifierTaskOwner(payload.Task, agent) {
			const reply = "The user wrote this comment on the task and reads replies there, not in this conversation. Act on it, then answer with task_comment on this task."
			if heading == "" {
				heading = "User commented on task"
				instructions = reply
			} else {
				instructions += " " + reply
			}
		}
		notificationEvent := struct {
			Kind      string          `json:"kind"`
			Project   string          `json:"project_ident"`
			CreatedAt int64           `json:"created_at"`
			Task      json.RawMessage `json:"task"`
			Comment   json.RawMessage `json:"comment"`
			Origin    json.RawMessage `json:"origin"`
		}{Kind: kind, Project: payload.Project, CreatedAt: event.EmittedAt, Task: payload.Task, Comment: payload.Comment, Origin: payload.Actor}
		receiver := map[string]string{
			"session_id": presence.Origin.SessionID, "instance_id": presence.Origin.InstanceID,
			"project": payload.Project, "provider": presence.Origin.Provider, "os": presence.Origin.OS,
		}
		if presence.Generation != nil {
			receiver["generation"] = *presence.Generation
		}
		notice, err := json.Marshal(map[string]any{
			"target": presence.Origin.SessionID, "project": payload.Project, "mode": "recipient",
			"receiver":     receiver,
			"require_idle": requesterResult,
			"new_task":     newTask, "heading": heading, "instructions": instructions,
			"event": notificationEvent,
		})
		if err != nil {
			return false, fmt.Errorf("encode notifier payload: %w", err)
		}
		emitted, err := emitRouterNotificationIn(ctx, tx, registry, "notifier", event.UID, notice)
		if err != nil {
			return false, fmt.Errorf("emit task notification: %w", err)
		}
		stored = stored || emitted.Stored
	}
	return stored, nil
}

func routerNotifierTaskOwner(raw json.RawMessage, agent notifierAgent) bool {
	var task map[string]json.RawMessage
	return json.Unmarshal(raw, &task) == nil && routerNotifierOwner(task, agent)
}

func userComment(raw json.RawMessage) bool {
	var comment struct {
		AuthorType string `json:"author_type"`
	}
	return json.Unmarshal(raw, &comment) == nil && comment.AuthorType == "user"
}

func routerNotifierNewTask(eventID string, task json.RawMessage) bool {
	if eventID != "task.created" {
		return false
	}
	if delegatedTaskNotReady(task) {
		return false
	}
	labels, err := taskplatform.ParseLabels(task)
	return err == nil && !hasLabel(labels, "task-review")
}

func delegatedTaskNotReady(raw json.RawMessage) bool {
	var task struct {
		Kind            string `json:"kind"`
		DelegationReady bool   `json:"delegation_ready"`
	}
	return json.Unmarshal(raw, &task) != nil || task.Kind == "delegated" && !task.DelegationReady
}

func delegatedInspectionGuidance(raw json.RawMessage) (string, string) {
	if delegatedTaskNotReady(raw) {
		return "", ""
	}
	var task struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(raw, &task) != nil || task.Kind != "delegated" {
		return "", ""
	}
	return "Inspect delegated completion", "The delegated project has completed this ticket. Inspect its linked target task, comments and acceptance evidence. This sender-side ticket remains open until inspection confirms completion. Comment with the result and close this ticket only after confirmation. If work is incomplete, comment with concrete clarification or request User review for a specific unresolved decision. Do not repeat completed work."
}

type notifierAgent struct {
	SessionID  string
	InstanceID string
	AgentID    *string
	OS         string
	Generation *string
	Goal       *string
}

func typedAgentPresence(presence AgentPresence) notifierAgent {
	return notifierAgent{SessionID: presence.Origin.SessionID, InstanceID: presence.Origin.InstanceID,
		AgentID: presence.AgentID, OS: presence.Origin.OS, Generation: presence.Generation, Goal: presence.Goal}
}

func routerNotifierRecipient(kind string, taskRaw, actorRaw json.RawMessage, agent notifierAgent) bool {
	var task map[string]json.RawMessage
	if err := json.Unmarshal(taskRaw, &task); err != nil || task == nil || agent.SessionID == "" || agent.InstanceID == "" {
		return false
	}
	labels, err := taskplatform.ParseLabels(taskRaw)
	if err != nil || !taskplatform.Matches(labels, agent.OS) {
		return false
	}
	if goal := task["goal"]; len(goal) > 0 && !isJSONNull(goal) {
		var taskGoal string
		if json.Unmarshal(goal, &taskGoal) != nil || agent.Goal == nil || taskGoal != *agent.Goal {
			return false
		}
	}
	if sameNotifierOrigin(actorRaw, agent) {
		return false
	}
	if routerNotifierRequesterResult(kind, taskRaw, agent) {
		return true
	}
	owner, requester := task["owner_origin"], task["requester"]
	status, ownerAgentID, routing := task["status"], task["owner_agent_id"], task["routing"]
	var statusValue string
	_ = json.Unmarshal(status, &statusValue)
	ownerlessTodo := statusValue == "todo" && (len(owner) == 0 || isJSONNull(owner)) && (len(ownerAgentID) == 0 || isJSONNull(ownerAgentID))
	created := kind == "task_created"
	routingMode := notifierRoutingMode(routing)
	broadcastCreation := created && routingMode == "broadcast"
	unassignedCreation := created && routingMode == "unassigned"
	if (isNotifierResult(kind) && len(requester) > 0 && !isJSONNull(requester) && (len(owner) == 0 || isJSONNull(owner))) ||
		statusValue == "user_review" || unassignedCreation || (ownerlessTodo && !broadcastCreation) {
		return false
	}
	if kind == "task_created" && notifierRoutingMode(routing) == "session" {
		var target struct {
			Origin     json.RawMessage `json:"origin"`
			Generation *string         `json:"generation"`
		}
		var routingObject struct {
			Target json.RawMessage `json:"target"`
		}
		_ = json.Unmarshal(routing, &routingObject)
		_ = json.Unmarshal(routingObject.Target, &target)
		if !sameNotifierOrigin(target.Origin, agent) || !sameOptionalString(target.Generation, agent.Generation) {
			return false
		}
	}
	if (len(owner) == 0 || isJSONNull(owner)) && (len(ownerAgentID) == 0 || isJSONNull(ownerAgentID)) {
		return true
	}
	return routerNotifierOwner(task, agent)
}

// routerNotifierOwner reports whether agent is the task's recorded owner,
// by typed owner origin or legacy owner agent ID. Ownerless tasks match no one.
func routerNotifierOwner(task map[string]json.RawMessage, agent notifierAgent) bool {
	if owner := task["owner_origin"]; len(owner) > 0 && !isJSONNull(owner) {
		return sameNotifierOrigin(owner, agent)
	}
	var legacyID string
	if ownerAgentID := task["owner_agent_id"]; len(ownerAgentID) == 0 || json.Unmarshal(ownerAgentID, &legacyID) != nil || legacyID == "" {
		return false
	}
	return (agent.AgentID != nil && legacyID == *agent.AgentID) || legacyID == agent.SessionID
}

func routerNotifierRequesterResult(kind string, taskRaw json.RawMessage, agent notifierAgent) bool {
	if !isNotifierResult(kind) {
		return false
	}
	var task struct {
		OwnerOrigin json.RawMessage `json:"owner_origin"`
		Requester   json.RawMessage `json:"requester"`
	}
	if json.Unmarshal(taskRaw, &task) != nil || sameNotifierOrigin(task.OwnerOrigin, agent) || len(task.Requester) == 0 || isJSONNull(task.Requester) {
		return false
	}
	var requester struct {
		Origin     json.RawMessage `json:"origin"`
		Generation *string         `json:"generation"`
	}
	if json.Unmarshal(task.Requester, &requester) != nil {
		return false
	}
	return sameNotifierOrigin(requester.Origin, agent) && sameOptionalString(requester.Generation, agent.Generation)
}

func isNotifierResult(kind string) bool {
	return kind == "task_commented" || kind == "task_user_review" || kind == "task_completed"
}

func notifierRoutingMode(raw json.RawMessage) string {
	var routing struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal(raw, &routing)
	return routing.Mode
}

func sameNotifierOrigin(raw json.RawMessage, agent notifierAgent) bool {
	if len(raw) == 0 || isJSONNull(raw) {
		return false
	}
	var origin struct {
		SessionID  string `json:"session_id"`
		InstanceID string `json:"instance_id"`
	}
	return json.Unmarshal(raw, &origin) == nil && origin.SessionID != "" && origin.SessionID == agent.SessionID && origin.InstanceID == agent.InstanceID
}
