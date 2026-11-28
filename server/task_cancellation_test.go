package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

// Rust task_cancel.rs::http_contract and replay_is_idempotent_and_cancelled_can_only_move_to_done.
func TestTaskCancellationHTTPPreservesClaimAndReplayReport(t *testing.T) {
	s, h := taskAPI(t)
	ctx := context.Background()
	actor := apiActor()
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Cancel this work", Reporter: "user"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/tasks/tasks/" + task.ID + "/cancellation"
	before, err := s.TaskEvents(ctx, storage.EventQuery{TaskID: &task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if response := taskRequest(h, "POST", path, `{"reason":"Stop work","changeset":null}`, nil); response.Code != 400 {
		t.Fatalf("missing origin: %d %s", response.Code, response.Body)
	}
	missing := taskRequest(h, "POST", path, `{"reason":"Stop work"}`, actor)
	if missing.Code != 409 || !strings.Contains(missing.Body.String(), "changeset_confirmation_required") {
		t.Fatalf("missing changeset: %d %s", missing.Code, missing.Body)
	}
	if response := taskRequest(h, "GET", path, "", nil); response.Code != 404 {
		t.Fatalf("failed cancel created report: %d %s", response.Code, response.Body)
	}
	peer := *actor
	peer.InstanceID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if response := taskRequest(h, "POST", path, `{"reason":"Stop work","changeset":null}`, &peer); response.Code != 409 {
		t.Fatalf("stale owner cancelled: %d %s", response.Code, response.Body)
	}
	response := taskRequest(h, "POST", path, `{"reason":"  Stop work  ","changeset":["main.go"]}`, actor)
	if response.Code != 200 {
		t.Fatalf("cancel: %d %s", response.Code, response.Body)
	}
	var result struct {
		Task         storage.Task `json:"task"`
		Cancellation struct {
			TaskID      string        `json:"task_id"`
			Reason      string        `json:"reason"`
			PriorStatus string        `json:"prior_status"`
			PriorOwner  *agent.Origin `json:"prior_owner_origin"`
			Reporter    agent.Origin  `json:"reporter_origin"`
			Stopped     bool          `json:"executor_stop_confirmed"`
		} `json:"cancellation"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Task.Status != "cancelled" || result.Task.OwnerOrigin == nil || *result.Task.OwnerOrigin != *actor || len(result.Task.Changeset) != 1 || result.Cancellation.TaskID != task.ID || result.Cancellation.Reason != "Stop work" || result.Cancellation.PriorStatus != "todo" || result.Cancellation.PriorOwner == nil || *result.Cancellation.PriorOwner != *actor || result.Cancellation.Reporter != *actor || result.Cancellation.Stopped {
		t.Fatalf("cancellation lost provenance: %+v", result)
	}
	report := taskRequest(h, "GET", path, "", nil)
	if report.Code != 200 || strings.Contains(report.Body.String(), `"executor_stop_confirmed":true`) {
		t.Fatalf("report: %d %s", report.Code, report.Body)
	}
	replay := taskRequest(h, "POST", path, `{"reason":"Stop work"}`, actor)
	if replay.Code != 200 {
		t.Fatalf("exact retry: %d %s", replay.Code, replay.Body)
	}
	after, err := s.TaskEvents(ctx, storage.EventQuery{TaskID: &task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 || after[0].Kind != "task_cancelled" {
		t.Fatalf("replay emitted extra events: %d -> %d", len(before), len(after))
	}
	var snapshot storage.Task
	if err := json.Unmarshal(after[0].Task, &snapshot); err != nil || len(snapshot.Changeset) != 1 {
		t.Fatalf("cancel event missed changeset: %+v %v", snapshot, err)
	}
	for _, attempt := range []struct {
		body  string
		actor *agent.Origin
	}{{`{"reason":"Different","changeset":null}`, actor}, {`{"reason":"Stop work","changeset":null}`, &peer}} {
		if got := taskRequest(h, "POST", path, attempt.body, attempt.actor); got.Code != 409 {
			t.Fatalf("conflicting replay: %d %s", got.Code, got.Body)
		}
	}
	if got := taskRequest(h, "PATCH", "/v1/projects/tasks/tasks/"+task.ID, `{"status":"todo"}`, nil); got.Code != 409 {
		t.Fatalf("cancelled reopened: %d %s", got.Code, got.Body)
	}
	if got := taskRequest(h, "PATCH", "/v1/projects/tasks/tasks/"+task.ID, `{"status":"done"}`, nil); got.Code != 200 {
		t.Fatalf("cancelled close: %d %s", got.Code, got.Body)
	}
	if got := taskRequest(h, "GET", path, "", nil); got.Code != 200 || strings.Contains(got.Body.String(), `"executor_stop_confirmed":true`) {
		t.Fatalf("report changed after completion: %d %s", got.Code, got.Body)
	}
}

func TestTaskCancellationHTTPRejectsMalformedReports(t *testing.T) {
	s, h := taskAPI(t)
	task, err := s.InsertTask(context.Background(), storage.TaskInsert{Project: "tasks", Title: "Validate cancel", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/tasks/tasks/" + task.ID + "/cancellation"
	for _, body := range []string{`null`, `{}`, `{"reason":null}`, `{"reason":" "}`, `{"reason":4}`, `{"reason":"x","changeset":[null]}`, `{"reason":"x","changeset":null,"executor_stop_confirmed":true}`, `{"reason":"` + strings.Repeat("x", 16385) + `","changeset":null}`} {
		response := taskRequest(h, "POST", path, body, apiActor())
		if response.Code != 400 {
			t.Errorf("bad cancellation: %d %s", response.Code, response.Body)
		}
	}
}
