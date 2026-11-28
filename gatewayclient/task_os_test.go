package gatewayclient

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

const taskOSTestID = "66666666-6666-4666-8666-666666666666"

func TestSetTaskOSRequiresExactActiveOwnerBeforeMutation(t *testing.T) {
	actor := agent.Origin{SessionID: "11111111-1111-4111-8111-111111111111", InstanceID: "22222222-2222-4222-8222-222222222222", Provider: "codex", OS: "linux"}
	for _, tc := range []struct {
		name   string
		status string
		owner  *agent.Origin
	}{
		{name: "other owner", status: "todo", owner: &agent.Origin{SessionID: "33333333-3333-4333-8333-333333333333", InstanceID: actor.InstanceID, Provider: actor.Provider, OS: actor.OS}},
		{name: "inactive", status: "done", owner: &actor},
		{name: "unowned", status: "todo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mutations int
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations++
					t.Errorf("unexpected mutation %s %s", r.Method, r.URL)
				}
				_ = json.NewEncoder(w).Encode(storage.Task{ID: taskOSTestID, ProjectIdent: "demo", Status: tc.status, Labels: []string{"keep"}, OwnerOrigin: tc.owner})
			}))
			defer host.Close()
			client, _ := New(host.URL, "key", host.Client())
			os := "windows"
			if _, err := client.SetTaskOS(context.Background(), "demo", taskOSTestID, &os, actor, nil); err == nil {
				t.Fatal("invalid owner or task state was accepted")
			}
			if mutations != 0 {
				t.Fatalf("mutation count=%d", mutations)
			}
		})
	}
}

func TestSetTaskOSRequiresExplicitNullsInIncompatibleHandoffReceipt(t *testing.T) {
	actor := agent.Origin{SessionID: "11111111-1111-4111-8111-111111111111", InstanceID: "22222222-2222-4222-8222-222222222222", Provider: "codex", OS: "linux"}
	const current = `{"id":"` + taskOSTestID + `","project_ident":"demo","status":"in_progress","labels":["keep","os:linux"],"owner_origin":{"session_id":"11111111-1111-4111-8111-111111111111","instance_id":"22222222-2222-4222-8222-222222222222","provider":"codex","os":"linux"}}`
	const receipt = `{"id":"` + taskOSTestID + `","project_ident":"demo","status":"todo","labels":["keep","os:windows"],"owner_agent_id":null,"owner_origin":null,"started_at":null,"done_at":null,"routing":{"mode":"unassigned"}}`
	for _, tc := range []struct{ name, body, want string }{
		{"omitted owner", strings.Replace(receipt, `,"owner_agent_id":null`, "", 1), "explicitly confirm cleared owner_agent_id"},
		{"retained owner", strings.Replace(receipt, `"owner_origin":null`, `"owner_origin":{"session_id":"11111111-1111-4111-8111-111111111111","instance_id":"22222222-2222-4222-8222-222222222222","provider":"codex","os":"linux"}`, 1), "did not confirm owner release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(current))
					return
				}
				var input storage.TaskWorkflowUpdate
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Errorf("decode update: %v", err)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer host.Close()
			client, _ := New(host.URL, "key", host.Client())
			os := "windows"
			if _, err := client.SetTaskOS(context.Background(), "demo", taskOSTestID, &os, actor, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v; want %q", err, tc.want)
			}
		})
	}
}
