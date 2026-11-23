package gatewayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestCallProjectMessageThreadValidatesSingleTextInOneRequest(t *testing.T) {
	var calls atomic.Int32
	thread := storage.ProjectMessageThread{
		Root:  storage.ProjectMessage{ID: 41, ProjectIdent: "tasks", Content: "full original"},
		State: "open", Replies: []storage.ProjectMessage{}, Confirmations: []storage.ProjectMessageConfirmation{},
	}
	encoded, err := json.Marshal(thread)
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/projects/tasks/conversations/conversation/tools" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var request struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Name != "project_message_thread" || string(request.Arguments) != `{"id":41}` {
			t.Errorf("tool request=%+v err=%v", request, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}]}`, encoded)
	}))
	defer host.Close()
	client, err := New(host.URL, "tool-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.CallTaskTool(context.Background(), "tasks", "conversation", storage.AgentOrigin{}, "project_message_thread", json.RawMessage(`{"id":41}`))
	if err != nil || result.IsError || calls.Load() != 1 || len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, "full original") {
		t.Fatalf("thread result=%+v err=%v requests=%d", result, err, calls.Load())
	}
}

func TestCallProjectMessageThreadRejectsMalformedSuccessfulResponses(t *testing.T) {
	valid := `{"root":{"id":41,"project_ident":"tasks","content":"root"},"state":"open","replies":[],"confirmations":[]}`
	for _, tc := range []struct {
		name  string
		body  string
		valid bool
	}{
		{name: "multiple text items", body: `{"content":[{"type":"text","text":` + fmt.Sprintf("%q", valid) + `},{"type":"text","text":"extra"}]}`},
		{name: "wrong ID", body: `{"content":[{"type":"text","text":"{\"root\":{\"id\":42,\"project_ident\":\"tasks\"},\"state\":\"open\",\"replies\":[],\"confirmations\":[]}"}]}`},
		{name: "wrong project", body: `{"content":[{"type":"text","text":"{\"root\":{\"id\":41,\"project_ident\":\"other\"},\"state\":\"open\",\"replies\":[],\"confirmations\":[]}"}]}`},
		{name: "valid error result", body: `{"isError":true,"content":[{"type":"text","text":"Project message not found in this project."}]}`, valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer host.Close()
			client, err := New(host.URL, "tool-key", nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.CallTaskTool(context.Background(), "tasks", "conversation", storage.AgentOrigin{}, "project_message_thread", json.RawMessage(`{"id":41}`))
			if tc.valid {
				if err != nil || !result.IsError || calls.Load() != 1 {
					t.Fatalf("server tool error result=%+v err=%v calls=%d", result, err, calls.Load())
				}
			} else if err == nil || calls.Load() != 1 {
				t.Fatalf("malformed success accepted result=%+v err=%v calls=%d", result, err, calls.Load())
			}
		})
	}
}

func TestCallProjectMessageThreadRejectsInvalidLocalIDWithoutRequest(t *testing.T) {
	var calls atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer host.Close()
	client, err := New(host.URL, "tool-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{`{"id":0}`, `{"id":"41"}`, `{}`} {
		if _, err := client.CallTaskTool(context.Background(), "tasks", "conversation", storage.AgentOrigin{}, "project_message_thread", json.RawMessage(args)); err == nil {
			t.Errorf("accepted invalid local arguments %s", args)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("sent requests for invalid local IDs: %d", calls.Load())
	}
}
