package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTasksDelegateChecksHierarchyThenPostsLiteralHandoff(t *testing.T) {
	const parent = "44444444-4444-4444-8444-444444444444"
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			if r.Method != http.MethodGet || r.URL.Path != "/v1/tasks/capabilities" {
				t.Errorf("first request = %s %s", r.Method, r.URL.Path)
			}
			_, _ = w.Write([]byte(`{"task_hierarchy":1}`))
		case 2:
			if r.Method != http.MethodPost || r.URL.Path != "/v1/projects/demo/tasks/delegate" {
				t.Errorf("second request = %s %s", r.Method, r.URL.Path)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			} else if body["target_project_ident"] != "other" || body["title"] != "Move assets" || body["parent_task_id"] != parent || body["specification"] != "keep $(literal)\ntext" {
				t.Errorf("delegation payload changed: %#v", body)
			}
			_, _ = w.Write([]byte(`{"delegation":{"id":"33333333-3333-4333-8333-333333333333","source_project_ident":"demo","source_task_id":"11111111-1111-4111-8111-111111111111","target_project_ident":"other","target_task_id":"22222222-2222-4222-8222-222222222222"},"source_task":{"id":"11111111-1111-4111-8111-111111111111","project_ident":"demo","status":"todo","kind":"delegated","delegated_to_project_ident":"other","delegated_to_task_id":"22222222-2222-4222-8222-222222222222","parent":{"id":"` + parent + `","project_ident":"demo"}},"target_task":{"id":"22222222-2222-4222-8222-222222222222","project_ident":"other","status":"todo","parent":{"id":"11111111-1111-4111-8111-111111111111","project_ident":"demo"}},"message_id":42}`))
		default:
			t.Errorf("unexpected extra request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer host.Close()
	args := []string{"--server", host.URL, "--project", "demo", "--session", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "--instance", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "--provider", "codex", "--os", "linux", "delegate", "Move assets", "--parent", parent, "--target-project", "other", "--specification", "keep $(literal)\ntext"}
	var stdout, stderr bytes.Buffer
	err := runTasks(context.Background(), args, &stdout, &stderr, func(string) string { return "task-key" })
	if err != nil || requests.Load() != 2 {
		t.Fatalf("runTasks err=%v requests=%d stderr=%s", err, requests.Load(), stderr.String())
	}
	var receipt map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil || receipt["message_id"] != float64(42) {
		t.Fatalf("receipt=%s err=%v", stdout.String(), err)
	}
}

func TestTasksDelegateRequiresNativeOriginAndHierarchyBeforePost(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer host.Close()
	base := []string{"--server", host.URL, "--project", "demo", "delegate", "Handoff", "--parent", "44444444-4444-4444-8444-444444444444", "--target-project", "other"}
	var stdout, stderr bytes.Buffer
	err := runTasks(context.Background(), append([]string{"--human"}, base...), &stdout, &stderr, func(string) string { return "task-key" })
	if err == nil || !strings.Contains(err.Error(), "requires a complete native agent origin") || requests.Load() != 0 {
		t.Fatalf("human delegate err=%v requests=%d", err, requests.Load())
	}
	args := append([]string{"--session", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "--instance", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "--provider", "codex", "--os", "linux"}, base...)
	err = runTasks(context.Background(), args, &stdout, &stderr, func(string) string { return "task-key" })
	if err == nil || !strings.Contains(err.Error(), "task_hierarchy") || requests.Load() != 1 {
		t.Fatalf("missing capability err=%v requests=%d", err, requests.Load())
	}
}
