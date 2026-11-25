package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/amux/storage"
)

func TestReassignTaskHTTPRequiresCallerAndReturnsExactTask(t *testing.T) {
	store, handler := taskAPI(t)
	ctx := context.Background()
	actor := apiActor()
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Reassign", Reporter: "user"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	targetOrigin := *apiActor()
	targetOrigin.SessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	targetOrigin.InstanceID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	identity := storage.RegisteredTargetIdentity{Origin: targetOrigin, Generation: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"}
	if _, err := store.RegisterTarget(ctx, "tasks", storage.TargetRegistration{Origin: targetOrigin, Generation: identity.Generation}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/tasks/tasks/" + task.ID + "/reassign"
	body, err := json.Marshal(map[string]any{"target": identity})
	if err != nil {
		t.Fatal(err)
	}
	if response := taskRequest(handler, http.MethodPost, path, string(body), nil); response.Code != http.StatusBadRequest {
		t.Fatalf("missing caller origin status=%d body=%s", response.Code, response.Body.String())
	}
	for _, invalid := range []string{`null`, `{}`, `{"target":null}`, `{"target":{"origin":{},"generation":"` + identity.Generation + `"}}`, `{"target":{"origin":{},"generation":"` + identity.Generation + `"},"extra":true}`} {
		if response := taskRequest(handler, http.MethodPost, path, invalid, actor); response.Code != http.StatusBadRequest {
			t.Errorf("invalid request %s status=%d body=%s", invalid, response.Code, response.Body.String())
		}
	}
	other := *actor
	other.InstanceID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	if response := taskRequest(handler, http.MethodPost, path, string(body), &other); response.Code != http.StatusConflict {
		t.Fatalf("wrong caller origin status=%d body=%s", response.Code, response.Body.String())
	}
	stale := identity
	stale.Generation = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	staleBody, err := json.Marshal(map[string]any{"target": stale})
	if err != nil {
		t.Fatal(err)
	}
	if response := taskRequest(handler, http.MethodPost, path, string(staleBody), actor); response.Code != http.StatusConflict {
		t.Fatalf("stale target generation status=%d body=%s", response.Code, response.Body.String())
	}
	response := taskRequest(handler, http.MethodPost, path, string(body), actor)
	if response.Code != http.StatusOK {
		t.Fatalf("reassign status=%d body=%s", response.Code, response.Body.String())
	}
	var updated storage.Task
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.ID != task.ID || updated.Status != "todo" || updated.OwnerOrigin == nil || *updated.OwnerOrigin != targetOrigin {
		t.Fatalf("reassignment response=%+v", updated)
	}
	var routing struct {
		Mode   string                           `json:"mode"`
		Target storage.RegisteredTargetIdentity `json:"target"`
	}
	if err := json.Unmarshal(updated.Routing, &routing); err != nil || routing.Mode != "session" || routing.Target != identity {
		t.Fatalf("reassignment route=%s err=%v", updated.Routing, err)
	}
	claimPath := strings.TrimSuffix(path, "/reassign")
	const claim = `{"status":"in_progress"}`
	if response := taskRequest(handler, http.MethodPatch, claimPath, claim, actor); response.Code != http.StatusConflict {
		t.Fatalf("former owner claim status=%d body=%s", response.Code, response.Body.String())
	}
	response = taskRequest(handler, http.MethodPatch, claimPath, claim, &targetOrigin)
	if response.Code != http.StatusOK {
		t.Fatalf("recipient claim status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestReassignTaskHTTPUsesBearerAuthentication(t *testing.T) {
	_, handler := taskAPI(t)
	request := httptest.NewRequest(http.MethodPost, "/v1/projects/tasks/tasks/00000000-0000-4000-8000-000000000001/reassign", strings.NewReader(`{"target":{}}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestHumanAssignTaskHTTPRequiresLiveCompatibleTargetAndAuditsUser(t *testing.T) {
	store, handler := taskAPI(t)
	ctx := context.Background()
	actor := apiActor()
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Manual assign", Reporter: "user"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	targetOrigin := *apiActor()
	targetOrigin.SessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	targetOrigin.InstanceID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	identity := storage.RegisteredTargetIdentity{Origin: targetOrigin, Generation: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"}
	now := time.Now().UTC()
	if _, err := store.RegisterTarget(ctx, "tasks", storage.TargetRegistration{Origin: targetOrigin, Generation: identity.Generation}, now); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"target": identity})
	path := "/v1/projects/tasks/tasks/" + task.ID + "/assign"

	withAgent := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	withAgent.Header.Set("Authorization", "Bearer task-key")
	withAgent.Header.Set("X-Agent-Session-Id", actor.SessionID)
	withAgent.Header.Set("X-Agent-Instance-Id", actor.InstanceID)
	withAgent.Header.Set("X-Agent-Provider", actor.Provider)
	withAgent.Header.Set("X-Agent-Os", actor.OS)
	agentResponse := httptest.NewRecorder()
	handler.ServeHTTP(agentResponse, withAgent)
	if agentResponse.Code != http.StatusBadRequest {
		t.Fatalf("agent identity status=%d body=%s", agentResponse.Code, agentResponse.Body.String())
	}

	stale := identity
	stale.Generation = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	staleBody, _ := json.Marshal(map[string]any{"target": stale})
	if response := taskRequest(handler, http.MethodPost, path, string(staleBody), nil); response.Code != http.StatusConflict {
		t.Fatalf("stale target status=%d body=%s", response.Code, response.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer task-key")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Agent-Id", "_default")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("human assignment status=%d body=%s", response.Code, response.Body.String())
	}
	var assigned storage.Task
	if err := json.Unmarshal(response.Body.Bytes(), &assigned); err != nil || assigned.OwnerOrigin == nil || *assigned.OwnerOrigin != targetOrigin {
		t.Fatalf("assigned task=%+v decode err=%v", assigned, err)
	}
	detail, err := store.TaskRecent(ctx, "tasks", task.ID, nil)
	if err != nil || len(detail.Comments) == 0 {
		t.Fatal(err)
	}
	comment := detail.Comments[0]
	if comment.Author != "user" || comment.AuthorType != "user" || !strings.Contains(comment.Content, "Previous assignment:") || !strings.Contains(comment.Content, "Instance at handoff:") {
		t.Fatalf("assignment audit comment=%+v", comment)
	}
}
