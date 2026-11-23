package gatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestInlineProjectMessageReadsAndUncertainWritesMatchSourceContract(t *testing.T) {
	actor := projectMessageActor()
	body := "handoff\nUnicode: λ; literal quotes: \"text\"; shell: $(never executed)"
	subject := "Review"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		number := requests.Add(1)
		switch number {
		case 1:
			if r.Method != http.MethodGet || r.URL.Path != "/v1/projects/demo/messages" {
				t.Errorf("list request = %s %s", r.Method, r.URL.String())
			}
			if r.URL.Query().Get("state") != "all" || r.URL.Query().Get("limit") != "2" || r.URL.Query().Get("offset") != "5" {
				t.Errorf("list query = %s", r.URL.RawQuery)
			}
			assertNoProjectMessageOriginHeaders(t, r)
			_, _ = fmt.Fprint(w, `{"threads":[],"counts":{"human_open":0,"agent_open":0,"alerts_open":0,"system_open":0,"resolved":0}}`)
		case 2:
			if r.Method != http.MethodGet || r.URL.Path != "/v1/projects/demo/messages/42/thread" {
				t.Errorf("thread request = %s %s", r.Method, r.URL.String())
			}
			assertNoProjectMessageOriginHeaders(t, r)
			_, _ = fmt.Fprint(w, `{"root":{"id":42,"project_ident":"demo","source":"agent","external_message_id":"external-42","content":"handoff","sent_at":1,"confirmed_at":null,"parent_message_id":null,"agent_id":"`+actor.SessionID+`","message_type":"message","subject":"Review","hostname":"host","event_at":1,"deliver_to_agents":false,"author_kind":"agent","resolved_at":null,"resolved_by":null},"state":"open","replies":[],"confirmations":[]}`)
		case 3:
			if r.Method != http.MethodPost || r.URL.Path != "/v1/projects/demo/messages" {
				t.Errorf("send request = %s %s", r.Method, r.URL.String())
			}
			assertProjectMessageOriginHeaders(t, r, actor)
			var sent ProjectMessageInput
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Errorf("decode sent message: %v", err)
			} else if sent.Body == nil || *sent.Body != body || sent.Subject == nil || *sent.Subject != subject {
				t.Errorf("sent text changed: %#v", sent)
			}
			_, _ = fmt.Fprint(w, `{"message_id":42,"external_message_id":"external-42"}`)
		case 4:
			assertProjectMessageOriginHeaders(t, r, actor)
			_, _ = fmt.Fprint(w, `{"external_message_id":"missing-canonical-id"}`)
		case 5:
			assertProjectMessageOriginHeaders(t, r, actor)
			http.Error(w, `{"error":"notifier unavailable"}`, http.StatusServiceUnavailable)
		default:
			t.Errorf("unexpected request %d: %s %s", number, r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "test-only", nil)
	if err != nil {
		t.Fatal(err)
	}

	limit := uint64(2)
	list, err := client.ListProjectMessageThreads(context.Background(), "demo", ProjectMessageListQuery{State: "all", Limit: &limit, Offset: 5})
	if err != nil || list == nil || list.Threads == nil {
		t.Fatalf("message list=%+v err=%v", list, err)
	}
	thread, err := client.GetProjectMessageThread(context.Background(), "demo", 42)
	if err != nil || thread == nil || thread.Root.ID != 42 {
		t.Fatalf("message thread=%+v err=%v", thread, err)
	}
	receipt, err := client.SendProjectMessage(context.Background(), "demo", ProjectMessageInput{Body: &body, Subject: &subject}, actor)
	if err != nil || receipt == nil || receipt.MessageID != 42 || receipt.ExternalMessageID != "external-42" {
		t.Fatalf("message receipt=%+v err=%v", receipt, err)
	}
	next := "next"
	if _, err := client.SendProjectMessage(context.Background(), "demo", ProjectMessageInput{Body: &next}, actor); err == nil || !strings.Contains(err.Error(), "outcome is not confirmed") {
		t.Fatalf("missing canonical ID error=%v", err)
	}
	if _, err := client.SendProjectMessage(context.Background(), "demo", ProjectMessageInput{Body: &next}, actor); err == nil || !strings.Contains(err.Error(), "outcome is not confirmed") {
		t.Fatalf("503 error=%v", err)
	} else {
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.Status != http.StatusServiceUnavailable {
			t.Fatalf("503 status was not preserved: %v", err)
		}
	}
	if _, err := client.GetProjectMessageThread(context.Background(), "demo", 0); err == nil {
		t.Fatal("zero thread ID was not rejected locally")
	}
	blank := "  "
	if _, err := client.SendProjectMessage(context.Background(), "demo", ProjectMessageInput{Body: &blank}, actor); err == nil {
		t.Fatal("blank message was not rejected locally")
	}
	if requests.Load() != 5 {
		t.Fatalf("requests=%d, want exactly five without retries or validation traffic", requests.Load())
	}
}

func TestProjectMessageEncodedLimitAppliesToRootReplyAndAction(t *testing.T) {
	const maxBytes = 1 << 20
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read message body: %v", err)
		}
		if len(raw) != maxBytes {
			t.Errorf("encoded request size=%d, want %d", len(raw), maxBytes)
		}
		_, _ = fmt.Fprint(w, `{"message_id":1,"external_message_id":"internal:1"}`)
	}))
	defer server.Close()
	client, err := New(server.URL, "test-only", nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := projectMessageActor()
	subject := "Review"
	emptyBody := ""
	template, err := json.Marshal(ProjectMessageInput{Body: &emptyBody, Subject: &subject})
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", maxBytes-len(template))
	input := ProjectMessageInput{Body: &body, Subject: &subject}
	encoded, err := json.Marshal(input)
	if err != nil || len(encoded) != maxBytes {
		t.Fatalf("boundary input size=%d err=%v, want exactly %d", len(encoded), err, maxBytes)
	}
	if _, err := client.SendProjectMessage(context.Background(), "demo", input, actor); err != nil {
		t.Fatalf("exact 1 MiB encoded request was rejected: %v", err)
	}

	tooLarge := strings.Repeat("x", maxBytes)
	oversized := ProjectMessageInput{Body: &tooLarge}
	checks := []struct {
		name string
		call func() error
	}{
		{name: "root", call: func() error {
			_, err := client.SendProjectMessage(context.Background(), "demo", oversized, actor)
			return err
		}},
		{name: "reply", call: func() error {
			_, err := client.ReplyToProjectMessage(context.Background(), "demo", 1, oversized, actor)
			return err
		}},
		{name: "action", call: func() error {
			_, err := client.TakeProjectMessageAction(context.Background(), "demo", 1, oversized, actor)
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.call(); err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
				t.Fatalf("oversized message error=%v", err)
			}
		})
	}
	if requests.Load() != 1 {
		t.Fatalf("oversized writes reached HTTP: requests=%d, want only the exact-boundary write", requests.Load())
	}
}

func assertNoProjectMessageOriginHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-agent-") {
			t.Errorf("read request unexpectedly included origin header %s", name)
		}
	}
}

func assertProjectMessageOriginHeaders(t *testing.T, r *http.Request, actor storage.AgentOrigin) {
	t.Helper()
	if r.Header.Get("X-Agent-Session-Id") != actor.SessionID || r.Header.Get("X-Agent-Instance-Id") != actor.InstanceID || r.Header.Get("X-Agent-Provider") != actor.Provider || r.Header.Get("X-Agent-OS") != actor.OS {
		t.Errorf("write origin headers do not match identity: %v", r.Header)
	}
	if r.Header.Get("X-Agent-Id") != "" {
		t.Errorf("write used legacy X-Agent-Id header: %q", r.Header.Get("X-Agent-Id"))
	}
}
