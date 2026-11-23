package gatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

const workflowAgentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const workflowInstanceID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

type workflowRequest struct {
	method       string
	path         string
	agentHeaders bool
	body         string
}

func newWorkflowFixture(t *testing.T) (*storage.Store, *Client, *httptest.Server, *[]workflowRequest) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	requests := new([]workflowRequest)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*requests = append(*requests, workflowRequest{
			method:       r.Method,
			path:         r.URL.Path,
			agentHeaders: r.Header.Get("X-Agent-Session-Id") != "" || r.Header.Get("X-Agent-Instance-Id") != "" || r.Header.Get("X-Agent-Provider") != "" || r.Header.Get("X-Agent-OS") != "",
			body:         string(body),
		})
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(host.Close)
	client, err := New(host.URL, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	return store, client, host, requests
}

func workflowOrigin() *storage.AgentOrigin {
	return &storage.AgentOrigin{SessionID: workflowAgentID, InstanceID: workflowInstanceID, Provider: "codex", OS: "linux"}
}

func TestDesktopCloseGeneralAndSoftwareWithoutAgentConfirmation(t *testing.T) {
	ctx := context.Background()
	store, client, _, requests := newWorkflowFixture(t)
	general, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "General close", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := client.CloseTask(ctx, "demo", general.ID)
	if err != nil || closed.Status != "done" || closed.DoneAt == nil || len(closed.Changeset) != 0 {
		t.Fatalf("general close acknowledgement: %+v %v", closed, err)
	}
	assertHumanCloseRequest(t, *requests, general.ID)
	storedGeneral, err := store.Task(ctx, "demo", general.ID)
	if err != nil || storedGeneral.Task.TaskType != "general" {
		t.Fatalf("general task type changed: %+v %v", storedGeneral, err)
	}

	actor := workflowOrigin()
	software, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Software close", Reporter: "user"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.TaskUpdate(ctx, "demo", software.ID, storage.TaskWorkflowUpdate{Status: "in_progress"}, actor, nil); err != nil {
		t.Fatal(err)
	}
	_, err = client.TaskUpdate(ctx, "demo", software.ID, storage.TaskWorkflowUpdate{Status: "done"}, actor, nil)
	var guided *HTTPError
	if !errors.As(err, &guided) || guided.Code != "task_confirmation_required" {
		t.Fatalf("agent completion did not require guided confirmation: %v", err)
	}
	current, err := store.Task(ctx, "demo", software.ID)
	if err != nil || current.Task.Status != "in_progress" || current.Task.DoneAt != nil {
		t.Fatalf("guided confirmation moved task: %+v %v", current, err)
	}
	kind := "software"
	confirmation, err := client.TaskConfirm(ctx, "demo", software.ID, &kind, nil, actor)
	if err != nil || !strings.Contains(string(confirmation), "task_set_changeset") {
		t.Fatalf("software confirmation did not request explicit changeset: %s %v", confirmation, err)
	}
	current, err = store.Task(ctx, "demo", software.ID)
	if err != nil || current.Task.Status != "in_progress" || current.Task.TaskType != "software" || current.Task.DoneAt != nil || len(current.Task.Changeset) != 0 {
		t.Fatalf("mutation confirmation moved task or invented changeset: %+v %v", current, err)
	}
	closed, err = client.CloseTask(ctx, "demo", software.ID)
	if err != nil || closed.Status != "done" || closed.DoneAt == nil || len(closed.Changeset) != 0 {
		t.Fatalf("software human close: %+v %v", closed, err)
	}
	assertHumanCloseRequest(t, *requests, software.ID)
	storedSoftware, err := store.Task(ctx, "demo", software.ID)
	if err != nil || storedSoftware.Task.TaskType != "software" || len(storedSoftware.Task.Changeset) != 0 || storedSoftware.Task.OwnerOrigin == nil || *storedSoftware.Task.OwnerOrigin != *actor || storedSoftware.Task.OwnerAgentID == nil || *storedSoftware.Task.OwnerAgentID != actor.SessionID {
		t.Fatalf("software task changed during human close: %+v %v", storedSoftware, err)
	}

}

func assertHumanCloseRequest(t *testing.T, requests []workflowRequest, taskID string) {
	t.Helper()
	if len(requests) == 0 {
		t.Fatal("close request was not recorded")
	}
	request := requests[len(requests)-1]
	if request.method != http.MethodPatch || !strings.HasSuffix(request.path, "/"+taskID) || request.agentHeaders {
		t.Fatalf("human close request carried wrong route or agent headers: %+v", request)
	}
	var body map[string]json.RawMessage
	if json.Unmarshal([]byte(request.body), &body) != nil || string(body["status"]) != `"done"` {
		t.Fatalf("human close sent unexpected request: %s", request.body)
	}
	if _, exists := body["changeset"]; exists {
		t.Fatalf("human close sent changeset: %s", request.body)
	}
}

func TestDesktopParkAndResumeUseHumanWorkflow(t *testing.T) {
	ctx := context.Background()
	store, client, _, requests := newWorkflowFixture(t)
	actor := workflowOrigin()
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Park and resume", Reporter: "user"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.TaskUpdate(ctx, "demo", task.ID, storage.TaskWorkflowUpdate{Status: "in_progress"}, actor, nil); err != nil {
		t.Fatal(err)
	}
	parked, err := client.ParkTask(ctx, "demo", task.ID, "  Choose the release date  ")
	if err != nil || parked.Status != "user_review" || parked.ReviewAction == nil || *parked.ReviewAction != "Choose the release date" || parked.OwnerAgentID != nil || parked.OwnerOrigin != nil || parked.StartedAt != nil {
		t.Fatalf("park acknowledgement: %+v %v", parked, err)
	}
	resumed, err := client.ResumeTask(ctx, "demo", task.ID)
	if err != nil || resumed.Status != "todo" || resumed.ReviewAction != nil || resumed.OwnerAgentID != nil || resumed.OwnerOrigin != nil || resumed.StartedAt != nil || resumed.DoneAt != nil {
		t.Fatalf("resume acknowledgement: %+v %v", resumed, err)
	}
	todoTask, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Park from todo", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	todoParked, err := client.ParkTask(ctx, "demo", todoTask.ID, "Choose a date")
	if err != nil || todoParked.Status != "user_review" || todoParked.ReviewAction == nil || *todoParked.ReviewAction != "Choose a date" {
		t.Fatalf("todo park acknowledgement: %+v %v", todoParked, err)
	}
	for _, request := range *requests {
		if !strings.HasSuffix(request.path, "/"+task.ID) || (request.method != http.MethodPatch && request.method != http.MethodGet) {
			continue
		}
		if request.method == http.MethodPatch && strings.Contains(request.body, `"status":"in_progress"`) {
			continue
		}
		if request.agentHeaders {
			t.Errorf("human park/resume sent agent headers: %+v", request)
		}
	}
	var parkBody map[string]json.RawMessage
	var resumeBody map[string]json.RawMessage
	for _, request := range *requests {
		if request.method != http.MethodPatch {
			continue
		}
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(request.body), &body)
		if string(body["status"]) == `"user_review"` && strings.HasSuffix(request.path, "/"+task.ID) {
			parkBody = body
		}
		if string(body["status"]) == `"todo"` && len(body["expected_status"]) > 0 {
			resumeBody = body
		}
	}
	if string(parkBody["review_action"]) != `"Choose the release date"` || string(parkBody["expected_status"]) != `"in_progress"` || len(parkBody["changeset"]) != 0 {
		t.Fatalf("park request was not canonical: %s", parkBody)
	}
	if string(resumeBody["status"]) != `"todo"` || string(resumeBody["expected_status"]) != `"user_review"` || len(resumeBody["changeset"]) != 0 {
		t.Fatalf("resume request was not atomically conditional: %s", resumeBody)
	}
	if _, err := client.ParkTask(ctx, "demo", task.ID, " \n "); err == nil {
		t.Fatal("accepted blank review action")
	}
	if _, err := client.ParkTask(ctx, "demo", task.ID, strings.Repeat("x", 4097)); err == nil {
		t.Fatal("accepted overlong review action")
	}
}

func TestDesktopWorkflowRejectsIgnoredAcknowledgements(t *testing.T) {
	const id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	for _, tc := range []struct {
		name  string
		call  func(*Client) error
		serve func(http.ResponseWriter, *http.Request)
	}{
		{"close missing done timestamp", func(c *Client) error { _, err := c.CloseTask(context.Background(), "demo", id); return err }, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"id":"` + id + `","project_ident":"demo","status":"done"}`))
		}},
		{"park owner not cleared", func(c *Client) error {
			_, err := c.ParkTask(context.Background(), "demo", id, "Choose date")
			return err
		}, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"id":"` + id + `","project_ident":"demo","status":"in_progress"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"` + id + `","project_ident":"demo","status":"user_review","review_action":"Choose date","owner_agent_id":"agent","started_at":123}`))
		}},
		{"resume review not cleared", func(c *Client) error { _, err := c.ResumeTask(context.Background(), "demo", id); return err }, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"id":"` + id + `","project_ident":"demo","status":"user_review"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"` + id + `","project_ident":"demo","status":"todo","review_action":"Choose date"}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(tc.serve))
			defer host.Close()
			client, _ := New(host.URL, "secret", nil)
			if err := tc.call(client); err == nil {
				t.Fatal("accepted ignored workflow acknowledgement")
			}
		})
	}
}

func TestParkTaskRejectsInactiveReadStateWithoutWriting(t *testing.T) {
	const id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	for _, status := range []string{"done", "user_review", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			requests := 0
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				body := `{"id":"` + id + `","project_ident":"demo","status":"` + status + `"}`
				if r.Method == http.MethodGet && status == "done" {
					body = `{"id":"` + id + `","project_ident":"demo","status":"done","done_at":123}`
				}
				_, _ = w.Write([]byte(body))
			}))
			defer host.Close()
			client, _ := New(host.URL, "secret", nil)
			if _, err := client.ParkTask(context.Background(), "demo", id, "Choose date"); err == nil {
				t.Fatal("parked task outside the active states")
			}
			if requests != 1 {
				t.Fatalf("request count=%d, want read only", requests)
			}
		})
	}
}

type workflowRoundTripper func(*http.Request) (*http.Response, error)

func (f workflowRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDesktopWorkflowDoesNotRetry503OrTransportErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Client) error
	}{
		{"close", func(c *Client) error {
			_, err := c.CloseTask(context.Background(), "demo", workflowAgentID)
			return err
		}},
		{"park", func(c *Client) error {
			_, err := c.ParkTask(context.Background(), "demo", workflowAgentID, "Choose date")
			return err
		}},
		{"resume", func(c *Client) error {
			_, err := c.ResumeTask(context.Background(), "demo", workflowAgentID)
			return err
		}},
	} {
		t.Run(tc.name+" 503", func(t *testing.T) {
			requests := 0
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method == http.MethodGet {
					status := "user_review"
					if tc.name == "park" {
						status = "in_progress"
					}
					_, _ = w.Write([]byte(`{"id":"` + workflowAgentID + `","project_ident":"demo","status":"` + status + `"}`))
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer host.Close()
			client, _ := New(host.URL, "secret", nil)
			err := tc.call(client)
			var response *HTTPError
			if !errors.As(err, &response) || response.Status != http.StatusServiceUnavailable {
				t.Fatalf("expected HTTP 503, got %v", err)
			}
			want := 1
			if tc.name == "resume" || tc.name == "park" {
				want = 2 // one pre-read and one update
			}
			if requests != want {
				t.Fatalf("request count=%d, want %d", requests, want)
			}
		})
		t.Run(tc.name+" transport", func(t *testing.T) {
			requests := 0
			transport := workflowRoundTripper(func(*http.Request) (*http.Response, error) {
				requests++
				return nil, errors.New("offline")
			})
			httpClient := &http.Client{Transport: transport}
			client, err := New("http://localhost", "secret", httpClient)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.call(client); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("expected unavailable transport error, got %v", err)
			}
			if requests != 1 {
				t.Fatalf("request count=%d, want 1", requests)
			}
		})
	}
}
