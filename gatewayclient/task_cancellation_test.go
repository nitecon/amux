package gatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nitecon/amux/storage"
)

const (
	cancelTestProject  = "demo"
	cancelTestTaskID   = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	cancelTestSession  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	cancelTestInstance = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

func cancelTestOrigin() *storage.AgentOrigin {
	return &storage.AgentOrigin{SessionID: cancelTestSession, InstanceID: cancelTestInstance, Provider: "claude", OS: "linux"}
}

func cancellationFixture(reason string, origin *storage.AgentOrigin) storage.Cancellation {
	return storage.Cancellation{
		TaskID: cancelTestTaskID, ProjectIdent: cancelTestProject, Reason: reason,
		PriorStatus: "in_progress", PriorOwnerAgentID: ptrString("agent-1"),
		PriorOwnerOrigin: origin, PriorStartedAt: ptrInt64(100), ReporterOrigin: *origin,
		CancelledAt: 200, ExecutorStopConfirmed: false,
	}
}

func cancellationResponseFixture(reason string, origin *storage.AgentOrigin) storage.CancelResponse {
	response := storage.CancelResponse{
		Task:         &storage.Task{ID: cancelTestTaskID, ProjectIdent: cancelTestProject, Status: "cancelled", OwnerAgentID: ptrString("agent-1"), OwnerOrigin: origin, StartedAt: ptrInt64(100)},
		Cancellation: &storage.Cancellation{},
	}
	cancellation := cancellationFixture(reason, origin)
	response.Cancellation = &cancellation
	return response
}

func TestTaskCancellationGETUsesScopedPathAndValidatesResult(t *testing.T) {
	want := cancellationFixture("superseded", cancelTestOrigin())
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/projects/demo/tasks/"+cancelTestTaskID+"/cancellation" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer host.Close()
	client, _ := New(host.URL, "secret", nil)
	got, err := client.TaskCancellation(context.Background(), cancelTestProject, cancelTestTaskID)
	if err != nil || got == nil || got.TaskID != want.TaskID || got.ExecutorStopConfirmed {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestTaskCancelPOSTPreservesChangesetPresenceAndValidatesAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name      string
		changeset json.RawMessage
		contains  string
	}{
		{name: "absent", contains: `"reason":"superseded"`},
		{name: "explicit null", changeset: json.RawMessage(`null`), contains: `"changeset":null`},
		{name: "files", changeset: json.RawMessage(` ["src/main.go"] `), contains: `"changeset":["src/main.go"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/projects/demo/tasks/"+cancelTestTaskID+"/cancellation" {
					t.Errorf("request %s %s", r.Method, r.URL)
				}
				for key, value := range map[string]string{"X-Agent-Session-Id": cancelTestSession, "X-Agent-Instance-Id": cancelTestInstance, "X-Agent-Provider": "claude", "X-Agent-OS": "linux"} {
					if r.Header.Get(key) != value {
						t.Errorf("%s=%q", key, r.Header.Get(key))
					}
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode body: %v", err)
				}
				encoded, _ := json.Marshal(body)
				if !strings.Contains(string(encoded), tc.contains) {
					t.Errorf("body %s missing %s", encoded, tc.contains)
				}
				if tc.name == "absent" && body["changeset"] != nil {
					t.Errorf("absent changeset emitted: %s", encoded)
				}
				_ = json.NewEncoder(w).Encode(cancellationResponseFixture("superseded", cancelTestOrigin()))
			}))
			defer host.Close()
			client, _ := New(host.URL, "secret", nil)
			got, err := client.TaskCancel(context.Background(), cancelTestProject, cancelTestTaskID, "  superseded \n ", tc.changeset, cancelTestOrigin())
			want := cancellationResponseFixture("superseded", cancelTestOrigin())
			if err != nil || got == nil || !reflect.DeepEqual(got.Cancellation, want.Cancellation) ||
				got.Task.ID != want.Task.ID || got.Task.ProjectIdent != want.Task.ProjectIdent || got.Task.Status != want.Task.Status ||
				!sameString(got.Task.OwnerAgentID, want.Task.OwnerAgentID) || !sameOrigin(got.Task.OwnerOrigin, want.Task.OwnerOrigin) ||
				!sameInt64(got.Task.StartedAt, want.Task.StartedAt) || got.Task.DoneAt != nil {
				t.Fatalf("result=%+v err=%v", got, err)
			}
		})
	}
}

func TestTaskCancelRejectsFalseTypedAcknowledgementsWithoutRetry(t *testing.T) {
	foreignSession := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	for _, tc := range []struct {
		name   string
		change func(*storage.CancelResponse)
	}{
		{"task status", func(value *storage.CancelResponse) { value.Task.Status = "done" }},
		{"task ID", func(value *storage.CancelResponse) { value.Task.ID = foreignSession }},
		{"task project", func(value *storage.CancelResponse) { value.Task.ProjectIdent = "foreign" }},
		{"task owner origin", func(value *storage.CancelResponse) {
			value.Task.OwnerOrigin.SessionID = foreignSession
		}},
		{"task started time", func(value *storage.CancelResponse) { value.Task.StartedAt = ptrInt64(43) }},
		{"task done time", func(value *storage.CancelResponse) { value.Task.DoneAt = ptrInt64(123) }},
		{"cancellation task ID", func(value *storage.CancelResponse) { value.Cancellation.TaskID = foreignSession }},
		{"cancellation project", func(value *storage.CancelResponse) { value.Cancellation.ProjectIdent = "foreign" }},
		{"reporter session", func(value *storage.CancelResponse) { value.Cancellation.ReporterOrigin.SessionID = foreignSession }},
		{"reporter provider", func(value *storage.CancelResponse) { value.Cancellation.ReporterOrigin.Provider = "codex" }},
		{"executor stop claimed", func(value *storage.CancelResponse) { value.Cancellation.ExecutorStopConfirmed = true }},
		{"cancelled time", func(value *storage.CancelResponse) { value.Cancellation.CancelledAt = 0 }},
		{"prior status", func(value *storage.CancelResponse) { value.Cancellation.PriorStatus = "done" }},
		{"reason", func(value *storage.CancelResponse) { value.Cancellation.Reason = "different reason" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := cancellationResponseFixture("superseded", cancelTestOrigin())
			tc.change(&response)
			var requests atomic.Int32
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v1/projects/demo/tasks/"+cancelTestTaskID+"/cancellation" {
					t.Errorf("request %s %s", r.Method, r.URL)
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 || string(body["reason"]) != `"superseded"` {
					t.Errorf("request body=%v err=%v", body, err)
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer host.Close()
			client, _ := New(host.URL, "secret", nil)
			if _, err := client.TaskCancel(context.Background(), cancelTestProject, cancelTestTaskID, "  superseded \n ", nil, cancelTestOrigin()); err == nil {
				t.Fatal("accepted false cancellation acknowledgement")
			}
			if requests.Load() != 1 {
				t.Fatalf("request count=%d, want one", requests.Load())
			}
		})
	}
}

func TestTaskCancelValidatesNativeInputAndDoesNotSendInvalidWrites(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer host.Close()
	client, _ := New(host.URL, "secret", nil)
	for _, tc := range []struct {
		reason    string
		changeset json.RawMessage
		origin    *storage.AgentOrigin
	}{
		{" ", nil, cancelTestOrigin()}, {strings.Repeat("x", 16385), nil, cancelTestOrigin()}, {"reason", json.RawMessage(`{}`), cancelTestOrigin()}, {"reason", nil, nil},
	} {
		if _, err := client.TaskCancel(context.Background(), cancelTestProject, cancelTestTaskID, tc.reason, tc.changeset, tc.origin); err == nil {
			t.Errorf("accepted invalid input %+v", tc)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid requests sent: %d", requests.Load())
	}
}

func TestTaskCancelAndCancellationRejectMalformedAcknowledgements(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		get        bool
	}{
		{"null acknowledgement", `null`, false},
		{"empty acknowledgement", `{}`, false},
		{"fake status", `{"task":{"id":"` + cancelTestTaskID + `","project_ident":"demo","status":"todo"},"cancellation":{}}`, false},
		{"wrong reporter", `{"task":{"id":"` + cancelTestTaskID + `","project_ident":"demo","status":"cancelled"},"cancellation":{"task_id":"` + cancelTestTaskID + `","project_ident":"demo","reason":"x","prior_status":"todo","reporter_origin":{"session_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","instance_id":"` + cancelTestInstance + `","provider":"claude","os":"linux"},"cancelled_at":1,"executor_stop_confirmed":false}}`, false},
		{"read foreign task ID", `{"task_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","project_ident":"demo","reason":"x","prior_status":"todo","reporter_origin":{"session_id":"` + cancelTestSession + `","instance_id":"` + cancelTestInstance + `","provider":"claude","os":"linux"},"cancelled_at":1,"executor_stop_confirmed":false}`, true},
		{"read foreign project", `{"task_id":"` + cancelTestTaskID + `","project_ident":"foreign","reason":"x","prior_status":"todo","reporter_origin":{"session_id":"` + cancelTestSession + `","instance_id":"` + cancelTestInstance + `","provider":"claude","os":"linux"},"cancelled_at":1,"executor_stop_confirmed":false}`, true},
		{"read null reporter origin", `{"task_id":"` + cancelTestTaskID + `","project_ident":"demo","reason":"x","prior_status":"todo","reporter_origin":null,"cancelled_at":1,"executor_stop_confirmed":false}`, true},
		{"stop claimed", `{"task_id":"` + cancelTestTaskID + `","project_ident":"demo","reason":"x","prior_status":"todo","reporter_origin":{"session_id":"` + cancelTestSession + `","instance_id":"` + cancelTestInstance + `","provider":"claude","os":"linux"},"cancelled_at":1,"executor_stop_confirmed":true}`, true},
		{"missing stop field", `{"task_id":"` + cancelTestTaskID + `","project_ident":"demo","reason":"x","prior_status":"todo","reporter_origin":{"session_id":"` + cancelTestSession + `","instance_id":"` + cancelTestInstance + `","provider":"claude","os":"linux"},"cancelled_at":1}`, true},
		{"null stop field", `{"task_id":"` + cancelTestTaskID + `","project_ident":"demo","reason":"x","prior_status":"todo","reporter_origin":{"session_id":"` + cancelTestSession + `","instance_id":"` + cancelTestInstance + `","provider":"claude","os":"linux"},"cancelled_at":1,"executor_stop_confirmed":null}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			defer host.Close()
			client, _ := New(host.URL, "secret", nil)
			var err error
			if tc.get {
				_, err = client.TaskCancellation(context.Background(), cancelTestProject, cancelTestTaskID)
			} else {
				_, err = client.TaskCancel(context.Background(), cancelTestProject, cancelTestTaskID, "x", nil, cancelTestOrigin())
			}
			if err == nil {
				t.Fatal("accepted fake/malformed acknowledgement")
			}
		})
	}
}

// Rust gateway.rs::validate_cancellation and cancel_task_with_changeset bind
// active prior claims and forbid acknowledging another execution's claim.
func TestCancellationRejectsUnboundOrDifferentPriorClaim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		read   bool
		change func(*storage.CancelResponse)
	}{
		{"unbound active claim", false, func(value *storage.CancelResponse) {
			value.Cancellation.PriorOwnerOrigin = nil
			value.Task.OwnerOrigin = nil
		}},
		{"different claimant", false, func(value *storage.CancelResponse) {
			other := *cancelTestOrigin()
			other.InstanceID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
			value.Cancellation.PriorOwnerOrigin = &other
			value.Task.OwnerOrigin = &other
		}},
		{"cancelled task has done timestamp", false, func(value *storage.CancelResponse) { value.Task.DoneAt = ptrInt64(200) }},
		{"read unbound reserved claim", true, func(value *storage.CancelResponse) {
			value.Cancellation.PriorStatus = "todo"
			value.Cancellation.PriorOwnerOrigin = nil
		}},
		{"read unbound active claim", true, func(value *storage.CancelResponse) {
			value.Cancellation.PriorOwnerAgentID = nil
			value.Cancellation.PriorOwnerOrigin = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := cancellationResponseFixture("x", cancelTestOrigin())
			tc.change(&value)
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.read {
					json.NewEncoder(w).Encode(value.Cancellation)
				} else {
					json.NewEncoder(w).Encode(value)
				}
			}))
			defer host.Close()
			client, _ := New(host.URL, "secret", nil)
			var err error
			if tc.read {
				_, err = client.TaskCancellation(context.Background(), cancelTestProject, cancelTestTaskID)
			} else {
				_, err = client.TaskCancel(context.Background(), cancelTestProject, cancelTestTaskID, "x", json.RawMessage(`null`), cancelTestOrigin())
			}
			if err == nil {
				t.Fatal("accepted inconsistent prior claim")
			}
		})
	}
}

func TestTaskCancelDoesNotRetryAndKeepsConflictCodeWhitelist(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusBadGateway} {
		var requests atomic.Int32
		host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":"missing_changeset"}`))
		}))
		client, _ := New(host.URL, "secret", nil)
		_, err := client.TaskCancel(context.Background(), cancelTestProject, cancelTestTaskID, "reason", nil, cancelTestOrigin())
		host.Close()
		if err == nil || requests.Load() != 1 {
			t.Fatalf("status=%d requests=%d err=%v", status, requests.Load(), err)
		}
		var response *HTTPError
		if errors.As(err, &response) && response.Code != "" {
			t.Fatalf("expanded HTTP code whitelist: %+v", response)
		}
	}
}

func ptrString(value string) *string { return &value }
func ptrInt64(value int64) *int64    { return &value }
