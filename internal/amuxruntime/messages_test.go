package amuxruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMessagesListAndGetUseConfiguredDefaultsWithoutNativeHeaders(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer read-key" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Agent-Session-Id") != "" || r.Header.Get("X-Agent-Instance-Id") != "" {
			t.Errorf("read request carried native identity: %v", r.Header)
		}
		switch r.URL.Path {
		case "/v1/projects/demo/messages":
			if r.Method != http.MethodGet || r.URL.Query().Get("state") != "all" || r.URL.Query().Get("limit") != "2" || r.URL.Query().Get("offset") != "5" {
				t.Errorf("list request=%s %s", r.Method, r.URL.RequestURI())
			}
			_, _ = fmt.Fprint(w, `{"threads":[],"counts":{"human_open":0,"agent_open":0,"alerts_open":0,"system_open":0,"resolved":0}}`)
		case "/v1/projects/target/messages/42/thread":
			if r.Method != http.MethodGet || r.URL.RawQuery != "" {
				t.Errorf("get request=%s %s", r.Method, r.URL.RequestURI())
			}
			_, _ = fmt.Fprint(w, `{"root":{"id":42,"project_ident":"target","source":"user","content":"handoff","sent_at":1},"state":"open","replies":[],"confirmations":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
			http.NotFound(w, r)
		}
	}))
	defer host.Close()
	env := testMessagesEnv(map[string]string{
		"AMUX_GATEWAY_URL":     host.URL,
		"AMUX_GATEWAY_PROJECT": "demo",
		"GATEWAY_API_KEY":      "read-key",
	})
	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"messages", "list", "--limit", "2", "--offset", "5"}, &out, &stderr, env); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(bytes.TrimSpace(out.Bytes())) {
		t.Fatalf("list output=%q", out.String())
	}
	out.Reset()
	if err := run(context.Background(), []string{"messages", "--server", host.URL, "--project", "target", "get", "42"}, &out, &stderr, env); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got["root"].(map[string]any)["id"] != float64(42) {
		t.Fatalf("get output=%q err=%v", out.String(), err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests=%d, want two reads", requests.Load())
	}
}

func TestMessagesSendUsesWorkerIdentityLiteralBodyAndDoesNotRetry(t *testing.T) {
	var requests atomic.Int32
	var posted []string
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := requests.Add(1)
		wantProject := "home-project"
		if call == 1 {
			wantProject = "remote-project"
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/projects/"+wantProject+"/messages" {
			t.Errorf("send request=%s %s", r.Method, r.URL.RequestURI())
		}
		if r.Header.Get("Authorization") != "Bearer selected-key" || r.Header.Get("X-Agent-Session-Id") != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" || r.Header.Get("X-Agent-Instance-Id") != "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" || r.Header.Get("X-Agent-Provider") != "claude" || r.Header.Get("X-Agent-OS") != "linux" {
			t.Errorf("send headers=%v", r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode send body: %v", err)
		}
		posted = append(posted, body["body"].(string))
		switch call {
		case 1:
			if body["subject"] != "Review" {
				t.Errorf("subject=%v", body["subject"])
			}
			_, _ = fmt.Fprint(w, `{"message_id":81,"external_message_id":"external-81"}`)
		case 2:
			if _, exists := body["subject"]; exists {
				t.Errorf("unexpected subject on literal help body: %v", body["subject"])
			}
			_, _ = fmt.Fprint(w, `{"message_id":82,"external_message_id":"external-82"}`)
		case 3:
			http.Error(w, "publication failed", http.StatusServiceUnavailable)
		default:
			t.Errorf("unexpected publication retry %d", call)
		}
	}))
	defer host.Close()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	env := testMessagesEnv(map[string]string{
		"AMUX_GATEWAY_URL":         host.URL,
		"AMUX_GATEWAY_PROJECT":     "home-project",
		"AMUX_GATEWAY_API_KEY_ENV": "CUSTOM_GATEWAY_KEY",
		"CUSTOM_GATEWAY_KEY":       "selected-key",
		"GATEWAY_API_KEY":          "wrong-default-key",
		"AMUX_GATEWAY_KEY":         "wrong-legacy-key",
		"AMUX_AGENT_SESSION_ID":    "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"AMUX_AGENT_INSTANCE_ID":   "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		"AMUX_AGENT_PROVIDER":      "claude",
		"AMUX_AGENT_OS":            "linux",
		"AMUX_AGENT_PROJECT":       filepath.Dir(cwd),
	})
	var out, stderr bytes.Buffer
	firstBody := "handoff\nUnicode: Î»; shell $(not executed)"
	if err := run(context.Background(), []string{"messages", "send", firstBody, "--subject", "Review", "--project", "remote-project"}, &out, &stderr, env); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(bytes.TrimSpace(out.Bytes())) {
		t.Fatalf("send output=%q", out.String())
	}
	out.Reset()
	if err := run(context.Background(), []string{"messages", "send", "--", "--help"}, &out, &stderr, env); err != nil {
		t.Fatalf("literal --help send: %v", err)
	}
	out.Reset()
	if err := run(context.Background(), []string{"messages", "send", "--", "-h"}, &out, &stderr, env); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("literal -h publication error=%v, want one HTTP 503", err)
	}
	if requests.Load() != 3 || len(posted) != 3 || posted[0] != firstBody || posted[1] != "--help" || posted[2] != "-h" {
		t.Fatalf("requests=%d posted=%q", requests.Load(), posted)
	}
}

func TestMessagesRejectInvalidIdentityAndArgumentsBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer host.Close()
	base := map[string]string{
		"AMUX_GATEWAY_URL":     host.URL,
		"AMUX_GATEWAY_PROJECT": "demo",
		"GATEWAY_API_KEY":      "key",
	}
	for _, tc := range []struct {
		name  string
		args  []string
		extra map[string]string
		want  string
	}{
		{name: "missing native identity", args: []string{"messages", "send", "body"}, want: "native identity"},
		{name: "nil session identity", args: []string{"messages", "send", "body"}, extra: map[string]string{"AMUX_AGENT_SESSION_ID": "00000000-0000-0000-0000-000000000000"}, want: "native identity"},
		{name: "caller cannot supply origin flags", args: []string{"messages", "send", "--session", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "body"}, want: "unknown flag"},
		{name: "invalid list limit", args: []string{"messages", "list", "--limit", "501"}, want: "between 1 and 500"},
		{name: "invalid get id", args: []string{"messages", "get", "0"}, want: "positive signed 64-bit"},
		{name: "explicit key does not fall back", args: []string{"messages", "--api-key-env", "MISSING_KEY", "list"}, extra: map[string]string{"AMUX_GATEWAY_KEY": "legacy-key"}, want: "credential is not configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := make(map[string]string, len(base)+len(tc.extra))
			for key, value := range base {
				values[key] = value
			}
			for key, value := range tc.extra {
				values[key] = value
			}
			var out, stderr bytes.Buffer
			err := run(context.Background(), tc.args, &out, &stderr, testMessagesEnv(values))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want containing %q", err, tc.want)
			}
			if out.Len() != 0 || requests.Load() != 0 {
				t.Fatalf("stdout=%q HTTP requests=%d", out.String(), requests.Load())
			}
		})
	}
	t.Run("different launch checkout", func(t *testing.T) {
		values := make(map[string]string, len(base)+5)
		for key, value := range base {
			values[key] = value
		}
		values["AMUX_AGENT_SESSION_ID"] = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		values["AMUX_AGENT_INSTANCE_ID"] = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		values["AMUX_AGENT_PROVIDER"] = "codex"
		values["AMUX_AGENT_OS"] = "linux"
		values["AMUX_AGENT_PROJECT"] = t.TempDir()
		var out, stderr bytes.Buffer
		err := run(context.Background(), []string{"messages", "send", "body"}, &out, &stderr, testMessagesEnv(values))
		if err == nil || !strings.Contains(err.Error(), "launch checkout") || out.Len() != 0 || requests.Load() != 0 {
			t.Fatalf("error=%v stdout=%q requests=%d", err, out.String(), requests.Load())
		}
	})
}

func testMessagesEnv(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}
