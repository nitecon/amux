package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func taskAPI(t *testing.T) (*storage.Store, http.Handler) {
	t.Helper()
	s, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.InsertProject(context.Background(), storage.Project{Ident: "tasks", ChannelName: "none", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(s, "task-key")
	if err != nil {
		t.Fatal(err)
	}
	return s, h
}
func taskRequest(h http.Handler, method, path, body string, actor *agent.Origin) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer task-key")
	req.Header.Set("Content-Type", "application/json")
	if actor != nil {
		req.Header.Set("X-Agent-Session-Id", actor.SessionID)
		req.Header.Set("X-Agent-Instance-Id", actor.InstanceID)
		req.Header.Set("X-Agent-Provider", actor.Provider)
		req.Header.Set("X-Agent-OS", actor.OS)
		req.Header.Set("X-Agent-Id", "worker")
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}
func apiActor() *agent.Origin {
	return &agent.Origin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", OS: "linux"}
}

// Rust task_events.rs::origin_headers_are_all_or_none_and_validated, extended for required Antigravity support.
func TestAgentOriginHeaders(t *testing.T) {
	h := http.Header{}
	if actor, err := agentOrigin(h); err != nil || actor != nil {
		t.Fatalf("anonymous %+v, %v", actor, err)
	}
	h.Set("X-Agent-Session-Id", apiActor().SessionID)
	if _, err := agentOrigin(h); err == nil {
		t.Fatal("partial actor accepted")
	}
	h.Set("X-Agent-Instance-Id", apiActor().InstanceID)
	h.Set("X-Agent-Provider", "codex")
	h.Set("X-Agent-OS", "linux")
	actor, err := agentOrigin(h)
	if err != nil || *actor != *apiActor() {
		t.Fatalf("actor %+v, %v", actor, err)
	}
	for name, value := range map[string]string{"X-Agent-Session-Id": "not-uuid", "X-Agent-Instance-Id": "", "X-Agent-Provider": "other", "X-Agent-OS": "other"} {
		bad := h.Clone()
		bad.Set(name, value)
		if _, err := agentOrigin(bad); err == nil {
			t.Fatalf("bad %s accepted", name)
		}
	}
	duplicate := h.Clone()
	duplicate.Add("X-Agent-Session-Id", apiActor().SessionID)
	if _, err := agentOrigin(duplicate); err == nil {
		t.Fatal("duplicate actor accepted")
	}
	h.Set("X-Agent-Provider", "antigravity")
	if _, err := agentOrigin(h); err != nil {
		t.Fatal(err)
	}
}

func TestTasksAPIAndOwnership(t *testing.T) {
	store, h := taskAPI(t)
	actor := apiActor()
	rejected := taskRequest(h, "POST", "/v1/projects/tasks/tasks", `{"title":"Unexplained root"}`, actor)
	if rejected.Code != 400 {
		t.Fatalf("unexplained root %d %s", rejected.Code, rejected.Body.String())
	}
	created := taskRequest(h, "POST", "/v1/projects/tasks/tasks", `{"title":"Work","root_reason":"user-directed","specification":"Port task contracts"}`, actor)
	if created.Code != 200 {
		t.Fatalf("creation %d %s", created.Code, created.Body.String())
	}
	var task storage.Task
	if err := json.Unmarshal(created.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.OwnerOrigin == nil || task.OwnerOrigin.SessionID != actor.SessionID {
		t.Fatalf("submitter reservation %+v", task)
	}
	path := "/v1/projects/tasks/tasks/" + task.ID
	if res := taskRequest(h, "PATCH", path, `{"status":"in_progress"}`, actor); res.Code != 200 {
		t.Fatalf("claim %d %s", res.Code, res.Body.String())
	}
	other := *actor
	other.InstanceID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if res := taskRequest(h, "PATCH", path, `{"status":"todo"}`, &other); res.Code != 409 {
		t.Fatalf("other instance released %d %s", res.Code, res.Body.String())
	}
	if res := taskRequest(h, "POST", path+"/comments", `{"content":"Reviewing the changes"}`, &other); res.Code != 200 {
		t.Fatalf("comment %d %s", res.Code, res.Body.String())
	}
	if res := taskRequest(h, "PATCH", path, `{"status":"todo"}`, actor); res.Code != 409 {
		t.Fatalf("release omitted file confirmation %d %s", res.Code, res.Body.String())
	}
	if res := taskRequest(h, "PATCH", path, `{"status":"todo","changeset":null}`, actor); res.Code != 200 {
		t.Fatalf("release %d %s", res.Code, res.Body.String())
	}
	if res := taskRequest(h, "PATCH", path, `{"status":"done"}`, actor); res.Code != 409 {
		t.Fatalf("completion bypassed file confirmation %d %s", res.Code, res.Body.String())
	}
	detail := taskRequest(h, "GET", path, "", nil)
	if detail.Code != 200 {
		t.Fatalf("detail %d %s", detail.Code, detail.Body.String())
	}
	list := taskRequest(h, "GET", "/v1/projects/tasks/tasks", "", nil)
	if list.Code != 200 || !strings.Contains(list.Body.String(), task.ID) {
		t.Fatalf("list %d %s", list.Code, list.Body.String())
	}
	events, err := store.TaskEvents(context.Background(), storage.EventQuery{})
	if err != nil || len(events) != 4 || events[0].Kind != "task_unclaimed" {
		t.Fatalf("journal %+v, %v", events, err)
	}
}

func TestHumanTaskClaimAndRelease(t *testing.T) {
	_, h := taskAPI(t)
	res := taskRequest(h, "POST", "/v1/projects/tasks/tasks", `{"title":"Human work"}`, nil)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	var task storage.Task
	if err := json.Unmarshal(res.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/tasks/tasks/" + task.ID
	for _, status := range []string{"in_progress", "todo"} {
		res := taskRequest(h, "PATCH", path, `{"status":"`+status+`"}`, nil)
		if res.Code != 200 {
			t.Fatalf("human %s failed: %d %s", status, res.Code, res.Body.String())
		}
	}
}

func TestTaskDetailFlatShapeAndCommentLimits(t *testing.T) {
	_, h := taskAPI(t)
	created := taskRequest(h, "POST", "/v1/projects/tasks/tasks", `{"title":"History","specification":"Keep the full thread"}`, nil)
	var task storage.Task
	if err := json.Unmarshal(created.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/tasks/tasks/" + task.ID
	for _, text := range []string{"First", "Second", "Third"} {
		res := taskRequest(h, "POST", path+"/comments", `{"content":"`+text+`"}`, nil)
		if res.Code != 200 {
			t.Fatal(res.Body.String())
		}
	}
	detail := taskRequest(h, "GET", path+"?comments=2", "", nil)
	var result struct {
		ID            string                `json:"id"`
		Specification *string               `json:"specification"`
		Comments      []storage.TaskComment `json:"comments"`
		Count         int64                 `json:"comment_count"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &result); err != nil || result.ID != task.ID || result.Specification == nil || *result.Specification != "Keep the full thread" || result.Count != 3 || len(result.Comments) != 2 || result.Comments[0].Content != "Second" {
		t.Fatalf("detail shape %+v, %v", result, err)
	}
	if res := taskRequest(h, "POST", path+"/comments", `{"content":"  "}`, nil); res.Code != 400 {
		t.Fatalf("empty comment accepted: %d", res.Code)
	}
	oversized, _ := json.Marshal(map[string]string{"content": strings.Repeat("x", 65537)})
	if res := taskRequest(h, "POST", path+"/comments", string(oversized), nil); res.Code != 413 {
		t.Fatalf("oversized comment accepted: %d", res.Code)
	}
}

func TestFreshServerRegistersNativeProjectAndCreatesTask(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h, err := NewHandler(store, "task-key")
	if err != nil {
		t.Fatal(err)
	}
	res := taskRequest(h, "POST", "/v1/projects", `{"ident":"Fresh Project"}`, nil)
	if res.Code != 200 {
		t.Fatalf("registration %d %s", res.Code, res.Body.String())
	}
	var project struct {
		Ident, Kind string
		RoomID      string `json:"room_id"`
		Channel     string `json:"channel_name"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &project); err != nil || project.Ident != "fresh-project" || project.Kind != "adhoc" || project.RoomID != "fresh-project" || project.Channel != "internal" {
		t.Fatalf("native project %+v, %v", project, err)
	}
	res = taskRequest(h, "POST", "/v1/projects/fresh-project/tasks", `{"title":"First task"}`, nil)
	if res.Code != 200 {
		t.Fatalf("first task %d %s", res.Code, res.Body.String())
	}
	res = taskRequest(h, "POST", "/v1/projects", `{"ident":"Fresh Project","repo_url":"https://github.com/owner/fresh-project.git"}`, nil)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	saved, err := store.Project(context.Background(), "fresh-project")
	if err != nil || saved.CanonicalRemote == nil || *saved.CanonicalRemote != "github.com/owner/fresh-project" {
		t.Fatalf("registration did not backfill: %+v, %v", saved, err)
	}
}
