package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/mcpstdio"
	"github.com/nitecon/amux/storage"
)

func TestAgentTaskToolsCatalogSchemas(t *testing.T) {
	tools := AgentTaskTools()
	want := []string{"task_get", "project_message_thread", "task_list", "task_create", "task_delegate", "task_update_delegation", "task_set_requester", "task_claim", "task_started", "task_comment", "task_ask", "task_release", "task_user_review", "task_done", "task_finish_review", "task_confirm_mutation", "task_set_changeset"}
	if len(tools) != len(want) {
		t.Fatalf("catalog length %d", len(tools))
	}
	for i, tool := range tools {
		if tool.Name != want[i] {
			t.Fatalf("tool %d: got %s want %s", i, tool.Name, want[i])
		}
		var parsed struct {
			Properties           map[string]json.RawMessage `json:"properties"`
			Required             []string                   `json:"required"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &parsed); err != nil {
			t.Fatalf("%s schema: %v", tool.Name, err)
		}
		if parsed.AdditionalProperties == nil || *parsed.AdditionalProperties {
			t.Fatalf("%s permits unknown arguments", tool.Name)
		}
		for _, required := range parsed.Required {
			if _, ok := parsed.Properties[required]; !ok {
				t.Fatalf("%s requires undocumented field %s", tool.Name, required)
			}
		}
	}
	for _, tc := range []struct{ name, field string }{{"task_get", "id"}, {"project_message_thread", "id"}, {"task_claim", "id"}, {"task_started", "id"}, {"task_comment", "id"}, {"task_comment", "body"}, {"task_create", "title"}, {"task_delegate", "title"}, {"task_delegate", "target_project"}, {"task_update_delegation", "id"}, {"task_set_requester", "id"}, {"task_set_changeset", "id"}, {"task_set_changeset", "changeset"}} {
		var tool mcpstdio.Tool
		for _, candidate := range tools {
			if candidate.Name == tc.name {
				tool = candidate
			}
		}
		var parsed struct {
			Required []string `json:"required"`
		}
		_ = json.Unmarshal(tool.InputSchema, &parsed)
		found := false
		for _, field := range parsed.Required {
			found = found || field == tc.field
		}
		if !found {
			t.Errorf("%s.%s must be explicitly required", tc.name, tc.field)
		}
	}
	for _, tc := range []struct{ name, field string }{{"task_create", "notify_results"}, {"task_delegate", "notify_results"}} {
		for _, tool := range tools {
			if tool.Name != tc.name {
				continue
			}
			var parsed struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(tool.InputSchema, &parsed); err != nil || len(parsed.Properties[tc.field]) == 0 {
				t.Errorf("%s schema omitted %s", tc.name, tc.field)
			}
		}
	}
	tools[0].InputSchema[0] = 'x'
	if AgentTaskTools()[0].InputSchema[0] != '{' {
		t.Fatal("catalog schema bytes are shared with callers")
	}
}

func TestLocalConversationTaskToolsCatalogSchemas(t *testing.T) {
	tools := LocalConversationTaskTools()
	want := []string{"task_get", "project_message_thread", "task_list", "task_create", "task_delegate", "task_comment"}
	if len(tools) != len(want) {
		t.Fatalf("local catalog length %d, want %d", len(tools), len(want))
	}
	byName := make(map[string]mcpstdio.Tool, len(tools))
	for i, tool := range tools {
		if tool.Name != want[i] {
			t.Fatalf("tool %d: got %s want %s", i, tool.Name, want[i])
		}
		byName[tool.Name] = tool
		var parsed struct {
			Properties           map[string]json.RawMessage `json:"properties"`
			Required             []string                   `json:"required"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &parsed); err != nil {
			t.Fatalf("%s schema: %v", tool.Name, err)
		}
		if parsed.AdditionalProperties == nil || *parsed.AdditionalProperties {
			t.Fatalf("%s permits unknown arguments", tool.Name)
		}
		for _, required := range parsed.Required {
			if _, ok := parsed.Properties[required]; !ok {
				t.Fatalf("%s requires undocumented field %s", tool.Name, required)
			}
		}
	}
	for _, name := range []string{"task_create", "task_delegate"} {
		var parsed struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(byName[name].InputSchema, &parsed); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		if _, hasNotify := parsed.Properties["notify_results"]; hasNotify {
			t.Errorf("%s local schema must omit notify_results", name)
		}
		if name == "task_delegate" {
			if strings.Contains(strings.ToLower(byName[name].Description), "defaults to") {
				t.Errorf("task_delegate local description still claims default parent: %q", byName[name].Description)
			}
			if strings.Contains(strings.ToLower(parsed.Properties["parent"].Description), "defaults to") {
				t.Errorf("task_delegate local parent description still claims default parent: %q", parsed.Properties["parent"].Description)
			}
			hasParent := false
			for _, field := range parsed.Required {
				hasParent = hasParent || field == "parent"
			}
			if !hasParent {
				t.Errorf("task_delegate local schema must require parent: %v", parsed.Required)
			}
		}
	}
	if _, err := ParseTaskCreateArgs(json.RawMessage(`{"title":"x","root_reason":"user-directed","specification":"spec","notify_results":true}`), false); err == nil || !strings.Contains(err.Error(), `unknown argument "notify_results"`) {
		t.Fatalf("ParseTaskCreateArgs local mode accepted notify_results: %v", err)
	}
	if _, err := ParseTaskDelegateArgs(json.RawMessage(`{"title":"x","target_project":"other"}`), true, false); err == nil || !strings.Contains(err.Error(), "parent is required") {
		t.Fatalf("ParseTaskDelegateArgs local mode accepted omitted parent: %v", err)
	}
	if _, err := ParseTaskDelegateArgs(json.RawMessage(`{"title":"x","target_project":"other","parent":"p1","notify_results":true}`), true, false); err == nil || !strings.Contains(err.Error(), `unknown argument "notify_results"`) {
		t.Fatalf("ParseTaskDelegateArgs local mode accepted notify_results: %v", err)
	}
	tools[0].InputSchema[0] = 'x'
	if LocalConversationTaskTools()[0].InputSchema[0] != '{' {
		t.Fatal("local catalog schema bytes are shared with callers")
	}
}

func invokeTaskTool(t *testing.T, a *api, actor *storage.AgentOrigin, conversationID, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.SetPathValue("ident", "tasks")
	r.SetPathValue("conversation", conversationID)
	r.Header.Set("X-Agent-Session-Id", actor.SessionID)
	r.Header.Set("X-Agent-Instance-Id", actor.InstanceID)
	r.Header.Set("X-Agent-Provider", actor.Provider)
	r.Header.Set("X-Agent-Os", actor.OS)
	w := httptest.NewRecorder()
	a.callTaskTool(w, r)
	return w
}

func TestTaskToolLifecycleAndStrictArguments(t *testing.T) {
	store, _ := taskAPI(t)
	ctx := context.Background()
	actor := apiActor()
	if _, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Fenced workflow", Reporter: actor.SessionID, RootReason: stringPtr("user-directed"), Details: stringPtr("Exercise workflow")}, actor); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.Tasks(ctx, "tasks", []string{"todo"}, true)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks: %v %d", err, len(tasks))
	}
	conversation, err := store.CreateConversation(ctx, "tasks", tasks[0].ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(store, "unused")
	call := func(name, args string) mcpstdio.Result {
		w := invokeTaskTool(t, a, actor, conversation.ID, `{"name":"`+name+`","arguments":`+args+`}`)
		if w.Code != 200 {
			t.Fatalf("%s HTTP %d: %s", name, w.Code, w.Body.String())
		}
		var result mcpstdio.Result
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	id := tasks[0].ID
	if got := call("task_claim", `{"id":"`+id+`"}`); got.IsError {
		t.Fatalf("claim: %+v", got)
	}
	if got := call("task_started", `{"id":"`+id+`"}`); got.IsError {
		t.Fatalf("started: %+v", got)
	}
	if got := call("task_done", `{"id":"`+id+`"}`); !got.IsError || !strings.Contains(got.Content[0].Text, "Did this task mutate any files?") {
		t.Fatalf("guided question: %+v", got)
	}
	if got := call("task_confirm_mutation", `{"id":"`+id+`"}`); got.IsError || !strings.Contains(got.Content[0].Text, "task_set_changeset") {
		t.Fatalf("confirm: %+v", got)
	}
	if got := call("task_set_changeset", `{"id":"`+id+`","changeset":["file.go"]}`); got.IsError {
		t.Fatalf("changeset: %+v", got)
	}
	detail, err := store.Task(ctx, "tasks", id)
	if err != nil || detail.Task.Status != "done" || len(detail.Task.Changeset) != 1 {
		t.Fatalf("done task %+v: %v", detail, err)
	}
	if got := call("task_get", `{"id":"`+id+`","surprise":true}`); !got.IsError || !strings.Contains(got.Content[0].Text, "unknown argument") {
		t.Fatalf("unknown argument: %+v", got)
	}
	if got := call("task_list", `{"status":null}`); !got.IsError || !strings.Contains(got.Content[0].Text, "invalid status") {
		t.Fatalf("null status: %+v", got)
	}
	if got := call("task_set_changeset", `{"id":"`+id+`"}`); !got.IsError || !strings.Contains(got.Content[0].Text, "changeset is required") {
		t.Fatalf("missing explicit changeset: %+v", got)
	}
}

func TestTaskToolStaleConversationRejected(t *testing.T) {
	store, _ := taskAPI(t)
	ctx := context.Background()
	owner := apiActor()
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Handoff", Reporter: owner.SessionID, RootReason: stringPtr("user-directed"), Details: stringPtr("Check stale actor")}, owner)
	if err != nil {
		t.Fatal(err)
	}
	c, err := store.CreateConversation(ctx, "tasks", task.ID, owner)
	if err != nil {
		t.Fatal(err)
	}
	current := *owner
	current.InstanceID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if _, err := store.ResumeConversation(ctx, "tasks", c.ID, owner.InstanceID, &current); err != nil {
		t.Fatal(err)
	}
	w := invokeTaskTool(t, newAPI(store, "unused"), owner, c.ID, `{"name":"task_done","arguments":{"id":"`+task.ID+`","changeset":null}}`)
	if w.Code != 409 {
		t.Fatalf("stale caller HTTP %d: %s", w.Code, w.Body.String())
	}
}

func TestTaskToolDoneWithExplicitNullChangeset(t *testing.T) {
	store, _ := taskAPI(t)
	ctx := context.Background()
	actor := apiActor()
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "No file changes", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := store.CreateConversation(ctx, "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(store, "unused")
	for _, call := range []struct {
		name string
		args string
	}{{"task_claim", `{"id":"` + task.ID + `"}`}, {"task_done", `{"id":"` + task.ID + `","changeset":null}`}} {
		w := invokeTaskTool(t, a, actor, conversation.ID, `{"name":"`+call.name+`","arguments":`+call.args+`}`)
		var result mcpstdio.Result
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result.IsError {
			t.Fatalf("%s: HTTP %d, result %+v, error %v", call.name, w.Code, result, err)
		}
	}
	detail, err := store.Task(ctx, "tasks", task.ID)
	if err != nil || detail == nil || detail.Task.Status != "done" || len(detail.Task.Changeset) != 0 {
		t.Fatalf("null changeset completion: %+v, %v", detail, err)
	}
}

func TestTaskToolRootCreationRules(t *testing.T) {
	store, _ := taskAPI(t)
	ctx := context.Background()
	actor := apiActor()
	parent, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Parent", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := store.CreateConversation(ctx, "tasks", parent.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(store, "unused")
	invoke := func(args string) mcpstdio.Result {
		w := invokeTaskTool(t, a, actor, conversation.ID, `{"name":"task_create","arguments":`+args+`}`)
		var result mcpstdio.Result
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 {
			t.Fatalf("create response HTTP %d: %s (%v)", w.Code, w.Body.String(), err)
		}
		return result
	}
	if result := invoke(`{"title":"Unexplained root"}`); !result.IsError {
		t.Fatal("unexplained root was accepted")
	}
	if result := invoke(`{"title":"Missing specification","root_reason":"principle"}`); !result.IsError {
		t.Fatal("root without specification was accepted")
	}
	if result := invoke(`{"title":"Subtask","parent":"` + parent.ID + `"}`); result.IsError {
		t.Fatalf("valid subtask rejected: %+v", result)
	}
}

func TestTaskToolLinksFirstCreatedTaskToStandaloneConversation(t *testing.T) {
	store, _ := taskAPI(t)
	ctx := context.Background()
	actor := apiActor()
	chat, err := store.CreateConversation(ctx, "tasks", "", actor)
	if err != nil {
		t.Fatal(err)
	}
	api := newAPI(store, "unused")
	create := func(title string) storage.Task {
		t.Helper()
		arguments, err := json.Marshal(map[string]string{"title": title, "root_reason": "user-directed", "specification": "Requested from the conversation."})
		if err != nil {
			t.Fatal(err)
		}
		response := invokeTaskTool(t, api, actor, chat.ID, `{"name":"task_create","arguments":`+string(arguments)+`}`)
		var result mcpstdio.Result
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 200 || result.IsError || len(result.Content) != 1 {
			t.Fatalf("task creation HTTP %d: %s (%v)", response.Code, response.Body, err)
		}
		var task storage.Task
		if err := json.Unmarshal([]byte(result.Content[0].Text), &task); err != nil || task.ID == "" {
			t.Fatalf("task response %+v: %v", task, err)
		}
		return task
	}
	first := create("First conversation task")
	second := create("Further conversation task")
	linked, err := store.Conversation(ctx, "tasks", chat.ID)
	if err != nil || linked == nil || linked.TaskID != first.ID || linked.TaskID == second.ID {
		t.Fatalf("conversation link %+v: %v", linked, err)
	}
	items, err := store.TaskConversations(ctx, "tasks", first.ID, "", 10)
	if err != nil || len(items) != 1 || items[0].ID != chat.ID {
		t.Fatalf("linked task history %+v: %v", items, err)
	}
}

func stringPtr(value string) *string { return &value }

func TestTaskAskToolRaisesQuestionListedUntilUserAnswers(t *testing.T) {
	store, _ := taskAPI(t)
	ctx := context.Background()
	actor := apiActor()
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Blocked work", Reporter: actor.SessionID, RootReason: stringPtr("user-directed"), Details: stringPtr("Exercise questions")}, actor)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := store.CreateConversation(ctx, "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(store, "unused")
	call := func(name, args string) mcpstdio.Result {
		w := invokeTaskTool(t, a, actor, conversation.ID, `{"name":"`+name+`","arguments":`+args+`}`)
		var result mcpstdio.Result
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("%s HTTP %d: %s", name, w.Code, w.Body.String())
		}
		return result
	}
	if got := call("task_claim", `{"id":"`+task.ID+`"}`); got.IsError {
		t.Fatalf("claim: %+v", got)
	}
	if got := call("task_ask", `{"id":"`+task.ID+`","question":""}`); !got.IsError || !strings.Contains(got.Content[0].Text, "question must be non-empty") {
		t.Fatalf("blank question: %+v", got)
	}
	if got := call("task_ask", `{"id":"`+task.ID+`","question":"Keep or drop?","options":["Keep","Drop"]}`); got.IsError {
		t.Fatalf("ask: %+v", got)
	}
	list := func() []storage.TaskQuestion {
		w := httptest.NewRecorder()
		a.openTaskQuestions(w, httptest.NewRequest("GET", "/v1/questions", nil))
		var questions []storage.TaskQuestion
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &questions) != nil {
			t.Fatalf("questions HTTP %d: %s", w.Code, w.Body.String())
		}
		return questions
	}
	if questions := list(); len(questions) != 1 || questions[0].TaskID != task.ID || len(questions[0].Options) != 2 {
		t.Fatalf("listed questions: %+v", questions)
	}
	mux := http.NewServeMux()
	a.registerTaskRoutes(mux)
	answer := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/projects/tasks/tasks/"+task.ID+"/comments", strings.NewReader(body)))
		return w
	}
	questionID := list()[0].ID
	if w := answer(`{"content":"Keep","author":"user","author_type":"agent","question_id":"` + questionID + `"}`); w.Code != 400 {
		t.Fatalf("agent answered a question: HTTP %d %s", w.Code, w.Body.String())
	}
	if w := answer(`{"content":"Keep","author":"user","author_type":"user","question_id":"` + questionID + `"}`); w.Code != 200 {
		t.Fatalf("answer HTTP %d: %s", w.Code, w.Body.String())
	}
	if w := answer(`{"content":"Again","author":"user","author_type":"user","question_id":"` + questionID + `"}`); w.Code != 409 {
		t.Fatalf("closed question answered again: HTTP %d %s", w.Code, w.Body.String())
	}
	if questions := list(); len(questions) != 0 {
		t.Fatalf("answered question still listed: %+v", questions)
	}
}

func TestGatewayClientAnswerTaskQuestionAcceptsQuotedAcknowledgement(t *testing.T) {
	store, h := taskAPI(t)
	ctx := context.Background()
	actor := apiActor()
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Blocked work", Reporter: actor.SessionID, RootReason: stringPtr("user-directed"), Details: stringPtr("Exercise answers")}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyTaskTransition(ctx, "tasks", task.ID, "in_progress", actor, storage.TaskTransitionPolicy{}); err != nil {
		t.Fatal(err)
	}
	question, err := store.AskTaskQuestion(ctx, "tasks", task.ID, "Which branch?\nPick one.", []string{"main"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	client, err := gatewayclient.New(srv.URL, "task-key", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	comment, err := client.AnswerTaskQuestion(ctx, "tasks", task.ID, question.ID, question.Question, "main")
	if err != nil {
		t.Fatalf("answer rejected: %v", err)
	}
	if comment.Content != storage.TaskAnswerComment(question.Question, "main") {
		t.Fatalf("answer comment = %q", comment.Content)
	}
	if detail, err := store.Task(ctx, "tasks", task.ID); err != nil || detail.Task.WaitingOnUser {
		t.Fatalf("answered task still waiting: %+v, %v", detail, err)
	}
}
