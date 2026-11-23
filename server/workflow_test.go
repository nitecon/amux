package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestGuidedTaskCompletionHTTP(t *testing.T) {
	_, h := taskAPI(t)
	actor := apiActor()
	created := taskRequest(h, "POST", "/v1/projects/tasks/tasks", `{"title":"Finish work","root_reason":"user-directed","specification":"Complete the feature"}`, actor)
	var task storage.Task
	if err := json.Unmarshal(created.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/tasks/tasks/" + task.ID
	question := taskRequest(h, "PATCH", path, `{"status":"done"}`, actor)
	if question.Code != 409 || !strings.Contains(question.Body.String(), "task has not moved") {
		t.Fatalf("question %d %s", question.Code, question.Body.String())
	}
	mutation := taskRequest(h, "POST", path+"/confirmation", `{"task_type":"software"}`, actor)
	if mutation.Code != 200 || !strings.Contains(mutation.Body.String(), "task_set_changeset") {
		t.Fatalf("mutation confirmation %d %s", mutation.Code, mutation.Body.String())
	}
	done := taskRequest(h, "POST", path+"/confirmation", `{"changeset":["main.go"]}`, actor)
	if done.Code != 200 {
		t.Fatalf("completion %d %s", done.Code, done.Body.String())
	}
	if err := json.Unmarshal(done.Body.Bytes(), &task); err != nil || task.Status != "done" || len(task.Changeset) != 1 {
		t.Fatalf("done %+v, %v", task, err)
	}
	reviews := taskRequest(h, "GET", "/v1/projects/tasks/tasks?all_goals=true", "", nil)
	if reviews.Code != 200 || !strings.Contains(reviews.Body.String(), "Documentation review: Finish work") || !strings.Contains(reviews.Body.String(), "Review: Finish work") {
		t.Fatalf("review queue %d %s", reviews.Code, reviews.Body.String())
	}
}

func TestHTTPPeerCompletionAndParking(t *testing.T) {
	_, h := taskAPI(t)
	actor := apiActor()
	created := taskRequest(h, "POST", "/v1/projects/tasks/tasks", `{"title":"Parked work","root_reason":"user-directed","specification":"Await a decision"}`, actor)
	var task storage.Task
	json.Unmarshal(created.Body.Bytes(), &task)
	path := "/v1/projects/tasks/tasks/" + task.ID
	parked := taskRequest(h, "PATCH", path, `{"status":"user_review","review_action":"Approve the release","changeset":null}`, actor)
	if parked.Code != 200 {
		t.Fatalf("parking %d %s", parked.Code, parked.Body.String())
	}
	peer := *actor
	peer.SessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	peer.Provider = "claude"
	done := taskRequest(h, "PATCH", path, `{"status":"done","changeset":null}`, &peer)
	if done.Code != 200 {
		t.Fatalf("peer completion %d %s", done.Code, done.Body.String())
	}
	if err := json.Unmarshal(done.Body.Bytes(), &task); err != nil || task.OwnerOrigin == nil || *task.OwnerOrigin != *actor {
		t.Fatalf("implementer changed: %+v, %v", task, err)
	}
}
