package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestProjectMessageThreadFilterPreservesSourceQuerySemantics(t *testing.T) {
	for _, tc := range []struct {
		name      string
		target    string
		limit     uint64
		offset    uint64
		state     string
		kinds     []string
		agentID   *string
		wantError bool
	}{
		{name: "defaults", target: "/?", limit: 50, offset: 0, state: ""},
		{name: "explicit zero", target: "/?limit=0", limit: 0},
		{name: "unsigned leading plus", target: "/?limit=%2B5&offset=%2B2", limit: 5, offset: 2},
		{name: "cap and filters", target: "/?state=unknown&kind=%20HUMAN%20,,Bot,&agent_id=&limit=900&offset=7", limit: 500, offset: 7, state: "unknown", kinds: []string{"human", "bot"}, agentID: stringPtr("")},
		{name: "invalid limit", target: "/?limit=no", wantError: true},
		{name: "invalid offset", target: "/?offset=-1", wantError: true},
		{name: "duplicate limit", target: "/?limit=2&limit=3", wantError: true},
		{name: "duplicate kind", target: "/?kind=human&kind=agent", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.target, nil)
			filter, err := projectMessageThreadFilter(r)
			if tc.wantError {
				if err == nil {
					t.Fatal("invalid query was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if filter.Limit != tc.limit || filter.Offset != tc.offset || filter.State != tc.state || !reflect.DeepEqual(filter.Kinds, tc.kinds) || !reflect.DeepEqual(filter.AgentID, tc.agentID) {
				t.Fatalf("filter=%+v, want limit=%d offset=%d state=%q kinds=%v agent=%v", filter, tc.limit, tc.offset, tc.state, tc.kinds, tc.agentID)
			}
		})
	}
}

func TestProjectMessageListRouteReturnsFilteredThreadsAndProjectCounts(t *testing.T) {
	store, handler := taskAPI(t)
	ctx := context.Background()
	resolved, err := store.CreateHumanProjectMessage(ctx, "tasks", storage.HumanProjectMessageInput{Content: "resolved"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateHumanProjectMessage(ctx, "tasks", storage.HumanProjectMessageInput{Content: "resolution reply", ParentMessageID: &resolved.ID, Resolve: true}); err != nil {
		t.Fatal(err)
	}
	root, err := store.CreateHumanProjectMessage(ctx, "tasks", storage.HumanProjectMessageInput{Content: "question"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateHumanProjectMessage(ctx, "tasks", storage.HumanProjectMessageInput{Content: "answer", ParentMessageID: &root.ID}); err != nil {
		t.Fatal(err)
	}
	response := taskRequest(handler, http.MethodGet, "/v1/projects/tasks/messages?state=all&kind=%20HUMAN%20,,&limit=0&offset=0", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	var result storage.ProjectMessageThreadList
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	states := map[int64]string{}
	for _, thread := range result.Threads {
		states[thread.ID] = thread.State
	}
	if len(result.Threads) != 2 || states[root.ID] != "answered" || states[resolved.ID] != "resolved" {
		t.Fatalf("threads=%+v", result.Threads)
	}
	if result.Counts.HumanOpen != 1 || result.Counts.Resolved != 1 {
		t.Fatalf("counts=%+v", result.Counts)
	}
	unknown := taskRequest(handler, http.MethodGet, "/v1/projects/tasks/messages?state=misspelled", "", nil)
	if unknown.Code != http.StatusOK {
		t.Fatalf("unknown state status=%d body=%s", unknown.Code, unknown.Body.String())
	}
	if err := json.Unmarshal(unknown.Body.Bytes(), &result); err != nil || len(result.Threads) != 1 || result.Threads[0].ID != root.ID {
		t.Fatalf("unknown state result=%+v err=%v", result, err)
	}
	emptyAgent := taskRequest(handler, http.MethodGet, "/v1/projects/tasks/messages?agent_id=", "", nil)
	if emptyAgent.Code != http.StatusOK {
		t.Fatalf("empty agent_id status=%d body=%s", emptyAgent.Code, emptyAgent.Body.String())
	}
	if err := json.Unmarshal(emptyAgent.Body.Bytes(), &result); err != nil || len(result.Threads) != 0 || result.Counts.HumanOpen != 1 {
		t.Fatalf("empty agent_id result=%+v err=%v", result, err)
	}
	if result.Threads == nil {
		t.Fatal("empty result thread list must encode as []")
	}
}

func TestProjectMessageListRouteRejectsInvalidPaginationAndMissingProject(t *testing.T) {
	_, handler := taskAPI(t)
	for _, tc := range []struct {
		path string
		want int
	}{
		{path: "/v1/projects/tasks/messages?limit=-1", want: http.StatusBadRequest},
		{path: "/v1/projects/tasks/messages?offset=18446744073709551616", want: http.StatusBadRequest},
		{path: "/v1/projects/missing/messages", want: http.StatusNotFound},
		{path: "/v1/projects/missing/messages?limit=invalid", want: http.StatusBadRequest},
	} {
		response := taskRequest(handler, http.MethodGet, tc.path, "", nil)
		if response.Code != tc.want {
			t.Errorf("GET %s status=%d body=%s, want %d", tc.path, response.Code, response.Body.String(), tc.want)
		}
	}
	response := taskRequest(handler, http.MethodGet, "/v1/projects/tasks/messages?limit=501&offset="+strconv.FormatUint(^uint64(0), 10), "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("large source-compatible offset status=%d body=%s", response.Code, response.Body.String())
	}
}
