package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestProjectMessageThreadRouteResolvesReplyAndReturnsTupleConfirmations(t *testing.T) {
	store, _ := taskAPI(t)
	ctx := context.Background()
	root, err := store.CreateHumanProjectMessage(ctx, "tasks", storage.HumanProjectMessageInput{Content: "root question"})
	if err != nil {
		t.Fatal(err)
	}
	rootID := root.ID
	reply, err := store.CreateHumanProjectMessage(ctx, "tasks", storage.HumanProjectMessageInput{Content: "answer", ParentMessageID: &rootID})
	if err != nil {
		t.Fatal(err)
	}
	replyID := reply.ID
	if _, err := store.ConfirmProjectMessage(ctx, "tasks", "reader", rootID); err != nil {
		t.Fatal(err)
	}

	api := newAPI(store, "task-key")
	mux := http.NewServeMux()
	api.registerProjectMessageThreadRoutes(mux)
	request := httptest.NewRequest(http.MethodGet, "/v1/projects/tasks/messages/"+strconv.FormatInt(replyID, 10)+"/thread", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("thread status=%d body=%s", response.Code, response.Body.String())
	}
	var thread storage.ProjectMessageThread
	if err := json.Unmarshal(response.Body.Bytes(), &thread); err != nil {
		t.Fatal(err)
	}
	if thread.Root.ID != rootID || thread.Root.Content != "root question" || thread.State != "answered" || len(thread.Replies) != 1 || thread.Replies[0].ID != replyID ||
		len(thread.Confirmations) != 1 || thread.Confirmations[0].AgentID != "reader" || thread.Confirmations[0].ConfirmedAt <= 0 {
		t.Fatalf("thread response=%+v", thread)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	var tuples [][]json.RawMessage
	if err := json.Unmarshal(wire["confirmations"], &tuples); err != nil || len(tuples) != 1 || len(tuples[0]) != 2 {
		t.Fatalf("confirmation tuples=%s err=%v", wire["confirmations"], err)
	}
}

func TestProjectMessageThreadRouteRejectsInvalidAndUnknownIDs(t *testing.T) {
	store, _ := taskAPI(t)
	api := newAPI(store, "task-key")
	mux := http.NewServeMux()
	api.registerProjectMessageThreadRoutes(mux)
	for _, tc := range []struct {
		path string
		want int
	}{
		{path: "/v1/projects/tasks/messages/nope/thread", want: http.StatusBadRequest},
		{path: "/v1/projects/tasks/messages/0/thread", want: http.StatusBadRequest},
		{path: "/v1/projects/foreign/messages/1/thread", want: http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if response.Code != tc.want {
			t.Errorf("GET %s status=%d body=%s, want %d", tc.path, response.Code, response.Body.String(), tc.want)
		}
	}
}
