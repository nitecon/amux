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

	"github.com/google/uuid"
	"github.com/nitecon/amux/storage"
)

const delegationParent = "44444444-4444-4444-8444-444444444444"

func delegationTestReceipt() map[string]any {
	source, target := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	return map[string]any{
		"source_task": map[string]any{"id": source, "project_ident": "demo", "status": "todo", "kind": "delegated", "delegated_to_project_ident": "other", "delegated_to_task_id": target, "parent": map[string]any{"id": delegationParent, "project_ident": "demo"}},
		"target_task": map[string]any{"id": target, "project_ident": "other", "status": "todo", "kind": "normal", "parent": map[string]any{"id": source, "project_ident": "demo"}},
		"delegation":  map[string]any{"id": "33333333-3333-4333-8333-333333333333", "source_project_ident": "demo", "source_task_id": source, "target_project_ident": "other", "target_task_id": target},
		"message_id":  42,
	}
}

func delegationTestActor() storage.AgentOrigin {
	return storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
}

func TestDelegationPreservesLiteralContextAndRequiresExactLinkedReceipt(t *testing.T) {
	cases := []struct {
		name, object, field string
		value               any
	}{
		{"valid", "", "", nil},
		{"missing source ID", "source_task", "id", nil},
		{"nil target ID", "target_task", "id", uuid.Nil.String()},
		{"wrong source project", "source_task", "project_ident", "wrong"},
		{"wrong target project", "target_task", "project_ident", "wrong"},
		{"unknown source status", "source_task", "status", "unknown"},
		{"invalid link ID", "delegation", "id", "not-a-uuid"},
		{"wrong target link", "delegation", "target_task_id", "11111111-1111-4111-8111-111111111111"},
		{"wrong source link project", "delegation", "source_project_ident", "other"},
		{"missing source target", "source_task", "delegated_to_task_id", nil},
		{"wrong source parent", "source_task", "parent", map[string]any{"id": uuid.NewString(), "project_ident": "demo"}},
		{"wrong target parent", "target_task", "parent", map[string]any{"id": delegationParent, "project_ident": "demo"}},
		{"zero message", "", "message_id", 0},
		{"null receipt", "", "", nil},
		{"HTTP refusal", "", "", nil},
	}
	actor := delegationTestActor()
	parent, literal := delegationParent, "line one\n$(literal text) <b>context</b>"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != "POST" || r.URL.Path != "/v1/projects/demo/tasks/delegate" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				for key, want := range map[string]string{"X-Agent-Session-Id": actor.SessionID, "X-Agent-Instance-Id": actor.InstanceID, "X-Agent-Provider": "codex", "X-Agent-Os": "linux"} {
					if r.Header.Get(key) != want {
						t.Errorf("%s = %q", key, r.Header.Get(key))
					}
				}
				var body TaskDelegateInput
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Title != "Review" || body.TargetProject != "other" || body.ParentID == nil || *body.ParentID != parent || body.Specification == nil || *body.Specification != literal {
					t.Errorf("invalid handoff body %+v", body)
				}
				if tc.name == "HTTP refusal" {
					w.WriteHeader(http.StatusConflict)
					fmt.Fprint(w, `{"error":"synthetic secret must not leak"}`)
					return
				}
				if tc.name == "null receipt" {
					fmt.Fprint(w, "null")
					return
				}
				receipt := delegationTestReceipt()
				if tc.field != "" {
					if tc.object == "" {
						receipt[tc.field] = tc.value
					} else {
						receipt[tc.object].(map[string]any)[tc.field] = tc.value
					}
				}
				json.NewEncoder(w).Encode(receipt)
			}))
			defer host.Close()
			client, err := New(host.URL, "fixture-key", host.Client())
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.DelegateTask(context.Background(), "demo", TaskDelegateInput{TargetProject: "other", Title: "  Review  ", ParentID: &parent, Specification: &literal}, &actor, nil)
			if tc.name == "valid" {
				if err != nil || result == nil {
					t.Fatalf("valid receipt = %+v, %v", result, err)
				}
			} else if err == nil || result != nil || !strings.Contains(err.Error(), "inspect") || strings.Contains(err.Error(), "synthetic secret") {
				t.Fatalf("uncertain receipt = %+v, %v", result, err)
			}
			if requests.Load() != 1 {
				t.Fatalf("unknown acceptance retried: %d requests", requests.Load())
			}
		})
	}
}

func TestOldGatewayFailsHierarchyPreflightWithoutCreation(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "GET" || r.URL.Path != "/v1/tasks/capabilities" {
			t.Errorf("creation attempted: %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"requester_subscriptions":1}`)
	}))
	defer host.Close()
	client, _ := New(host.URL, "fixture-key", host.Client())
	err := client.RequireTaskHierarchy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "No task creation sent") || requests.Load() != 1 {
		t.Fatalf("preflight = %v requests=%d", err, requests.Load())
	}
}

func TestDelegationRequesterBelongsToSourceAndRequiresExactAcknowledgement(t *testing.T) {
	actor := delegationTestActor()
	requester := storage.RegisteredTargetIdentity{Origin: actor, Generation: uuid.NewString()}
	parent := delegationParent
	for _, acknowledged := range []bool{true, false} {
		t.Run(fmt.Sprint(acknowledged), func(t *testing.T) {
			var reads, writes atomic.Int32
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/v1/tasks/capabilities" {
					reads.Add(1)
					fmt.Fprint(w, `{"requester_subscriptions":1}`)
					return
				}
				writes.Add(1)
				var body TaskDelegateInput
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Requester == nil || *body.Requester != requester {
					t.Errorf("source requester lost: %+v", body)
				}
				receipt := delegationTestReceipt()
				if acknowledged {
					receipt["source_task"].(map[string]any)["requester"] = requester
				}
				json.NewEncoder(w).Encode(receipt)
			}))
			defer host.Close()
			client, _ := New(host.URL, "fixture-key", host.Client())
			_, err := client.DelegateTask(context.Background(), "demo", TaskDelegateInput{TargetProject: "other", Title: "Review", ParentID: &parent, Requester: &requester}, &actor, nil)
			if (err == nil) != acknowledged || reads.Load() != 1 || writes.Load() != 1 {
				t.Fatalf("ack=%v result=%v reads=%d writes=%d", acknowledged, err, reads.Load(), writes.Load())
			}
		})
	}
}

func TestInvalidDelegationsSendNothing(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer host.Close()
	client, _ := New(host.URL, "fixture-key", host.Client())
	actor := delegationTestActor()
	parent := delegationParent
	large := strings.Repeat("x", 65537)
	for _, input := range []TaskDelegateInput{
		{TargetProject: "demo", Title: "Review", ParentID: &parent},
		{TargetProject: "DEMO", Title: "Review", ParentID: &parent},
		{TargetProject: "../other", Title: "Review", ParentID: &parent},
		{TargetProject: "other", Title: " ", ParentID: &parent},
		{TargetProject: "other", Title: strings.Repeat("a", 1025), ParentID: &parent},
		{TargetProject: "other", Title: "Review\nnow", ParentID: &parent},
		{TargetProject: "other", Title: "Review", ParentID: &parent, Specification: &large},
		{TargetProject: "other", Title: "Review"},
		{TargetProject: "other", Title: "Review", ParentID: &parent, Requester: &storage.RegisteredTargetIdentity{Origin: delegationTestActor(), Generation: uuid.NewString()}},
	} {
		if _, err := client.DelegateTask(context.Background(), "demo", input, &actor, nil); err == nil {
			t.Errorf("invalid delegation accepted: %+v", input)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid request reached network: %d", requests.Load())
	}
}
