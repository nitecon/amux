package gatewayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/storage"
)

func TestDelegationToolPinsConversationParentAndValidatesReceiptWithoutRetry(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "missing receipt link"}[malformed], func(t *testing.T) {
			actor := delegationTestActor()
			conversationID := uuid.NewString()
			var reads, writes atomic.Int32
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					reads.Add(1)
					if r.URL.Path != conversationPath("demo", conversationID) {
						t.Errorf("unexpected conversation read %s", r.URL.Path)
					}
					json.NewEncoder(w).Encode(storage.Conversation{ID: conversationID, ProjectIdent: "demo", TaskID: delegationParent, Owner: actor, Provider: actor.Provider})
					return
				}
				writes.Add(1)
				if r.Method != "POST" || r.URL.Path != conversationPath("demo", conversationID)+"/tools" {
					t.Errorf("unexpected mutation %s %s", r.Method, r.URL.Path)
				}
				var body struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Name != "task_delegate" || body.Arguments["parent"] != delegationParent {
					t.Errorf("default parent was not pinned: %+v", body)
				}
				if r.Header.Get("X-Agent-Instance-Id") != actor.InstanceID {
					t.Error("mutation lost native origin")
				}
				receipt := delegationTestReceipt()
				if malformed {
					delete(receipt, "delegation")
				}
				encoded, _ := json.Marshal(receipt)
				json.NewEncoder(w).Encode(storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: string(encoded)}}})
			}))
			defer host.Close()
			client, _ := New(host.URL, "fixture-key", host.Client())
			_, err := client.CallTaskToolWithGoal(context.Background(), "demo", conversationID, actor, nil, "task_delegate", json.RawMessage(`{"title":"Review","target_project":"other"}`))
			if malformed {
				if err == nil || !strings.Contains(err.Error(), "inspect") {
					t.Fatalf("malformed receipt error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if reads.Load() != 1 || writes.Load() != 1 {
				t.Fatalf("unexpected retries: reads=%d writes=%d", reads.Load(), writes.Load())
			}
		})
	}
}

func TestDelegationToolRefusesForeignConversationBeforeMutation(t *testing.T) {
	actor := delegationTestActor()
	conversationID := uuid.NewString()
	for _, field := range []string{"id", "project", "owner"} {
		t.Run(field, func(t *testing.T) {
			var writes atomic.Int32
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes.Add(1)
					w.WriteHeader(500)
					return
				}
				c := storage.Conversation{ID: conversationID, ProjectIdent: "demo", TaskID: delegationParent, Owner: actor}
				switch field {
				case "id":
					c.ID = uuid.NewString()
				case "project":
					c.ProjectIdent = "other"
				case "owner":
					c.Owner = delegationTestActor()
				}
				json.NewEncoder(w).Encode(c)
			}))
			defer host.Close()
			client, _ := New(host.URL, "fixture-key", host.Client())
			if _, err := client.CallTaskTool(context.Background(), "demo", conversationID, actor, "task_delegate", json.RawMessage(`{"title":"Review","target_project":"other"}`)); err == nil {
				t.Fatal("foreign conversation accepted")
			}
			if writes.Load() != 0 {
				t.Fatal("foreign conversation caused mutation")
			}
		})
	}
}
