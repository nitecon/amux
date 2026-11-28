package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nitecon/amux/auth"
	"github.com/nitecon/amux/storage"
)

func TestWorkerAssignmentReuseRequiresRecordedPrincipal(t *testing.T) {
	ctx := context.Background()
	s, _ := taskAPI(t)
	actor := apiActor()
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Follow up", Reporter: "user"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", actor); err != nil {
		t.Fatal(err)
	}
	a := newAPI(s, "task-key")
	mux := http.NewServeMux()
	a.registerWorkerAssignmentRoutes(mux)
	adopt := func(principal, taskID string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]string{"task_id": taskID})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/v1/projects/tasks/worker-assignment", strings.NewReader(string(body)))
		r.Header.Set("X-Agent-Session-Id", actor.SessionID)
		r.Header.Set("X-Agent-Instance-Id", actor.InstanceID)
		r.Header.Set("X-Agent-Provider", actor.Provider)
		r.Header.Set("X-Agent-OS", actor.OS)
		r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	readAssignment := func(w *httptest.ResponseRecorder) storage.WorkerAssignment {
		t.Helper()
		var assignment storage.WorkerAssignment
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &assignment) != nil || assignment.RunID == "" || assignment.Conversation.ID == "" || assignment.Task.ID != task.ID {
			t.Fatalf("adoption: status=%d body=%s", w.Code, w.Body)
		}
		return assignment
	}
	first := readAssignment(adopt("node-a", task.ID))
	// Both explicit-task and automatic adoption must reject a different enrolled
	// client even when it supplies the owner's exact origin headers.
	for _, taskID := range []string{task.ID, ""} {
		w := adopt("node-b", taskID)
		if w.Code != http.StatusForbidden {
			t.Fatalf("foreign principal task=%q: status=%d body=%s", taskID, w.Code, w.Body)
		}
	}
	second := readAssignment(adopt("node-a", task.ID))
	if second.RunID != first.RunID || second.Conversation.ID != first.Conversation.ID {
		t.Fatalf("owner retry changed assignment: first=%+v second=%+v", first, second)
	}
}
