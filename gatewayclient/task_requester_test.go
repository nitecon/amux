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
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/storage"
)

func TestRequesterCreationKeepsExecutorIndependentAndChecksCapabilityBeforePost(t *testing.T) {
	actor := delegationTestActor()
	requester := storage.RegisteredTargetIdentity{Origin: actor, Generation: uuid.NewString()}
	executor := uuid.NewString()
	for _, scenario := range []string{"accepted", "missing capability", "unsupported capability", "missing acknowledgement"} {
		t.Run(scenario, func(t *testing.T) {
			var reads, writes atomic.Int32
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/v1/tasks/capabilities" {
					reads.Add(1)
					if scenario == "missing capability" {
						fmt.Fprint(w, `{}`)
					} else if scenario == "unsupported capability" {
						fmt.Fprint(w, `{"requester_subscriptions":0}`)
					} else {
						fmt.Fprint(w, `{"requester_subscriptions":1}`)
					}
					return
				}
				writes.Add(1)
				if r.Method != "POST" || r.URL.Path != "/v1/projects/demo/tasks" || reads.Load() != 1 || r.Header.Get("X-Agent-Session-Id") != actor.SessionID {
					t.Errorf("wrong creation ordering/scope: %s %s", r.Method, r.URL.Path)
				}
				var body TaskCreateInput
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Requester == nil || *body.Requester != requester || body.Specification == nil || *body.Specification != "literal\ncontext" {
					t.Errorf("requester creation body changed: %+v", body)
				}
				accepted := map[string]any{"id": uuid.NewString(), "project_ident": "demo", "status": "todo", "parent": nil, "owner_agent_id": executor, "requester": requester}
				if scenario == "missing acknowledgement" {
					delete(accepted, "requester")
				}
				json.NewEncoder(w).Encode(accepted)
			}))
			defer host.Close()
			client, _ := New(host.URL, "fixture-key", host.Client())
			literal := "literal\ncontext"
			raw, err := client.TaskCreate(context.Background(), "demo", TaskCreateInput{Title: "Review", Specification: &literal, Requester: &requester}, &actor, nil)
			if scenario == "accepted" {
				if err != nil {
					t.Fatal(err)
				}
				var task storage.Task
				if json.Unmarshal(raw, &task) != nil || task.OwnerAgentID == nil || *task.OwnerAgentID != executor {
					t.Fatalf("subscription replaced executor: %s", raw)
				}
			} else if err == nil {
				t.Fatal("unconfirmed subscription accepted")
			}
			wantWrites := int32(1)
			if scenario == "missing capability" || scenario == "unsupported capability" {
				wantWrites = 0
			}
			if reads.Load() != 1 || writes.Load() != wantWrites {
				t.Fatalf("unexpected mutation/retry: reads=%d writes=%d", reads.Load(), writes.Load())
			}
			if scenario == "missing acknowledgement" && !strings.Contains(err.Error(), "inspect the task before retrying") {
				t.Fatalf("missing uncertainty guidance: %v", err)
			}
		})
	}
}

func TestHumanRequesterSetClearRequireExactResourceAndDoNotImpersonateWatcher(t *testing.T) {
	watcher := storage.RegisteredTargetIdentity{Origin: delegationTestActor(), Generation: uuid.NewString()}
	id := uuid.NewString()
	for _, scenario := range []string{"set", "clear", "wrong project", "wrong task", "wrong requester", "missing requester", "clear missing requester"} {
		t.Run(scenario, func(t *testing.T) {
			var reads, writes atomic.Int32
			var requested *storage.RegisteredTargetIdentity
			if scenario != "clear" && scenario != "clear missing requester" {
				requested = &watcher
			}
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/v1/tasks/capabilities" {
					reads.Add(1)
					fmt.Fprint(w, `{"requester_subscriptions":1}`)
					return
				}
				writes.Add(1)
				if r.Method != "PUT" || r.URL.Path != taskPath("demo")+"/"+id+"/requester" {
					t.Errorf("wrong requester resource: %s %s", r.Method, r.URL.Path)
				}
				for key := range r.Header {
					if strings.HasPrefix(strings.ToLower(key), "x-agent-") {
						t.Errorf("human impersonated watcher through %s", key)
					}
				}
				var body map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 {
					t.Errorf("requester setter body invalid: %+v", body)
				}
				value, present := body["requester"]
				var actual *storage.RegisteredTargetIdentity
				if !present || json.Unmarshal(value, &actual) != nil || (requested == nil && actual != nil) || (requested != nil && (actual == nil || *actual != *requested)) {
					t.Error("requester field omitted or changed in request")
				}
				accepted := map[string]any{"id": id, "project_ident": "demo", "status": "todo", "requester": requested}
				switch scenario {
				case "wrong project":
					accepted["project_ident"] = "other"
				case "wrong task":
					accepted["id"] = uuid.NewString()
				case "wrong requester":
					accepted["requester"] = nil
				case "missing requester", "clear missing requester":
					delete(accepted, "requester")
				}
				json.NewEncoder(w).Encode(accepted)
			}))
			defer host.Close()
			client, _ := New(host.URL, "fixture-key", host.Client())
			_, err := client.SetTaskRequester(context.Background(), "demo", id, requested, nil)
			if scenario == "set" || scenario == "clear" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "inspect") {
				t.Fatalf("unconfirmed requester accepted: %v", err)
			}
			if reads.Load() != 1 || writes.Load() != 1 {
				t.Fatalf("setter retried: reads=%d writes=%d", reads.Load(), writes.Load())
			}
		})
	}
}

func TestRequesterTargetResolvesOnlyUniqueCurrentExactExecution(t *testing.T) {
	actor := delegationTestActor()
	now := time.Now().UnixMilli()
	want := storage.RegisteredTargetIdentity{Origin: actor, Generation: uuid.NewString()}
	entry := func(target storage.RegisteredTargetIdentity) storage.RegisteredTarget {
		return storage.RegisteredTarget{Target: target, SeenAt: now - 1000, ExpiresAt: now + 119000}
	}
	for _, scenario := range []string{"exact", "duplicate", "expired", "same session foreign instance", "no advertisement", "nil generation"} {
		t.Run(scenario, func(t *testing.T) {
			targets := []storage.RegisteredTarget{entry(want)}
			switch scenario {
			case "duplicate":
				targets = append(targets, entry(want))
			case "expired":
				targets[0].SeenAt = now - 121000
				targets[0].ExpiresAt = now - 1000
			case "same session foreign instance":
				targets[0].Target.Origin.InstanceID = uuid.NewString()
			case "no advertisement":
				targets = nil
			case "nil generation":
				targets[0].Target.Generation = uuid.Nil.String()
			}
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != taskTargetsPath("demo") {
					t.Errorf("discovery mutated: %s %s", r.Method, r.URL.Path)
				}
				json.NewEncoder(w).Encode(storage.TargetDirectory{Targets: targets})
			}))
			defer host.Close()
			client, _ := New(host.URL, "fixture-key", host.Client())
			got, err := client.RequesterTarget(context.Background(), "demo", actor)
			if scenario == "exact" {
				if err != nil || got == nil || *got != want {
					t.Fatalf("exact requester=%+v err=%v", got, err)
				}
			} else if err == nil {
				t.Fatalf("ambiguous/unavailable requester accepted: %+v", got)
			}
		})
	}
}

func TestInvalidRequesterMutationSendsNoRequests(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer host.Close()
	client, _ := New(host.URL, "fixture-key", host.Client())
	actor := delegationTestActor()
	for _, target := range []storage.RegisteredTargetIdentity{{Origin: actor, Generation: uuid.Nil.String()}, {Origin: actor, Generation: "bad"}, {Origin: delegationTestActor(), Generation: uuid.NewString()}} {
		if _, err := client.SetTaskRequester(context.Background(), "demo", uuid.NewString(), &target, &actor); err == nil {
			t.Fatal("invalid setter accepted")
		}
		if _, err := client.TaskCreate(context.Background(), "demo", TaskCreateInput{Title: "Review", Requester: &target}, &actor, nil); err == nil {
			t.Fatal("invalid creation accepted")
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid requester reached gateway %d times", requests.Load())
	}
}
