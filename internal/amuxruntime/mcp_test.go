package amuxruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/nitecon/amux/config"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestMCPRejectsInvalidSelectedGoal(t *testing.T) {
	writeMCPTestSettings(t, config.Client{GatewayEndpoint: "https://gateway.example"})
	args := []string{"--project", "demo", "--conversation", uuid.NewString(), "--session", uuid.NewString(), "--instance", uuid.NewString(), "--provider", "codex", "--os", "linux", "--goal", strings.Repeat("x", 129)}
	if err := runMCP(t, context.Background(), args, io.NopCloser(strings.NewReader("")), &bytes.Buffer{}, &bytes.Buffer{}, func(string) string { return "test-key" }); err == nil {
		t.Fatal("invalid selected task goal was accepted")
	}
}

func TestMCPHeadlessStartupListsToolsWithoutResolvingWorkPreferences(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", "")
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		"",
	}, "\n")
	var output bytes.Buffer
	writeMCPTestSettings(t, config.Client{GatewayEndpoint: "https://gateway.example"})
	args := []string{"--project", "demo", "--conversation", uuid.NewString(), "--session", uuid.NewString(), "--instance", uuid.NewString(), "--provider", "codex", "--os", "linux"}
	if err := runMCP(t, context.Background(), args, io.NopCloser(strings.NewReader(input)), &output, &bytes.Buffer{}, func(name string) string {
		if name == "GATEWAY_API_KEY" {
			return "test-key"
		}
		return ""
	}); err != nil {
		t.Fatalf("headless MCP initialization failed: %v", err)
	}
	if !strings.Contains(output.String(), `"name":"submit_work"`) || !strings.Contains(output.String(), `"name":"task_list"`) {
		t.Fatalf("tools/list response omitted native tools: %s", output.String())
	}
}

func TestStandaloneMCPInitializeEmitsSessionGuidance(t *testing.T) {
	writeMCPTestSettings(t, config.Client{GatewayEndpoint: "https://gateway.example"})
	priorProvider, hadProvider := os.LookupEnv("AMUX_AGENT_PROVIDER")
	priorDesktop, hadDesktop := os.LookupEnv("AMUX_DESKTOP_GUIDANCE")
	defer func() {
		if hadProvider {
			_ = os.Setenv("AMUX_AGENT_PROVIDER", priorProvider)
		} else {
			_ = os.Unsetenv("AMUX_AGENT_PROVIDER")
		}
		if hadDesktop {
			_ = os.Setenv("AMUX_DESKTOP_GUIDANCE", priorDesktop)
		} else {
			_ = os.Unsetenv("AMUX_DESKTOP_GUIDANCE")
		}
	}()
	_ = os.Unsetenv("AMUX_AGENT_PROVIDER")
	_ = os.Unsetenv("AMUX_DESKTOP_GUIDANCE")
	t.Setenv("AMUX_BUNDLED_DIR", filepath.Join(t.TempDir(), "missing"))
	var output bytes.Buffer
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n"
	if err := runMCP(t, context.Background(), nil, io.NopCloser(strings.NewReader(input)), &output, &bytes.Buffer{}, func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"instructions":"[Amux session guidance]`) {
		t.Fatalf("standalone initialize omitted physical startup guidance: %s", output.String())
	}
}

func TestMCPTaskDelegateChecksHierarchyBeforeToolCall(t *testing.T) {
	conversation, session, instance := uuid.NewString(), uuid.NewString(), uuid.NewString()
	var paths []string
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Method == http.MethodGet {
			if r.URL.Path == "/v1/tasks/capabilities" {
				_, _ = fmt.Fprint(w, `{"task_hierarchy":1}`)
				return
			}
			if r.URL.Path != "/v1/projects/demo/conversations/"+conversation {
				t.Errorf("unexpected GET %s", r.URL.Path)
			}
			_, _ = fmt.Fprintf(w, `{"id":%q,"project_ident":"demo","task_id":"44444444-4444-4444-8444-444444444444","owner":{"session_id":%q,"instance_id":%q,"provider":"codex","os":"linux"}}`, conversation, session, instance)
			return
		}
		var request struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Name != "task_delegate" || !bytes.Contains(request.Arguments, []byte(`"target_project":"other"`)) || !bytes.Contains(request.Arguments, []byte(`"parent":"44444444-4444-4444-8444-444444444444"`)) {
			t.Errorf("tool request %+v err=%v", request, err)
		}
		receipt := `{"delegation":{"id":"33333333-3333-4333-8333-333333333333","source_project_ident":"demo","source_task_id":"11111111-1111-4111-8111-111111111111","target_project_ident":"other","target_task_id":"22222222-2222-4222-8222-222222222222"},"source_task":{"id":"11111111-1111-4111-8111-111111111111","project_ident":"demo","status":"todo","kind":"delegated","delegated_to_project_ident":"other","delegated_to_task_id":"22222222-2222-4222-8222-222222222222","parent":{"id":"44444444-4444-4444-8444-444444444444","project_ident":"demo"}},"target_task":{"id":"22222222-2222-4222-8222-222222222222","project_ident":"other","status":"todo","parent":{"id":"11111111-1111-4111-8111-111111111111","project_ident":"demo"}},"message_id":42}`
		_, _ = fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}],"isError":false}`, receipt)
	}))
	defer host.Close()
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"task_delegate","arguments":{"title":"Handoff","target_project":"other"}}}`,
		"",
	}, "\n")
	writeMCPTestSettings(t, config.Client{GatewayEndpoint: host.URL})
	args := []string{"--project", "demo", "--conversation", conversation, "--session", session, "--instance", instance, "--provider", "codex", "--os", "linux"}
	var output bytes.Buffer
	if err := runMCP(t, context.Background(), args, io.NopCloser(strings.NewReader(input)), &output, &bytes.Buffer{}, func(string) string { return "test-key" }); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 || paths[0] != "/v1/tasks/capabilities" || paths[1] != "/v1/projects/demo/conversations/"+conversation || paths[2] != "/v1/projects/demo/conversations/"+conversation+"/tools" || !strings.Contains(output.String(), "message_id") {
		t.Fatalf("paths=%v output=%s", paths, output.String())
	}
}
