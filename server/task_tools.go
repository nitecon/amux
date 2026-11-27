package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nitecon/amux/mcpstdio"
	"github.com/nitecon/amux/storage"
)

const taskToolCreateDescription = "Create a subtask under a fitting existing parent; read the parent first and check scope. Create a root (root_reason instead of parent) only for an explicit user request (user-directed) or a core-principle finding (principle), with a specification recording the request or finding, evidence and acceptance criteria. Otherwise, if no parent fits, ask the user to create the top-level task and propose its title and description."

var agentTaskTools = []mcpstdio.Tool{
	{Name: "task_get", Description: "Show a task with its specification and comments.", InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"Task ID"}},"required":["id"],"additionalProperties":false}`)},
	{Name: "project_message_thread", Description: "Read a full project message thread, including the original message and replies.", InputSchema: schema(`{"type":"object","properties":{"id":{"type":"integer","minimum":1,"description":"Project message ID"}},"required":["id"],"additionalProperties":false}`)},
	{Name: "task_list", Description: "List this project's tasks (default: todo, in_progress and stalled).", InputSchema: schema(`{"type":"object","properties":{"status":{"type":"string","description":"Comma-separated statuses: todo, in_progress, stalled, user_review, done, cancelled"}},"additionalProperties":false}`)},
	{Name: "task_create", Description: taskToolCreateDescription, InputSchema: schema(`{"type":"object","properties":{"title":{"type":"string","description":"Task title"},"parent":{"type":"string","description":"Existing parent task UUID whose scope includes this work"},"root_reason":{"type":"string","enum":["user-directed","principle"],"description":"Create a root instead of a subtask; requires specification and cannot accompany parent"},"specification":{"type":"string","description":"Context and acceptance criteria"},"notify_results":{"type":"boolean","description":"Subscribe this exact execution to task results"}},"required":["title"],"additionalProperties":false}`)},
	{Name: "task_delegate", Description: "Delegate cross-project work under a fitting source parent. The parent defaults to this conversation's task; inspect both projects before retrying an uncertain delegation.", InputSchema: schema(`{"type":"object","properties":{"title":{"type":"string","description":"Handoff title"},"target_project":{"type":"string","description":"Owning target project identifier"},"parent":{"type":"string","description":"Source parent UUID; defaults to this conversation's task"},"specification":{"type":"string","description":"Context and acceptance criteria"},"notify_results":{"type":"boolean","description":"Subscribe this exact execution to delegation results"}},"required":["title","target_project"],"additionalProperties":false}`)},
	{Name: "task_update_delegation", Description: "Update the shared metadata contract for a delegated task. The request is fenced to the current native conversation, and storage enforces task-owner checks for OS changes.", InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"Source delegated task ID"},"title":{"type":["string","null"]},"description":{"type":["string","null"]},"specification":{"type":["string","null"],"description":"Shared specification (takes precedence over details)"},"details":{"type":["string","null"],"description":"Alias for specification"},"labels":{"type":["array","null"],"items":{"type":"string"},"description":"Shared labels; an empty array clears them"}},"required":["id"],"additionalProperties":false}`)},
	{Name: "task_set_requester", Description: "Subscribe this exact execution to task comments and completion results, or clear the subscription.", InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"Task ID"},"clear":{"type":"boolean","description":"Clear this task's requester subscription"}},"required":["id"],"additionalProperties":false}`)},
	{Name: "task_claim", Description: "Claim a task before working on it.", InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"Task ID"}},"required":["id"],"additionalProperties":false}`)},
	{Name: "task_started", Description: "Report that work on a claimed task has started.", InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"Task ID"}},"required":["id"],"additionalProperties":false}`)},
	{Name: "task_comment", Description: "Add a progress, evidence or question comment to a task.", InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"Task ID"},"body":{"type":"string","description":"Comment text"}},"required":["id","body"],"additionalProperties":false}`)},
	{Name: "task_release", Description: "Release an owned task you cannot continue.", InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"Task ID"},"changeset":{"description":"Every file mutated by this task, or null if no files were mutated.","oneOf":[{"type":"array","items":{"type":"string"}},{"type":"null"}]}},"required":["id"],"additionalProperties":false}`)},
	{Name: "task_user_review", Description: "Park an owned task for a user decision. Supply the specific action the user must take.", InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"Task ID"},"action":{"type":"string","description":"Specific decision or action required from the user"},"changeset":{"description":"Every file mutated by this task, or null if no files were mutated.","oneOf":[{"type":"array","items":{"type":"string"}},{"type":"null"}]}},"required":["id","action"],"additionalProperties":false}`)},
	{Name: "task_done", Description: "Complete a task regardless of its current status or owner. Confirm every mutated file (or null for none). Closing a delegated tracker does not complete unfinished target work.", InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"Task ID"},"changeset":{"description":"Every file mutated by this task, or null if no files were mutated.","oneOf":[{"type":"array","items":{"type":"string"}},{"type":"null"}]}},"required":["id"],"additionalProperties":false}`)},
	{Name: "task_finish_review", Description: "Finish the current submission review. Approve to close the parent, or request corrections from the implementer. Record unrelated actionable findings as separate queued tasks.", InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string"},"outcome":{"enum":["approved","changes_requested"]},"summary":{"type":"string"},"findings":{"type":"array","items":{"type":"object","properties":{"title":{"type":"string"},"details":{"type":"string"}},"required":["title","details"],"additionalProperties":false}}},"required":["id","outcome","summary"],"additionalProperties":false}`)},
	{Name: "task_confirm_mutation", Description: "Confirm this task mutated files after its User review/completion question. Returns the changeset submission instruction; does not move the task yet.", InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"Task ID"}},"required":["id"],"additionalProperties":false}`)},
	{Name: "task_set_changeset", Description: "Supply every mutated file (or null for no mutations) and finish the pending User review/completion transition. Follow task_confirm_mutation first when files changed.", InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"Task ID"},"changeset":{"description":"Every modified, added or deleted path, or null if no files were mutated.","oneOf":[{"type":"array","items":{"type":"string"}},{"type":"null"}]}},"required":["id","changeset"],"additionalProperties":false}`)},
}

func schema(raw string) json.RawMessage { return json.RawMessage(raw) }

// AgentTaskTools returns the native task tools supported by this server.
func AgentTaskTools() []mcpstdio.Tool {
	return cloneTaskTools(agentTaskTools)
}

var localConversationTaskTools = []mcpstdio.Tool{
	mustTaskTool("task_get"),
	mustTaskTool("project_message_thread"),
	mustTaskTool("task_list"),
	{
		Name:        "task_create",
		Description: taskToolCreateDescription,
		InputSchema: schema(`{"type":"object","properties":{"title":{"type":"string","description":"Task title"},"parent":{"type":"string","description":"Existing parent task UUID whose scope includes this work"},"root_reason":{"type":"string","enum":["user-directed","principle"],"description":"Create a root instead of a subtask; requires specification and cannot accompany parent"},"specification":{"type":"string","description":"Context and acceptance criteria"}},"required":["title"],"additionalProperties":false}`),
	},
	{
		Name:        "task_delegate",
		Description: "Delegate cross-project work under an explicit source parent task UUID; inspect both projects before retrying an uncertain delegation.",
		InputSchema: schema(`{"type":"object","properties":{"title":{"type":"string","description":"Handoff title"},"target_project":{"type":"string","description":"Owning target project identifier"},"parent":{"type":"string","description":"Existing source parent task UUID"},"specification":{"type":"string","description":"Context and acceptance criteria"}},"required":["title","target_project","parent"],"additionalProperties":false}`),
	},
	mustTaskTool("task_comment"),
}

// LocalConversationTaskTools returns the task tools supported by standalone
// local conversations that are not bound to a server conversation or task.
func LocalConversationTaskTools() []mcpstdio.Tool {
	return cloneTaskTools(localConversationTaskTools)
}

func cloneTaskTools(source []mcpstdio.Tool) []mcpstdio.Tool {
	tools := append([]mcpstdio.Tool(nil), source...)
	for i := range tools {
		tools[i].InputSchema = append(json.RawMessage(nil), tools[i].InputSchema...)
	}
	return tools
}

func mustTaskTool(name string) mcpstdio.Tool {
	tool, ok := taskToolByName(name)
	if !ok {
		panic("unknown task tool: " + name)
	}
	return tool
}

type taskToolRequest struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (a *api) callTaskTool(w http.ResponseWriter, r *http.Request) {
	actor, ok := requestActor(w, r)
	if !ok {
		return
	}
	if actor == nil {
		writeError(w, http.StatusBadRequest, "task tools require complete agent origin headers")
		return
	}
	project, conversationID := r.PathValue("ident"), r.PathValue("conversation")
	conversation, err := a.store.Conversation(r.Context(), project, conversationID)
	if err != nil {
		conversationError(w, err)
		return
	}
	if conversation.Owner != *actor {
		conversationError(w, storage.ErrTaskOwnership)
		return
	}
	var req *taskToolRequest
	if !readTaskRequest(w, r, &req) {
		return
	}
	if req == nil || req.Name == "" || len(req.Arguments) == 0 || !json.Valid(req.Arguments) {
		writeError(w, http.StatusBadRequest, "name and arguments are required")
		return
	}
	if _, found := taskToolByName(req.Name); !found {
		writeJSON(w, taskToolResult("Unknown task tool. Supported tools: task_get, project_message_thread, task_list, task_create, task_delegate, task_update_delegation, task_set_requester, task_claim, task_started, task_comment, task_release, task_user_review, task_done, task_finish_review, task_confirm_mutation, task_set_changeset.", true))
		return
	}
	selectedGoal, err := selectedTaskGoal(r.Header)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid task goal")
		return
	}
	ctx := storage.WithConversationFence(r.Context(), project, conversationID)
	value, message, err := a.executeTaskTool(ctx, project, actor, selectedGoal, conversation.TaskID, req.Name, req.Arguments)
	if err != nil {
		writeJSON(w, taskToolResult(safeTaskToolError(err), true))
		return
	}
	if message != "" {
		writeJSON(w, taskToolResult(message, false))
		return
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		writeJSON(w, taskToolResult("Task operation failed.", true))
		return
	}
	writeJSON(w, taskToolResult(string(encoded), false))
}

func selectedTaskGoal(header http.Header) (*string, error) {
	values := header.Values("X-Task-Goal")
	if len(values) == 0 {
		unscoped := ""
		return &unscoped, nil
	}
	if len(values) != 1 {
		return nil, fmt.Errorf("multiple selected task goals")
	}
	normalized, err := storage.NormalizeTaskGoal(&values[0])
	if err != nil {
		return nil, err
	}
	selected := ""
	if normalized != nil {
		selected = *normalized
	}
	return &selected, nil
}

func taskToolByName(name string) (mcpstdio.Tool, bool) {
	for _, tool := range agentTaskTools {
		if tool.Name == name {
			return tool, true
		}
	}
	return mcpstdio.Tool{}, false
}

func taskToolResult(text string, isError bool) mcpstdio.Result {
	return mcpstdio.Result{Content: []mcpstdio.TextContent{{Type: "text", Text: text}}, IsError: isError}
}

func decodeTaskToolArgs(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return nil, errors.New("invalid task tool arguments: arguments must be one object")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("invalid task tool arguments: arguments must be one object")
	}
	valid := map[string]bool{}
	for _, key := range allowed {
		valid[key] = true
	}
	for key := range fields {
		if !valid[key] {
			return nil, fmt.Errorf("invalid task tool arguments: unknown argument %q", key)
		}
	}
	return fields, nil
}

func decodeField(fields map[string]json.RawMessage, key string, target any, required bool) error {
	raw, ok := fields[key]
	if !ok {
		if required {
			return fmt.Errorf("invalid task tool arguments: %s is required", key)
		}
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("invalid task tool arguments: %s cannot be null", key)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("invalid task tool arguments: invalid %s", key)
	}
	return nil
}

func decodeDelegationUpdateFields(fields map[string]json.RawMessage, update *storage.TaskDelegationUpdate) error {
	raw, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("invalid task tool arguments: invalid delegation metadata")
	}
	if err := json.Unmarshal(raw, update); err != nil {
		return fmt.Errorf("invalid task tool arguments: invalid delegation metadata")
	}
	return nil
}

// TaskCreateArgs holds validated arguments for task_create.
type TaskCreateArgs struct {
	Title         string
	ParentID      *string
	RootReason    *string
	Specification *string
	NotifyResults bool
}

// TaskDelegateArgs holds validated arguments for task_delegate.
type TaskDelegateArgs struct {
	Title         string
	TargetProject string
	ParentID      *string
	Specification *string
	NotifyResults bool
}

// ParseTaskIDArg validates a single-field {"id": "..."} task tool argument object.
func ParseTaskIDArg(raw json.RawMessage) (string, error) {
	fields, err := decodeTaskToolArgs(raw, "id")
	if err != nil {
		return "", err
	}
	var id string
	if err := decodeField(fields, "id", &id, true); err != nil {
		return "", err
	}
	return id, nil
}

// ParseProjectMessageThreadArgs validates arguments for project_message_thread.
func ParseProjectMessageThreadArgs(raw json.RawMessage) (int64, error) {
	fields, err := decodeTaskToolArgs(raw, "id")
	if err != nil {
		return 0, err
	}
	var messageID int64
	if err := decodeField(fields, "id", &messageID, true); err != nil || messageID <= 0 {
		return 0, errors.New("invalid task tool arguments: id must be a positive integer")
	}
	return messageID, nil
}

// ParseTaskListArgs validates arguments for task_list and returns the requested statuses.
func ParseTaskListArgs(raw json.RawMessage) ([]string, error) {
	fields, err := decodeTaskToolArgs(raw, "status")
	if err != nil {
		return nil, err
	}
	statuses := []string{"todo", "in_progress", "stalled"}
	if _, ok := fields["status"]; ok {
		var status string
		if err := decodeField(fields, "status", &status, true); err != nil {
			return nil, errors.New("invalid task tool arguments: invalid status")
		}
		statuses = []string{}
		for _, part := range strings.Split(status, ",") {
			if part = strings.TrimSpace(part); part != "" {
				statuses = append(statuses, part)
			}
		}
	}
	for _, status := range statuses {
		if status != "todo" && status != "in_progress" && status != "stalled" && status != "user_review" && status != "done" && status != "cancelled" {
			return nil, errors.New("invalid task tool arguments: invalid task status")
		}
	}
	return statuses, nil
}

// ParseTaskCreateArgs validates arguments for task_create.
func ParseTaskCreateArgs(raw json.RawMessage, allowNotifyResults bool) (TaskCreateArgs, error) {
	allowed := []string{"title", "parent", "root_reason", "specification"}
	if allowNotifyResults {
		allowed = append(allowed, "notify_results")
	}
	fields, err := decodeTaskToolArgs(raw, allowed...)
	if err != nil {
		return TaskCreateArgs{}, err
	}
	var args TaskCreateArgs
	if err := decodeField(fields, "title", &args.Title, true); err != nil {
		return TaskCreateArgs{}, err
	}
	if err := decodeField(fields, "parent", &args.ParentID, false); err != nil {
		return TaskCreateArgs{}, err
	}
	if err := decodeField(fields, "root_reason", &args.RootReason, false); err != nil {
		return TaskCreateArgs{}, err
	}
	if err := decodeField(fields, "specification", &args.Specification, false); err != nil {
		return TaskCreateArgs{}, err
	}
	if allowNotifyResults {
		if err := decodeField(fields, "notify_results", &args.NotifyResults, false); err != nil {
			return TaskCreateArgs{}, err
		}
	}
	if args.ParentID == nil && args.RootReason == nil {
		return TaskCreateArgs{}, errors.New("invalid task tool arguments: agent task creation requires parent or root_reason with an explanatory specification")
	}
	if args.RootReason != nil && (args.ParentID != nil || args.Specification == nil || strings.TrimSpace(*args.Specification) == "") {
		return TaskCreateArgs{}, errors.New("invalid task tool arguments: root_reason requires specification and cannot accompany parent")
	}
	return args, nil
}

// ParseTaskDelegateArgs validates arguments for task_delegate.
func ParseTaskDelegateArgs(raw json.RawMessage, requireParent, allowNotifyResults bool) (TaskDelegateArgs, error) {
	allowed := []string{"title", "target_project", "parent", "specification"}
	if allowNotifyResults {
		allowed = append(allowed, "notify_results")
	}
	fields, err := decodeTaskToolArgs(raw, allowed...)
	if err != nil {
		return TaskDelegateArgs{}, err
	}
	var args TaskDelegateArgs
	if err := decodeField(fields, "title", &args.Title, true); err != nil {
		return TaskDelegateArgs{}, err
	}
	if err := decodeField(fields, "target_project", &args.TargetProject, true); err != nil {
		return TaskDelegateArgs{}, err
	}
	if requireParent {
		var parent string
		if err := decodeField(fields, "parent", &parent, true); err != nil {
			return TaskDelegateArgs{}, err
		}
		if strings.TrimSpace(parent) == "" {
			return TaskDelegateArgs{}, errors.New("invalid task tool arguments: parent is required")
		}
		args.ParentID = &parent
	} else if _, ok := fields["parent"]; ok {
		if err := decodeField(fields, "parent", &args.ParentID, true); err != nil {
			return TaskDelegateArgs{}, err
		}
	}
	if err := decodeField(fields, "specification", &args.Specification, false); err != nil {
		return TaskDelegateArgs{}, err
	}
	if allowNotifyResults {
		if err := decodeField(fields, "notify_results", &args.NotifyResults, false); err != nil {
			return TaskDelegateArgs{}, err
		}
	}
	return args, nil
}

// ParseTaskCommentArgs validates arguments for task_comment.
func ParseTaskCommentArgs(raw json.RawMessage) (string, string, error) {
	fields, err := decodeTaskToolArgs(raw, "id", "body")
	if err != nil {
		return "", "", err
	}
	var id, body string
	if err := decodeField(fields, "id", &id, true); err != nil {
		return "", "", err
	}
	if err := decodeField(fields, "body", &body, true); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(body) == "" || len(body) > 65536 {
		return "", "", errors.New("invalid task tool arguments: body must be non-empty and at most 65536 bytes")
	}
	return id, body, nil
}

func (a *api) executeTaskTool(ctx context.Context, project string, actor *storage.AgentOrigin, selectedGoal *string, conversationTaskID, name string, raw json.RawMessage) (any, string, error) {
	switch name {
	case "task_get":
		id, err := ParseTaskIDArg(raw)
		if err != nil {
			return nil, "", err
		}
		detail, e := a.store.Task(ctx, project, id)
		if e != nil {
			return nil, "", e
		}
		if detail == nil {
			return nil, "", storage.ErrTaskNotFound
		}
		return struct {
			storage.Task
			Specification *string               `json:"specification"`
			Comments      []storage.TaskComment `json:"comments"`
			Count         int64                 `json:"comment_count"`
		}{detail.Task, detail.Task.Details, detail.Comments, detail.CommentCount}, "", nil
	case "project_message_thread":
		messageID, err := ParseProjectMessageThreadArgs(raw)
		if err != nil {
			return nil, "", err
		}
		thread, e := a.store.GetProjectMessageThread(ctx, project, messageID)
		return thread, "", e
	case "task_list":
		statuses, err := ParseTaskListArgs(raw)
		if err != nil {
			return nil, "", err
		}
		value, e := a.store.Tasks(ctx, project, statuses, false)
		if e != nil || selectedGoal == nil {
			return value, "", e
		}
		filtered := value[:0]
		for _, task := range value {
			goal := ""
			if task.Goal != nil {
				goal = *task.Goal
			}
			if goal == *selectedGoal {
				filtered = append(filtered, task)
			}
		}
		value = filtered
		return value, "", e
	case "task_create":
		args, err := ParseTaskCreateArgs(raw, true)
		if err != nil {
			return nil, "", err
		}
		input := storage.TaskInsert{
			Project:    project,
			Title:      args.Title,
			Reporter:   actor.SessionID,
			Goal:       selectedGoal,
			ParentID:   args.ParentID,
			RootReason: args.RootReason,
			Details:    args.Specification,
		}
		if args.NotifyResults {
			input.Requester, err = currentRequester(ctx, a.store, project, actor)
			if err != nil {
				return nil, "", err
			}
		}
		task, e := a.store.InsertTask(ctx, input, actor)
		if e != nil {
			return nil, "", e
		}
		return struct {
			*storage.Task
			Specification *string `json:"specification"`
			Hint          string  `json:"hint"`
		}{task, task.Details, specificationHint}, "", nil
	case "task_delegate":
		args, err := ParseTaskDelegateArgs(raw, false, true)
		if err != nil {
			return nil, "", err
		}
		input := storage.TaskDelegationInput{
			SourceProject: project,
			Reporter:      actor.SessionID,
			Title:         args.Title,
			TargetProject: args.TargetProject,
			ParentID:      args.ParentID,
			Details:       args.Specification,
		}
		if input.ParentID == nil {
			input.ParentID = &conversationTaskID
		}
		if args.NotifyResults {
			input.Requester, err = currentRequester(ctx, a.store, project, actor)
			if err != nil {
				return nil, "", err
			}
		}
		result, e := a.store.DelegateTask(ctx, input, actor)
		return result, "", e
	case "task_comment":
		id, body, err := ParseTaskCommentArgs(raw)
		if err != nil {
			return nil, "", err
		}
		value, e := a.store.AddTaskComment(ctx, project, id, actor.SessionID, "agent", body, nil, actor)
		return value, "", e
	}
	allowed := map[string][]string{
		"task_update_delegation": {"id", "title", "description", "specification", "details", "labels"},
		"task_set_requester":     {"id", "clear"},
		"task_claim":             {"id"},
		"task_started":           {"id"},
		"task_release":           {"id", "changeset"},
		"task_user_review":       {"id", "action", "changeset"},
		"task_done":              {"id", "changeset"},
		"task_confirm_mutation":  {"id"},
		"task_set_changeset":     {"id", "changeset"},
		"task_finish_review":     {"id", "outcome", "summary", "findings"},
	}
	keys, ok := allowed[name]
	if !ok {
		return nil, "", errors.New("unsupported task tool")
	}
	fields, err := decodeTaskToolArgs(raw, keys...)
	if err != nil {
		return nil, "", err
	}
	var id string
	if err = decodeField(fields, "id", &id, true); err != nil {
		return nil, "", err
	}
	switch name {
	case "task_set_requester":
		clear := false
		if err = decodeField(fields, "clear", &clear, false); err != nil {
			return nil, "", err
		}
		var requester *storage.RegisteredTargetIdentity
		if !clear {
			requester, err = currentRequester(ctx, a.store, project, actor)
			if err != nil {
				return nil, "", err
			}
		}
		result, e := a.store.SetTaskRequester(ctx, project, id, requester, actor)
		return result, "", e
	case "task_update_delegation":
		var update storage.TaskDelegationUpdate
		if err = decodeDelegationUpdateFields(fields, &update); err != nil {
			return nil, "", err
		}
		if update.Title == nil && update.Description == nil && update.Details == nil && update.Specification == nil && update.Labels == nil {
			return nil, "", errors.New("invalid task tool arguments: provide at least one metadata field")
		}
		result, e := a.store.UpdateTaskDelegationContract(ctx, project, id, update, actor)
		return result, "", e
	case "task_claim":
		goal := ""
		if selectedGoal != nil {
			goal = *selectedGoal
		}
		value, e := a.store.ApplyTaskTransition(ctx, project, id, "in_progress", actor, storage.TaskTransitionPolicy{CheckGoal: true, SelectedGoal: goal})
		return value, "", e
	case "task_started":
		detail, e := a.store.Task(ctx, project, id)
		if e != nil {
			return nil, "", e
		}
		if detail == nil {
			return nil, "", storage.ErrTaskNotFound
		}
		if !storage.IsActiveTaskStatus(detail.Task.Status) || detail.Task.OwnerOrigin == nil || *detail.Task.OwnerOrigin != *actor {
			return nil, "", storage.ErrTaskOwnership
		}
		return detail.Task, "", nil
	case "task_finish_review":
		if conversationTaskID != id {
			return nil, "", storage.ErrTaskOwnership
		}
		var outcome, summary string
		var findings []storage.ReviewFinding
		if err := decodeField(fields, "outcome", &outcome, true); err != nil {
			return nil, "", err
		}
		if err := decodeField(fields, "summary", &summary, true); err != nil {
			return nil, "", err
		}
		if err := decodeField(fields, "findings", &findings, false); err != nil {
			return nil, "", err
		}
		value, err := a.store.FinishSubmissionReview(ctx, project, id, outcome, summary, findings, actor)
		return value, "", err
	case "task_release", "task_user_review", "task_done":
		status := map[string]string{"task_release": "todo", "task_user_review": "user_review", "task_done": "done"}[name]
		update := storage.TaskWorkflowUpdate{Status: status}
		if name == "task_user_review" {
			var action string
			if err = decodeField(fields, "action", &action, true); err != nil {
				return nil, "", err
			}
			update.ReviewAction = &action
		}
		if rawChanges, ok := fields["changeset"]; ok {
			update.Changeset = append(json.RawMessage(nil), rawChanges...)
		}
		value, e := a.store.UpdateTaskWorkflow(ctx, project, id, update, actor)
		return value, "", e
	case "task_confirm_mutation":
		taskType := "software"
		_, e := a.store.ConfirmTaskWorkflow(ctx, project, id, storage.TaskWorkflowConfirmation{TaskType: &taskType}, actor)
		if e != nil {
			return nil, "", e
		}
		return nil, fmt.Sprintf("Task mutation confirmed; the task has not moved. Submit every modified, added or deleted path using `amux set-changeset %s '[\"path/to/file\"]'` (MCP `task_set_changeset`).", id), nil
	case "task_set_changeset":
		rawChanges, ok := fields["changeset"]
		if !ok {
			return nil, "", errors.New("invalid task tool arguments: changeset is required (use null for no mutations)")
		}
		result, e := a.store.ConfirmTaskWorkflow(ctx, project, id, storage.TaskWorkflowConfirmation{Changeset: append(json.RawMessage(nil), rawChanges...)}, actor)
		if e != nil {
			return nil, "", e
		}
		return result.Task, "", nil
	default:
		return nil, "", errors.New("unsupported task tool")
	}
}

func safeTaskToolError(err error) string {
	if strings.HasPrefix(err.Error(), "invalid task tool arguments:") {
		return strings.TrimPrefix(err.Error(), "invalid task tool arguments: ")
	}
	switch {
	case errors.Is(err, storage.ErrTaskGuidedQuestion):
		return err.Error()
	case errors.Is(err, storage.ErrTaskChangeset):
		return changesetQuestion
	case errors.Is(err, storage.ErrTaskOwnership):
		return "Task ownership or conversation ownership changed; read the current task and conversation state before retrying."
	case errors.Is(err, storage.ErrTaskNotFound):
		return "Task not found in this project."
	case errors.Is(err, storage.ErrProjectMessageNotFound):
		return "Project message not found in this project."
	case errors.Is(err, storage.ErrTaskUnsupported):
		return "This task workflow is not supported by the server."
	case errors.Is(err, storage.ErrTaskConflict):
		return err.Error()
	default:
		return "Task operation failed. Check the arguments and current task state."
	}
}
