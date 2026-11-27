package amuxruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"
)

const requesterTaskID = "11111111-1111-4111-8111-111111111111"
const requesterSessionID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const requesterInstanceID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
const requesterGeneration = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"

func requesterTargetDirectoryJSON() []byte {
	seenAt := time.Now().UnixMilli() - 1000
	return []byte(`{"canonical_remote":null,"targets":[{"target":{"origin":{"session_id":"` + requesterSessionID + `","instance_id":"` + requesterInstanceID + `","provider":"codex","os":"linux"},"generation":"` + requesterGeneration + `"},"seen_at":` + stringInt64(seenAt) + `,"expires_at":` + stringInt64(seenAt+120000) + `}]}`)
}

func stringInt64(value int64) string { return strconv.FormatInt(value, 10) }

func TestTasksWatchClearSendsExplicitNull(t *testing.T) {
	var paths []string
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"requester_subscriptions":1}`))
			return
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || string(body["requester"]) != "null" {
			t.Errorf("clear request body=%s err=%v", body["requester"], err)
		}
		_, _ = w.Write([]byte(`{"id":"` + requesterTaskID + `","project_ident":"demo","status":"todo","requester":null}`))
	}))
	defer host.Close()
	args := []string{"--server", host.URL, "--project", "demo", "--session", requesterSessionID, "--instance", requesterInstanceID, "--provider", "codex", "--os", "linux", "watch", requesterTaskID, "--clear"}
	var stdout, stderr bytes.Buffer
	if err := runTasks(context.Background(), args, &stdout, &stderr, func(string) string { return "requester-key" }); err != nil {
		t.Fatalf("watch --clear: %v; stderr=%s", err, stderr.String())
	}
	if len(paths) != 2 || paths[0] != "GET /v1/tasks/capabilities" || paths[1] != "PUT /v1/projects/demo/tasks/"+requesterTaskID+"/requester" {
		t.Fatalf("request order=%v", paths)
	}
}

func TestTasksCreateNotifyResultsResolvesCurrentExecution(t *testing.T) {
	var paths []string
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/projects/demo/task-targets":
			_, _ = w.Write(requesterTargetDirectoryJSON())
		case "/v1/tasks/capabilities":
			_, _ = w.Write([]byte(`{"requester_subscriptions":1}`))
		case "/v1/projects/demo/tasks":
			var body struct {
				Requester map[string]any `json:"requester"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Requester["generation"] != requesterGeneration || body.Requester["origin"].(map[string]any)["session_id"] != requesterSessionID {
				t.Errorf("create requester=%+v err=%v", body.Requester, err)
			}
			_, _ = w.Write([]byte(`{"id":"` + requesterTaskID + `","project_ident":"demo","status":"todo","requester":{"origin":{"session_id":"` + requesterSessionID + `","instance_id":"` + requesterInstanceID + `","provider":"codex","os":"linux"},"generation":"` + requesterGeneration + `"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer host.Close()
	args := []string{"--server", host.URL, "--project", "demo", "--session", requesterSessionID, "--instance", requesterInstanceID, "--provider", "codex", "--os", "linux", "create", "Subscribed task", "--root-reason", "user-directed", "--specification", "scope", "--notify-results"}
	var stdout, stderr bytes.Buffer
	if err := runTasks(context.Background(), args, &stdout, &stderr, func(string) string { return "requester-key" }); err != nil {
		t.Fatalf("create --notify-results: %v; stderr=%s", err, stderr.String())
	}
	want := []string{"GET /v1/projects/demo/task-targets", "GET /v1/tasks/capabilities", "POST /v1/projects/demo/tasks"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("request order=%v want=%v", paths, want)
	}
}

func TestTasksDelegateNotifyResultsUsesCurrentExecution(t *testing.T) {
	const parent = "44444444-4444-4444-8444-444444444444"
	var paths []string
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/projects/demo/task-targets":
			_, _ = w.Write(requesterTargetDirectoryJSON())
		case "/v1/tasks/capabilities":
			_, _ = w.Write([]byte(`{"task_hierarchy":1,"requester_subscriptions":1}`))
		case "/v1/projects/demo/tasks/delegate":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["requester"].(map[string]any)["generation"] != requesterGeneration {
				t.Errorf("delegation requester=%+v err=%v", body["requester"], err)
			}
			_, _ = w.Write([]byte(`{"delegation":{"id":"33333333-3333-4333-8333-333333333333","source_project_ident":"demo","source_task_id":"11111111-1111-4111-8111-111111111111","target_project_ident":"other","target_task_id":"22222222-2222-4222-8222-222222222222"},"source_task":{"id":"11111111-1111-4111-8111-111111111111","project_ident":"demo","status":"todo","kind":"delegated","delegated_to_project_ident":"other","delegated_to_task_id":"22222222-2222-4222-8222-222222222222","requester":{"origin":{"session_id":"` + requesterSessionID + `","instance_id":"` + requesterInstanceID + `","provider":"codex","os":"linux"},"generation":"` + requesterGeneration + `"},"parent":{"id":"` + parent + `","project_ident":"demo"}},"target_task":{"id":"22222222-2222-4222-8222-222222222222","project_ident":"other","status":"todo","parent":{"id":"11111111-1111-4111-8111-111111111111","project_ident":"demo"}},"message_id":42}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer host.Close()
	args := []string{"--server", host.URL, "--project", "demo", "--session", requesterSessionID, "--instance", requesterInstanceID, "--provider", "codex", "--os", "linux", "delegate", "Handoff", "--parent", parent, "--target-project", "other", "--notify-results"}
	var stdout, stderr bytes.Buffer
	if err := runTasks(context.Background(), args, &stdout, &stderr, func(string) string { return "requester-key" }); err != nil {
		t.Fatalf("delegate --notify-results: %v; stderr=%s", err, stderr.String())
	}
	want := []string{"GET /v1/tasks/capabilities", "GET /v1/projects/demo/task-targets", "GET /v1/tasks/capabilities", "POST /v1/projects/demo/tasks/delegate"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("request order=%v want=%v", paths, want)
	}
}
