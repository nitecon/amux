package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/taskplatform"
)

const specificationHint = "If this is a complex task, make sure it has a proper specification so another agent can pick up the work if you drop it."
const changesetQuestion = `Did this task mutate any files? Repeat the transition with changeset: ["path/to/file"] listing every modified, added or deleted file, or changeset: null if no files were mutated. The task has not moved.`

func agentOrigin(headers http.Header) (*storage.AgentOrigin, error) {
	names := []string{"X-Agent-Session-Id", "X-Agent-Instance-Id", "X-Agent-Provider", "X-Agent-Os"}
	any := false
	for _, name := range names {
		if len(headers.Values(name)) > 0 {
			any = true
		}
	}
	if !any {
		return nil, nil
	}
	values := make([]string, len(names))
	for i, name := range names {
		entries := headers.Values(name)
		if len(entries) != 1 {
			return nil, fmt.Errorf("exactly one %s required", name)
		}
		values[i] = entries[0]
	}
	return storage.NormalizeOrigin(&storage.AgentOrigin{SessionID: values[0], InstanceID: values[1], Provider: values[2], OS: values[3]})
}
func requestActor(w http.ResponseWriter, r *http.Request) (*storage.AgentOrigin, bool) {
	origin, err := agentOrigin(r.Header)
	if err != nil {
		writeError(w, 400, err.Error())
		return nil, false
	}
	id := strings.TrimSpace(r.Header.Get("X-Agent-Id"))
	if origin == nil && id != "" && id != "_default" {
		writeError(w, 400, "agent task operations require complete X-Agent session/instance/provider/OS headers")
		return nil, false
	}
	return origin, true
}
func readTaskRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	return readJSONRequest(w, r, value, 1<<20)
}
func readJSONRequest(w http.ResponseWriter, r *http.Request, value any, maxBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		writeError(w, 400, "invalid request")
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		writeError(w, 400, "request must contain one JSON object")
		return false
	}
	return true
}
func resolveTaskIdentity(explicit *string, r *http.Request) string {
	if explicit != nil && strings.TrimSpace(*explicit) != "" {
		return strings.TrimSpace(*explicit)
	}
	id := strings.TrimSpace(r.Header.Get("X-Agent-Id"))
	if id != "" && id != "_default" {
		return id
	}
	return "user"
}
func taskError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrTaskMetadataInput), errors.Is(err, storage.ErrTaskQuestionInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, storage.ErrTaskDelegation):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, storage.ErrTaskCancellationInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, storage.ErrTaskGuidedQuestion):
		var question *storage.TaskGuidedQuestion
		if errors.As(err, &question) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "code": "task_confirmation_required", "task_id": question.TaskID})
		} else {
			writeError(w, 409, err.Error())
		}
	case errors.Is(err, storage.ErrTaskChangeset):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": changesetQuestion, "code": "changeset_confirmation_required"})
	case errors.Is(err, storage.ErrTargetBinding):
		writeError(w, http.StatusConflict, "canonical_remote must match the project repository")
	case errors.Is(err, storage.ErrTaskOwnership), errors.Is(err, storage.ErrTaskConflict):
		writeError(w, 409, err.Error())
	case errors.Is(err, storage.ErrTaskNotFound):
		writeError(w, 404, "task or project not found")
	case errors.Is(err, storage.ErrTaskUnsupported):
		writeError(w, 501, err.Error())
	default:
		writeError(w, 500, "task operation failed")
	}
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

func (a *api) registerTaskRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/tasks/capabilities", taskCapabilities)
	mux.HandleFunc("POST /v1/projects/{ident}/tasks/delegate", a.delegateTask)
	mux.HandleFunc("PATCH /v1/projects/{ident}/tasks/{id}/delegation", a.updateTaskDelegation)
	mux.HandleFunc("PUT /v1/projects/{ident}/tasks/{id}/requester", a.setTaskRequester)
	mux.HandleFunc("POST /v1/projects/{ident}/tasks", a.createTask)
	mux.HandleFunc("GET /v1/projects/{ident}/tasks/{id}/subtasks", a.getTaskSubtasks)
	mux.HandleFunc("POST /v1/projects/{ident}/tasks/{id}/subtasks", a.createTaskSubtask)
	mux.HandleFunc("GET /v1/projects/{ident}/tasks", a.listTasks)
	mux.HandleFunc("GET /v1/projects/{ident}/tasks/{id}", a.getTask)
	mux.HandleFunc("PATCH /v1/projects/{ident}/tasks/{id}", a.transitionTask)
	mux.HandleFunc("POST /v1/projects/{ident}/tasks/reorder", a.reorderTasks)
	mux.HandleFunc("POST /v1/projects/{ident}/tasks/{id}/reassign", a.reassignTask)
	mux.HandleFunc("POST /v1/projects/{ident}/tasks/{id}/assign", a.assignTask)
	mux.HandleFunc("POST /v1/projects/{ident}/tasks/{id}/comments", a.commentTask)
	mux.HandleFunc("POST /v1/projects/{ident}/tasks/{id}/questions", a.askTaskQuestion)
	mux.HandleFunc("GET /v1/questions", a.openTaskQuestions)
	mux.HandleFunc("POST /v1/projects/{ident}/tasks/{id}/confirmation", a.confirmTask)
	mux.HandleFunc("POST /v1/projects/{ident}/tasks/{id}/review", a.enqueueReviews)
}

func taskCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]int{
		"task_hierarchy": 1, "requester_subscriptions": 1,
		"changeset_confirmation": 1, "guided_confirmation": 1, "native_worker_assignment": 1,
	})
}

func (a *api) createTask(w http.ResponseWriter, r *http.Request) {
	origin, ok := requestActor(w, r)
	if !ok {
		return
	}
	var req *struct {
		Title           string               `json:"title"`
		Parent          *string              `json:"parent_task_id"`
		RootReason      *string              `json:"root_reason"`
		Goal            json.RawMessage      `json:"goal"`
		Description     *string              `json:"description"`
		Specification   *string              `json:"specification"`
		Details         *string              `json:"details"`
		Hostname        *string              `json:"hostname"`
		Reporter        *string              `json:"reporter"`
		Labels          []string             `json:"labels"`
		Requester       json.RawMessage      `json:"requester"`
		Routing         *storage.TaskRouting `json:"routing"`
		CanonicalRemote *string              `json:"canonical_remote"`
	}
	if !readTaskRequest(w, r, &req) {
		return
	}
	if req == nil {
		writeError(w, 400, "task must be an object")
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeError(w, 400, "title must be non-empty")
		return
	}
	if _, err := taskplatform.RequiredOS(req.Labels); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if origin != nil && req.Parent == nil && req.RootReason == nil {
		writeError(w, 400, "agent task creation requires parent_task_id or root_reason=user-directed|principle with an explanatory specification")
		return
	}
	if req.Specification != nil {
		req.Details = req.Specification
	}
	if req.Routing != nil && req.Routing.Mode != "auto" && req.Routing.Mode != "unassigned" && req.Routing.Mode != "broadcast" && req.Routing.Mode != "session" {
		writeError(w, http.StatusUnprocessableEntity, "unsupported task routing mode")
		return
	}
	if req.RootReason != nil && ((*req.RootReason != "user-directed" && *req.RootReason != "principle") || req.Parent != nil || req.Details == nil || strings.TrimSpace(*req.Details) == "") {
		writeError(w, 400, "invalid root_reason or missing explanatory specification")
		return
	}
	requester, err := parseOptionalRequester(req.Requester)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid requester subscription")
		return
	}
	if requester != nil && origin != nil && requester.Origin != *origin {
		writeError(w, http.StatusBadRequest, "native callers may subscribe only their current execution")
		return
	}
	var goal *string
	if len(req.Goal) > 0 {
		if err := json.Unmarshal(req.Goal, &goal); err != nil {
			writeError(w, 400, "goal must be a string or null")
			return
		}
	} else if text := r.Header.Get("X-Task-Goal"); text != "" {
		goal = &text
	}
	goal, err = storage.NormalizeTaskGoal(goal)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	for _, label := range req.Labels {
		if req.RootReason != nil && label == "autoreview-finding" {
			writeError(w, 400, "root_reason cannot accompany a review finding")
			return
		}
	}
	task, err := a.store.InsertTask(r.Context(), storage.TaskInsert{Project: r.PathValue("ident"), Title: strings.TrimSpace(req.Title), Reporter: resolveTaskIdentity(req.Reporter, r), Description: req.Description, Details: req.Details, Hostname: req.Hostname, Goal: goal, Labels: req.Labels, ParentID: req.Parent, RootReason: req.RootReason, Requester: requester, Routing: req.Routing, CanonicalRemote: req.CanonicalRemote}, origin)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, struct {
		*storage.Task
		Specification *string `json:"specification"`
		Hint          string  `json:"hint"`
	}{task, task.Details, specificationHint})
}

func (a *api) listTasks(w http.ResponseWriter, r *http.Request) {
	origin, ok := requestActor(w, r)
	if !ok {
		return
	}
	p, err := a.store.Project(r.Context(), r.PathValue("ident"))
	if err != nil {
		taskError(w, err)
		return
	}
	if p == nil {
		taskError(w, storage.ErrTaskNotFound)
		return
	}
	query := r.URL.Query()
	statuses := storage.OpenTaskStatuses()
	if text := query.Get("status"); text != "" {
		statuses = []string{}
		for _, part := range strings.Split(text, ",") {
			if s := strings.TrimSpace(part); s != "" {
				statuses = append(statuses, s)
			}
		}
	}
	includeStale := false
	if text := query.Get("include_stale"); text != "" {
		includeStale, err = strconv.ParseBool(text)
		if err != nil {
			writeError(w, 400, "invalid include_stale")
			return
		}
	}
	allGoals := false
	if text := query.Get("all_goals"); text != "" {
		allGoals, err = strconv.ParseBool(text)
		if err != nil {
			writeError(w, 400, "invalid all_goals")
			return
		}
	}
	for _, s := range statuses {
		if !storage.IsValidTaskStatus(s) {
			writeError(w, 400, "invalid task status")
			return
		}
	}
	tasks, err := a.store.Tasks(r.Context(), r.PathValue("ident"), statuses, includeStale)
	if err != nil {
		taskError(w, err)
		return
	}
	goal, selected := query["goal"]
	selectedGoal := ""
	if selected {
		selectedGoal = goal[0]
	} else if header := r.Header.Get("X-Task-Goal"); header != "" {
		selectedGoal = header
		selected = true
	}
	if !allGoals && (selected || origin != nil) {
		filtered := []storage.TaskSummary{}
		for _, task := range tasks {
			value := ""
			if task.Goal != nil {
				value = *task.Goal
			}
			if value == strings.TrimSpace(selectedGoal) {
				filtered = append(filtered, task)
			}
		}
		tasks = filtered
	}
	writeJSON(w, tasks)
}

type taskAction struct {
	Verb         string `json:"verb"`
	Style        string `json:"style"`
	TargetStatus string `json:"target_status"`
}

func taskActions(status string) []taskAction {
	switch {
	case status == "todo":
		return []taskAction{{"Claim", "primary", "in_progress"}, {"Done", "primary", "done"}, {"User review", "secondary", "user_review"}}
	case storage.IsActiveTaskStatus(status):
		return []taskAction{{"Release", "secondary", "todo"}, {"Done", "primary", "done"}, {"User review", "secondary", "user_review"}}
	case status == "user_review":
		return []taskAction{{"Return to TODO", "secondary", "todo"}, {"Close", "primary", "done"}}
	case status == "done":
		return []taskAction{{"Reopen", "secondary", "todo"}}
	default:
		return []taskAction{}
	}
}
func (a *api) getTask(w http.ResponseWriter, r *http.Request) {
	var limit *uint64
	if text, ok := r.URL.Query()["comments"]; ok {
		count, err := strconv.ParseUint(text[0], 10, 64)
		if err != nil {
			writeError(w, 400, "invalid comment limit")
			return
		}
		limit = &count
	}
	detail, err := a.store.TaskRecent(r.Context(), r.PathValue("ident"), r.PathValue("id"), limit)
	if err != nil {
		taskError(w, err)
		return
	}
	if detail == nil {
		taskError(w, storage.ErrTaskNotFound)
		return
	}
	writeJSON(w, struct {
		storage.Task
		TaskType      string                `json:"task_type"`
		Specification *string               `json:"specification"`
		Comments      []storage.TaskComment `json:"comments"`
		Count         int64                 `json:"comment_count"`
		Actions       []taskAction          `json:"actions"`
	}{detail.Task, detail.Task.TaskType, detail.Task.Details, detail.Comments, detail.CommentCount, taskActions(detail.Task.Status)})
}

func (a *api) transitionTask(w http.ResponseWriter, r *http.Request) {
	origin, ok := requestActor(w, r)
	if !ok {
		return
	}
	var req *struct {
		storage.TaskWorkflowUpdate
		Status json.RawMessage `json:"status"`
	}
	if !readTaskRequest(w, r, &req) {
		return
	}
	if req == nil {
		writeError(w, 400, "task update must be an object")
		return
	}
	if req.Status != nil && !bytes.Equal(bytes.TrimSpace(req.Status), []byte("null")) {
		var status string
		if json.Unmarshal(req.Status, &status) != nil || (status != "todo" && status != "in_progress" && status != "user_review" && status != "done") {
			writeError(w, http.StatusBadRequest, "invalid task status")
			return
		}
		req.TaskWorkflowUpdate.Status = status
	}
	if err := storage.ValidateTaskMetadata(req.TaskMetadataUpdate); err != nil {
		taskError(w, err)
		return
	}
	req.Policy = storage.TaskTransitionPolicy{HumanRequest: origin == nil, CheckGoal: origin != nil, SelectedGoal: r.Header.Get("X-Task-Goal")}
	task, err := a.store.UpdateTaskWorkflow(r.Context(), r.PathValue("ident"), r.PathValue("id"), req.TaskWorkflowUpdate, origin)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, task)
}

func (a *api) confirmTask(w http.ResponseWriter, r *http.Request) {
	origin, ok := requestActor(w, r)
	if !ok {
		return
	}
	var req *struct {
		TaskType  *string         `json:"task_type"`
		Changeset json.RawMessage `json:"changeset"`
	}
	if !readTaskRequest(w, r, &req) {
		return
	}
	if req == nil {
		writeError(w, 400, "confirmation must be an object")
		return
	}
	result, err := a.store.ConfirmTaskWorkflow(r.Context(), r.PathValue("ident"), r.PathValue("id"), storage.TaskWorkflowConfirmation{TaskType: req.TaskType, Changeset: req.Changeset}, origin)
	if err != nil {
		taskError(w, err)
		return
	}
	if result.NeedsChangeset {
		id := r.PathValue("id")
		writeJSON(w, map[string]any{"task_id": id, "message": "Task mutation confirmed; the task has not moved. Submit every modified, added or deleted path using `amux set-changeset " + id + " '[\"path/to/file\"]'` (MCP `task_set_changeset`).", "next_tool": "task_set_changeset"})
		return
	}
	writeJSON(w, result.Task)
}

func (a *api) enqueueReviews(w http.ResponseWriter, r *http.Request) {
	tasks, err := a.store.EnqueueTaskReviews(r.Context(), r.PathValue("ident"), r.PathValue("id"))
	if err != nil {
		taskError(w, err)
		return
	}
	if len(tasks) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	for _, task := range tasks {
		if !hasReviewLabel(task.Labels, "documentation-review") {
			writeJSON(w, task)
			return
		}
	}
	writeJSON(w, tasks[0])
}
func hasReviewLabel(labels []string, match string) bool {
	for _, label := range labels {
		if label == match {
			return true
		}
	}
	return false
}

func (a *api) commentTask(w http.ResponseWriter, r *http.Request) {
	origin, err := agentOrigin(r.Header)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	var req *struct {
		Content    *string `json:"content"`
		Hostname   *string `json:"hostname"`
		Author     *string `json:"author"`
		AuthorType *string `json:"author_type"`
		QuestionID *string `json:"question_id"`
	}
	if !readTaskRequest(w, r, &req) {
		return
	}
	if req == nil || req.Content == nil {
		writeError(w, 400, "content is required")
		return
	}
	if strings.TrimSpace(*req.Content) == "" {
		writeError(w, 400, "content must be non-empty")
		return
	}
	if len(*req.Content) > 65536 {
		writeError(w, 413, "content must be at most 65536 bytes")
		return
	}
	kind := "user"
	if id := strings.TrimSpace(r.Header.Get("X-Agent-Id")); id != "" && id != "_default" {
		kind = "agent"
	}
	if req.AuthorType != nil {
		kind = *req.AuthorType
	}
	if kind != "agent" && kind != "user" {
		writeError(w, 400, "author_type must be agent or user")
		return
	}
	var comment *storage.TaskComment
	if req.QuestionID != nil {
		// An answer is a user comment bound to one open question.
		if kind != "user" || origin != nil {
			writeError(w, 400, "only the user can answer a question")
			return
		}
		comment, err = a.store.AnswerTaskQuestion(r.Context(), r.PathValue("ident"), r.PathValue("id"), *req.QuestionID, *req.Content)
	} else {
		comment, err = a.store.AddTaskComment(r.Context(), r.PathValue("ident"), r.PathValue("id"), resolveTaskIdentity(req.Author, r), kind, *req.Content, req.Hostname, origin)
	}
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, comment)
}

// askTaskQuestion records a question from the task's active owner.
func (a *api) askTaskQuestion(w http.ResponseWriter, r *http.Request) {
	origin, ok := requestActor(w, r)
	if !ok {
		return
	}
	if origin == nil {
		writeError(w, 400, "questions require complete agent origin headers")
		return
	}
	var req *struct {
		Question string   `json:"question"`
		Options  []string `json:"options"`
	}
	if !readTaskRequest(w, r, &req) {
		return
	}
	if req == nil {
		writeError(w, 400, "question is required")
		return
	}
	question, err := a.store.AskTaskQuestion(r.Context(), r.PathValue("ident"), r.PathValue("id"), req.Question, req.Options, origin)
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, question)
}

// openTaskQuestions lists unanswered questions, optionally for one project.
func (a *api) openTaskQuestions(w http.ResponseWriter, r *http.Request) {
	questions, err := a.store.OpenTaskQuestions(r.Context(), r.URL.Query().Get("project"))
	if err != nil {
		taskError(w, err)
		return
	}
	writeJSON(w, questions)
}
