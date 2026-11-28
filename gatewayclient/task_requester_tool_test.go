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

func TestRequesterToolsRequireCapabilityAndExactSelfAcknowledgement(t *testing.T) {
	for _, name := range []string{"task_create", "task_delegate", "task_set_requester"} {
		for _, scenario := range []string{"accepted", "missing capability", "missing acknowledgement"} {
			t.Run(name+"/"+scenario, func(t *testing.T) {
				actor := delegationTestActor()
				id := uuid.NewString()
				conversationID := uuid.NewString()
				target := storage.RegisteredTargetIdentity{Origin: actor, Generation: uuid.NewString()}
				var caps, targets, writes atomic.Int32
				host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "GET" && r.URL.Path == "/v1/tasks/capabilities" {
						caps.Add(1)
						if scenario == "missing capability" {
							fmt.Fprint(w, `{}`)
						} else {
							fmt.Fprint(w, `{"requester_subscriptions":1}`)
						}
						return
					}
					if r.Method == "GET" && r.URL.Path == taskTargetsPath("demo") {
						targets.Add(1)
						now := time.Now().UnixMilli()
						json.NewEncoder(w).Encode(storage.TargetDirectory{Targets: []storage.RegisteredTarget{{Target: target, SeenAt: now, ExpiresAt: now + 120000}}})
						return
					}
					writes.Add(1)
					if r.Method != "POST" || r.URL.Path != conversationPath("demo", conversationID)+"/tools" || r.Header.Get("X-Agent-Instance-Id") != actor.InstanceID {
						t.Errorf("wrong tool request %s %s", r.Method, r.URL.Path)
					}
					var receipt any
					if name == "task_delegate" {
						v := delegationTestReceipt()
						if scenario != "missing acknowledgement" {
							v["source_task"].(map[string]any)["requester"] = target
						}
						receipt = v
					} else {
						v := map[string]any{"id": id, "project_ident": "demo", "status": "todo", "requester": target}
						if scenario == "missing acknowledgement" {
							delete(v, "requester")
						}
						receipt = v
					}
					encoded, _ := json.Marshal(receipt)
					json.NewEncoder(w).Encode(storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: string(encoded)}}})
				}))
				defer host.Close()
				client, _ := New(host.URL, "fixture-key", host.Client())
				args := map[string]any{"title": "Review", "parent": delegationParent, "notify_results": true}
				if name == "task_delegate" {
					args["target_project"] = "other"
				}
				if name == "task_set_requester" {
					args = map[string]any{"id": id}
				}
				raw, _ := json.Marshal(args)
				_, err := client.CallTaskTool(context.Background(), "demo", conversationID, actor, name, raw)
				if scenario == "accepted" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil {
					t.Fatal("unconfirmed requester accepted")
				}
				if scenario == "missing acknowledgement" && !strings.Contains(err.Error(), "inspect") {
					t.Fatalf("missing uncertainty guidance %v", err)
				}
				wantWrites, wantTargets := int32(1), int32(1)
				if scenario == "missing capability" {
					wantWrites = 0
					wantTargets = 0
				}
				if caps.Load() != 1 || writes.Load() != wantWrites || targets.Load() != wantTargets {
					t.Fatalf("unexpected effects/retries: caps=%d targets=%d writes=%d", caps.Load(), targets.Load(), writes.Load())
				}
			})
		}
	}
}

func TestRequesterToolClearRequiresExplicitNullWithoutDiscovery(t *testing.T) {
	actor := delegationTestActor()
	id := uuid.NewString()
	conversationID := uuid.NewString()
	for _, acknowledged := range []bool{true, false} {
		t.Run(fmt.Sprint(acknowledged), func(t *testing.T) {
			var writes atomic.Int32
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/v1/tasks/capabilities" {
					fmt.Fprint(w, `{"requester_subscriptions":1}`)
					return
				}
				if r.Method != "POST" {
					t.Errorf("clear unexpectedly discovers requester: %s", r.URL.Path)
					w.WriteHeader(500)
					return
				}
				writes.Add(1)
				receipt := map[string]any{"id": id, "project_ident": "demo", "status": "done"}
				if acknowledged {
					receipt["requester"] = nil
				}
				raw, _ := json.Marshal(receipt)
				json.NewEncoder(w).Encode(storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: string(raw)}}})
			}))
			defer host.Close()
			client, _ := New(host.URL, "fixture-key", host.Client())
			args, _ := json.Marshal(map[string]any{"id": id, "clear": true})
			_, err := client.CallTaskTool(context.Background(), "demo", conversationID, actor, "task_set_requester", args)
			if (err == nil) != acknowledged || writes.Load() != 1 {
				t.Fatalf("clear ack=%t err=%v writes=%d", acknowledged, err, writes.Load())
			}
		})
	}
}
