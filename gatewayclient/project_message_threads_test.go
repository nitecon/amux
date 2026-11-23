package gatewayclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestGetProjectMessageThreadValidatesRouteAndTupleResponse(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/projects/demo/messages/42/thread" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer thread-key" || r.Header.Get("X-Agent-Session-Id") != "" {
			t.Errorf("thread read headers=%v", r.Header)
		}
		_, _ = fmt.Fprint(w, `{"root":{"id":40,"project_ident":"demo","source":"user","content":"question","sent_at":10,"message_type":"message"},"state":"answered","replies":[{"id":42,"project_ident":"demo","source":"agent","content":"answer","sent_at":11,"parent_message_id":40,"message_type":"reply"}],"confirmations":[["session-a",12],["session-b",13]]}`)
	}))
	defer host.Close()
	client, err := New(host.URL, "thread-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := client.GetProjectMessageThread(context.Background(), "DEMO", 42)
	if err != nil || thread.Root.ID != 40 || thread.State != "answered" || len(thread.Replies) != 1 || len(thread.Confirmations) != 2 {
		t.Fatalf("thread=%+v err=%v", thread, err)
	}
	if thread.Confirmations[0].AgentID != "session-a" || thread.Confirmations[0].ConfirmedAt != 12 || requests.Load() != 1 {
		t.Fatalf("confirmation decode=%+v requests=%d", thread.Confirmations, requests.Load())
	}
}

func TestGetProjectMessageThreadRejectsMalformedOrMismatchedResponses(t *testing.T) {
	actorProject := "demo"
	for _, body := range []string{
		`{"root":{"id":40,"project_ident":"other"},"state":"open","replies":[],"confirmations":[]}`,
		`{"root":{"id":40,"project_ident":"demo"},"state":"unknown","replies":[],"confirmations":[]}`,
		`{"root":{"id":40,"project_ident":"demo"},"state":"open","replies":null,"confirmations":[]}`,
		`{"root":{"id":40,"project_ident":"demo"},"state":"open","replies":[],"confirmations":[["agent"]]}`,
		`{"root":{"id":40,"project_ident":"demo"},"state":"open","replies":[{"id":42,"project_ident":"other","parent_message_id":40}],"confirmations":[]}`,
		`{"root":{"id":40,"project_ident":"demo"},"state":"open","replies":[],"confirmations":[]}`,
	} {
		t.Run(body, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) }))
			defer host.Close()
			client, err := New(host.URL, "thread-key", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.GetProjectMessageThread(context.Background(), actorProject, 42); err == nil {
				t.Fatalf("accepted invalid thread response %s", body)
			}
		})
	}
}

func TestGetProjectMessageThreadRejectsInvalidRequestWithoutHTTP(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer host.Close()
	client, err := New(host.URL, "thread-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		project string
		id      int64
	}{
		{project: " ", id: 1},
		{project: "demo", id: 0},
		{project: "demo", id: -1},
	} {
		if _, err := client.GetProjectMessageThread(context.Background(), tc.project, tc.id); err == nil {
			t.Errorf("accepted project=%q ID=%d", tc.project, tc.id)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid arguments sent %d HTTP requests", requests.Load())
	}
}
