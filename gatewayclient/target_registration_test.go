package gatewayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func TestTargetRegistrationUsesNativeHeadersAndRustWireShape(t *testing.T) {
	origin := agent.Origin{
		SessionID:  "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		Provider:   "codex", OS: "linux",
	}
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/projects/local/task-targets" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		}
		if r.Method == http.MethodPost {
			for name, want := range map[string]string{
				"X-Agent-Session-Id":  origin.SessionID,
				"X-Agent-Instance-Id": origin.InstanceID,
				"X-Agent-Provider":    origin.Provider,
				"X-Agent-Os":          origin.OS,
			} {
				if got := r.Header.Get(name); got != want {
					t.Errorf("%s=%q, want %q", name, got, want)
				}
			}
		}
		switch r.Method {
		case http.MethodPost:
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if _, ok := body["origin"]; ok {
				t.Errorf("origin must come from headers, body=%s", body["origin"])
			}
			for _, field := range []string{"generation", "canonical_remote", "display_name", "hostname"} {
				if _, ok := body[field]; !ok {
					t.Errorf("POST omitted %s", field)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"target":{"origin":{"session_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","instance_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","provider":"codex","os":"linux"},"generation":"cccccccc-cccc-4ccc-8ccc-cccccccccccc"},"seen_at":1800000000000,"expires_at":1800000120000,"canonical_remote":null,"display_name":null,"hostname":null}`))
		case http.MethodGet:
			if r.Header.Get("X-Agent-Session-Id") != "" {
				t.Error("GET must not invent an origin")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"targets":[{"target":{"origin":{"session_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","instance_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","provider":"codex","os":"linux"},"generation":"cccccccc-cccc-4ccc-8ccc-cccccccccccc"},"seen_at":1800000000000,"expires_at":1800000120000,"display_name":null,"hostname":null}],"canonical_remote":null}`))
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "test-key", nil)
	if err != nil {
		t.Fatal(err)
	}

	registered, err := client.RegisterTarget(context.Background(), "local", storage.TargetRegistration{
		Origin: origin, Generation: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
	})
	if err != nil || registered.Target.Origin != origin || registered.Target.Generation == "" {
		t.Fatalf("registered target %+v, err=%v", registered, err)
	}
	targets, err := client.Targets(context.Background(), "local")
	if err != nil || len(targets) != 1 || targets[0].Target.Origin != origin {
		t.Fatalf("targets %+v, err=%v", targets, err)
	}
	if requests != 2 {
		t.Fatalf("unexpected retry behavior: sent %d requests", requests)
	}
}
