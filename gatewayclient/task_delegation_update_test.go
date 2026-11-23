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

func TestDelegationUpdatePinsLinksAndConfirmsMetadataForEveryTaskStatus(t *testing.T) {
	for _, scenario := range []string{"accepted", "source changed", "target changed", "link changed", "parent changed", "source title dropped", "target title dropped", "specification dropped", "labels omitted", "unknown status", "notice missing"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := delegationTestReceipt()
			source := fixture["source_task"].(map[string]any)
			target := fixture["target_task"].(map[string]any)
			source["status"], target["status"] = "user_review", "cancelled"
			id := source["id"].(string)
			title, description, legacy, specification := "  New (DELEGATED) (DELEGATED)", "line one\n$(literal)", "legacy", "preferred"
			var reads, writes atomic.Int32
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					reads.Add(1)
					if r.URL.Path != taskPath("demo")+"/"+id || r.URL.Query().Get("comments") != "0" {
						t.Errorf("pre-read wrong resource %s", r.URL.String())
					}
					json.NewEncoder(w).Encode(source)
					return
				}
				writes.Add(1)
				if r.Method != "PATCH" || r.URL.Path != taskPath("demo")+"/"+id+"/delegation" {
					t.Errorf("wrong contract mutation %s %s", r.Method, r.URL.Path)
				}
				var body map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&body) != nil || string(body["labels"]) != "[]" {
					t.Errorf("explicit empty labels lost on wire: %s", body["labels"])
				}
				var gotTitle, gotDescription string
				if json.Unmarshal(body["title"], &gotTitle) != nil || gotTitle != title || json.Unmarshal(body["description"], &gotDescription) != nil || gotDescription != description {
					t.Error("literal metadata changed")
				}
				source["title"], target["title"] = title, "  New"
				for _, task := range []map[string]any{source, target} {
					task["description"] = description
					task["details"] = specification
					task["labels"] = []string{}
				}
				switch scenario {
				case "source changed":
					source["id"] = uuid.NewString()
				case "target changed":
					target["id"] = uuid.NewString()
				case "link changed":
					fixture["delegation"].(map[string]any)["target_project_ident"] = "wrong"
				case "parent changed":
					source["parent"] = map[string]any{"id": uuid.NewString(), "project_ident": "demo"}
				case "source title dropped":
					source["title"] = "old"
				case "target title dropped":
					target["title"] = "old"
				case "specification dropped":
					target["details"] = legacy
				case "labels omitted":
					delete(source, "labels")
				case "unknown status":
					target["status"] = "unknown"
				case "notice missing":
					fixture["message_id"] = 0
				}
				json.NewEncoder(w).Encode(fixture)
			}))
			defer host.Close()
			client, _ := New(host.URL, "fixture-key", host.Client())
			result, err := client.UpdateTaskDelegation(context.Background(), "demo", id, storage.TaskDelegationUpdate{Title: &title, Description: &description, Details: &legacy, Specification: &specification, Labels: []string{}}, nil)
			if scenario == "accepted" {
				if err != nil || result == nil {
					t.Fatalf("valid edit rejected: %v", err)
				}
			} else if err == nil || result != nil || !strings.Contains(err.Error(), "inspect") {
				t.Fatalf("unconfirmed edit accepted: %+v %v", result, err)
			}
			if reads.Load() != 1 || writes.Load() != 1 {
				t.Fatalf("edit retried: reads=%d writes=%d", reads.Load(), writes.Load())
			}
		})
	}
}

func TestDelegationUpdateRejectsUnlinkedSourceBeforeWriting(t *testing.T) {
	for _, scenario := range []string{"normal task", "missing target", "invalid target", "same project"} {
		t.Run(scenario, func(t *testing.T) {
			source := delegationTestReceipt()["source_task"].(map[string]any)
			id := source["id"].(string)
			switch scenario {
			case "normal task":
				source["kind"] = "normal"
			case "missing target":
				delete(source, "delegated_to_task_id")
			case "invalid target":
				source["delegated_to_task_id"] = "bad"
			case "same project":
				source["delegated_to_project_ident"] = "demo"
			}
			var writes atomic.Int32
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes.Add(1)
					w.WriteHeader(500)
					return
				}
				json.NewEncoder(w).Encode(source)
			}))
			defer host.Close()
			client, _ := New(host.URL, "fixture-key", host.Client())
			if _, err := client.UpdateTaskDelegation(context.Background(), "demo", id, storage.TaskDelegationUpdate{}, nil); err == nil {
				t.Fatal("unlinked source accepted")
			}
			if writes.Load() != 0 {
				t.Fatal("unlinked source mutated")
			}
		})
	}
}

func TestDelegationUpdateToolConfirmsExistingLinksAndRequestedMetadata(t *testing.T) {
	fixture := delegationTestReceipt()
	source := fixture["source_task"].(map[string]any)
	target := fixture["target_task"].(map[string]any)
	id := source["id"].(string)
	conversationID := uuid.NewString()
	actor := delegationTestActor()
	var writes atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(source)
			return
		}
		writes.Add(1)
		if r.URL.Path != conversationPath("demo", conversationID)+"/tools" || r.Header.Get("X-Agent-Session-Id") != actor.SessionID {
			t.Error("tool scope changed")
		}
		source["title"], target["title"] = "New (DELEGATED)", "New"
		raw, _ := json.Marshal(fixture)
		fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}]}`, string(raw))
	}))
	defer host.Close()
	client, _ := New(host.URL, "fixture-key", host.Client())
	args, _ := json.Marshal(map[string]any{"id": id, "title": "New"})
	if _, err := client.CallTaskTool(context.Background(), "demo", conversationID, actor, "task_update_delegation", args); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 1 {
		t.Fatal("tool mutation retried")
	}
}
