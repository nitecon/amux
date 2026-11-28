package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

// Rust routes.rs::update_task_handler: nullable fields and specification alias.
func TestTaskMetadataPatchNullableFieldsAndCombinedSnapshot(t *testing.T) {
	s, h := taskAPI(t)
	task, err := s.InsertTask(context.Background(), storage.TaskInsert{Project: "tasks", Title: "Before", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/tasks/tasks/" + task.ID
	response := taskRequest(h, http.MethodPatch, path, `{"title":"After","goal":" focused ","description":"Context","details":"old alias","specification":"Plan","hostname":"worker-host","labels":["documentation"],"rank":12}`, nil)
	if response.Code != 200 {
		t.Fatalf("metadata patch: %d %s", response.Code, response.Body)
	}
	var updated storage.Task
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Title != "After" || updated.Goal == nil || *updated.Goal != "focused" || updated.Details == nil || *updated.Details != "Plan" || updated.Rank != 12 || updated.Hostname == nil || *updated.Hostname != "worker-host" {
		t.Fatalf("metadata: %+v", updated)
	}
	response = taskRequest(h, http.MethodPatch, path, `{"status":"done","title":"Completed title","goal":null,"description":null,"specification":null,"hostname":null,"rank":23}`, nil)
	if response.Code != 200 {
		t.Fatalf("combined patch: %d %s", response.Code, response.Body)
	}
	detail, err := s.Task(context.Background(), "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Goal != nil || detail.Task.Description != nil || detail.Task.Details != nil || detail.Task.Hostname != nil || detail.Task.Status != "done" {
		t.Fatalf("nullable fields not cleared: %+v", detail.Task)
	}
	events, err := s.TaskEvents(context.Background(), storage.EventQuery{TaskID: &task.ID})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "task_completed" {
			var snapshot storage.Task
			if err := json.Unmarshal(event.Task, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.Title != "Completed title" || snapshot.Rank != 23 {
				t.Fatalf("partial event snapshot: %+v", snapshot)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("missing completion event")
	}
}

func TestTaskMetadataPatchRejectsMalformedNullableFields(t *testing.T) {
	s, h := taskAPI(t)
	task, err := s.InsertTask(context.Background(), storage.TaskInsert{Project: "tasks", Title: "Before", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"goal":3}`, `{"description":[]}`, `{"details":{}}`, `{"specification":false}`, `{"hostname":4}`, `{"goal":"bad\nvalue"}`, `{"labels":[null]}`, `{"status":""}`, `{"status":false}`, `{"owner_agent_id":"unsupported-alias"}`} {
		response := taskRequest(h, http.MethodPatch, "/v1/projects/tasks/tasks/"+task.ID, body, nil)
		if response.Code != 400 {
			t.Errorf("%s: %d %s", body, response.Code, response.Body)
		}
	}
}

// Rust routes.rs::task_goal_claims_require_matching_context_and_can_be_cleared.
func TestTaskMetadataGoalChangesNeedSelectedGoalAndExactOwner(t *testing.T) {
	s, h := taskAPI(t)
	goal := "release"
	task, err := s.InsertTask(context.Background(), storage.TaskInsert{Project: "tasks", Title: "Scoped", Reporter: "user", Goal: &goal}, apiActor())
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/tasks/tasks/" + task.ID
	patch := func(actor *agent.Origin, selected string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(`{"goal":null}`))
		req.Header.Set("Authorization", "Bearer task-key")
		req.Header.Set("X-Agent-Session-Id", actor.SessionID)
		req.Header.Set("X-Agent-Instance-Id", actor.InstanceID)
		req.Header.Set("X-Agent-Provider", actor.Provider)
		req.Header.Set("X-Agent-OS", actor.OS)
		req.Header.Set("X-Task-Goal", selected)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, req)
		return response
	}
	peer := *apiActor()
	peer.InstanceID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	for _, attempt := range []struct {
		actor *agent.Origin
		goal  string
	}{{&peer, goal}, {apiActor(), "different"}, {apiActor(), ""}} {
		response := patch(attempt.actor, attempt.goal)
		if response.Code != 409 {
			t.Fatalf("goal guard: %d %s", response.Code, response.Body)
		}
	}
	response := patch(apiActor(), goal)
	if response.Code != 200 {
		t.Fatalf("owner clear goal: %d %s", response.Code, response.Body)
	}
	detail, err := s.Task(context.Background(), "tasks", task.ID)
	if err != nil || detail.Task.Goal != nil {
		t.Fatalf("goal not cleared: %+v %v", detail, err)
	}
}

func TestTaskMetadataReviewLabelsAndOSChangesRemainGuarded(t *testing.T) {
	s, h := taskAPI(t)
	ctx := context.Background()
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Move hosts", Reporter: "user"}, apiActor())
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/tasks/tasks/" + task.ID
	rejected := taskRequest(h, http.MethodPatch, path, `{"labels":["os:windows"],"status":"in_progress"}`, apiActor())
	if rejected.Code != 409 {
		t.Fatalf("combined OS transition: %d %s", rejected.Code, rejected.Body)
	}
	changed := taskRequest(h, http.MethodPatch, path, `{"labels":["os:windows"]}`, apiActor())
	if changed.Code != 200 {
		t.Fatalf("OS update: %d %s", changed.Code, changed.Body)
	}
	detail, err := s.Task(ctx, "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != "todo" || detail.Task.OwnerOrigin != nil || detail.Task.OwnerAgentID != nil || detail.Task.StartedAt != nil {
		t.Fatalf("incompatible owner retained: %+v", detail.Task)
	}

	source, err := s.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Review source", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	completed := taskRequest(h, http.MethodPatch, "/v1/projects/tasks/tasks/"+source.ID, `{"status":"done","changeset":["main.go"]}`, nil)
	if completed.Code != 200 {
		t.Fatalf("source completion: %d %s", completed.Code, completed.Body)
	}
	tasks, err := s.Tasks(ctx, "tasks", []string{"todo"}, true)
	if err != nil {
		t.Fatal(err)
	}
	review := ""
	for _, candidate := range tasks {
		for _, label := range candidate.Labels {
			if label == "task-review" {
				review = candidate.ID
				break
			}
		}
	}
	if review == "" {
		t.Fatal("missing generated review")
	}
	for _, actor := range []*agent.Origin{nil, apiActor()} {
		response := taskRequest(h, http.MethodPatch, "/v1/projects/tasks/tasks/"+review, `{"labels":[]}`, actor)
		if response.Code != 409 {
			t.Fatalf("review label mutation: %d %s", response.Code, response.Body)
		}
	}
}

func TestTaskMetadataReviewActionOnlyRequiresUserDecision(t *testing.T) {
	s, h := taskAPI(t)
	task, err := s.InsertTask(context.Background(), storage.TaskInsert{Project: "tasks", Title: "Decision", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/tasks/tasks/" + task.ID
	parked := taskRequest(h, http.MethodPatch, path, `{"status":"user_review","review_action":"Choose date"}`, nil)
	if parked.Code != 200 {
		t.Fatalf("park: %d %s", parked.Code, parked.Body)
	}
	denied := taskRequest(h, http.MethodPatch, path, `{"review_action":"Approve release"}`, apiActor())
	if denied.Code != 409 {
		t.Fatalf("agent decision change: %d %s", denied.Code, denied.Body)
	}
	changed := taskRequest(h, http.MethodPatch, path, `{"review_action":"Approve release"}`, nil)
	if changed.Code != 200 {
		t.Fatalf("user decision edit: %d %s", changed.Code, changed.Body)
	}
	detail, err := s.Task(context.Background(), "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != "user_review" || detail.Task.ReviewAction == nil || *detail.Task.ReviewAction != "Approve release" {
		t.Fatalf("action edit lost: %+v", detail.Task)
	}
	unchanged := taskRequest(h, http.MethodPatch, path, `{}`, nil)
	if unchanged.Code != 200 {
		t.Fatalf("empty patch contract: %d %s", unchanged.Code, unchanged.Body)
	}
}

func TestTaskMetadataGoalAndClaimUseExistingGoalContext(t *testing.T) {
	s, h := taskAPI(t)
	goal := "current"
	task, err := s.InsertTask(context.Background(), storage.TaskInsert{Project: "tasks", Title: "Move queue", Reporter: "user", Goal: &goal}, apiActor())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/v1/projects/tasks/tasks/"+task.ID, strings.NewReader(`{"status":"in_progress","goal":"next"}`))
	req.Header.Set("Authorization", "Bearer task-key")
	actor := apiActor()
	req.Header.Set("X-Agent-Session-Id", actor.SessionID)
	req.Header.Set("X-Agent-Instance-Id", actor.InstanceID)
	req.Header.Set("X-Agent-Provider", actor.Provider)
	req.Header.Set("X-Agent-OS", actor.OS)
	req.Header.Set("X-Task-Goal", goal)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	if response.Code != 200 {
		t.Fatalf("authorized combined goal/claim: %d %s", response.Code, response.Body)
	}
	detail, err := s.Task(context.Background(), "tasks", task.ID)
	if err != nil || detail.Task.Status != "in_progress" || detail.Task.Goal == nil || *detail.Task.Goal != "next" {
		t.Fatalf("combined goal/claim result: %+v %v", detail, err)
	}
}

func TestTaskMetadataGuidedCompletionDoesNotCommitPreview(t *testing.T) {
	s, h := taskAPI(t)
	task, err := s.InsertTask(context.Background(), storage.TaskInsert{Project: "tasks", Title: "Original", Reporter: "user"}, apiActor())
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/tasks/tasks/" + task.ID
	response := taskRequest(h, http.MethodPatch, path, `{"status":"done","title":"Confirmed title","specification":"Confirmed plan"}`, apiActor())
	if response.Code != 409 {
		t.Fatalf("guided completion: %d %s", response.Code, response.Body)
	}
	detail, err := s.Task(context.Background(), "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Title != "Original" || detail.Task.Details != nil || detail.Task.Status == "done" {
		t.Fatalf("preview escaped: %+v", detail.Task)
	}
	response = taskRequest(h, http.MethodPost, path+"/confirmation", `{"changeset":null}`, apiActor())
	if response.Code != 200 {
		t.Fatalf("confirmation: %d %s", response.Code, response.Body)
	}
	detail, err = s.Task(context.Background(), "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Title != "Confirmed title" || detail.Task.Details == nil || *detail.Task.Details != "Confirmed plan" || detail.Task.Status != "done" {
		t.Fatalf("confirmation lost metadata: %+v", detail.Task)
	}
}
