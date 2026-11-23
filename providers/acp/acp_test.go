package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/amux/conversation"
)

func TestMain(m *testing.M) {
	if os.Getenv("AMUX_MCP_TEST_SHIM") == "1" {
		os.Exit(runMCPTestShim())
	}
	os.Exit(m.Run())
}

func runMCPTestShim() int {
	if len(os.Args) < 2 || os.Args[1] != "mcp" {
		return 26
	}
	joined := strings.Join(os.Args[1:], " ")
	if !strings.Contains(joined, "--server https://gateway.test") || !strings.Contains(joined, "--project assigned-project") ||
		!strings.Contains(joined, "--directory /saved/worker/checkout") ||
		!strings.Contains(joined, "--conversation assigned-conversation") || !strings.Contains(joined, "--session assigned-session") ||
		!strings.Contains(joined, "--instance rotated-instance") || !strings.Contains(joined, "--provider antigravity") ||
		!strings.Contains(joined, "--os linux") || !strings.Contains(joined, "--api-key-env AMUX_API_KEY") {
		return 27
	}
	if os.Getenv("AMUX_API_KEY") != "credential-value-must-not-enter-ACP-config" || strings.Contains(joined, os.Getenv("AMUX_API_KEY")) {
		return 35
	}
	reader := bufio.NewReader(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	toolCall := 0
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if toolCall != 4 {
				return 34
			}
			return 0
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &request) != nil {
			return 28
		}
		if len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "amux-test", "version": "1"}}
		case "tools/call":
			want := []string{"task_get", "task_claim", "task_comment", "task_done"}
			if toolCall >= len(want) || request.Params.Name != want[toolCall] {
				return 29
			}
			if request.Params.Arguments["id"] != "assigned-task" {
				return 33
			}
			toolCall++
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "ok"}}}
		default:
			return 30
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}); err != nil {
			return 31
		}
	}
}

// The fixture is a native stdio subprocess. It performs no provider login or
// model requests, and verifies that no terminal/PTY is involved.
func TestACPProcessFixture(t *testing.T) {
	if os.Getenv("AMUX_ACP_FIXTURE") != "1" {
		return
	}
	info, _ := os.Stdin.Stat()
	if info.Mode()&os.ModeCharDevice != 0 {
		os.Exit(12)
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	send := func(value any) {
		if encoder.Encode(value) != nil {
			os.Exit(13)
		}
	}
	var promptID json.RawMessage
	permissionPending := false
	cancelling := false
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(14)
		}
		respond := func(result any) { send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}) }
		switch req.Method {
		case "initialize":
			var params struct {
				Version      int `json:"protocolVersion"`
				Capabilities struct {
					Terminal bool `json:"terminal"`
				} `json:"clientCapabilities"`
			}
			json.Unmarshal(req.Params, &params)
			if params.Version != 1 || params.Capabilities.Terminal {
				os.Exit(15)
			}
			capabilities := map[string]any{"loadSession": true, "sessionCapabilities": map[string]any{"resume": map[string]any{}}}
			if os.Getenv("AMUX_ACP_NO_RESUME") == "1" {
				capabilities = map[string]any{}
			}
			respond(map[string]any{"protocolVersion": 1, "agentCapabilities": capabilities})
		case "session/new":
			if os.Getenv("AMUX_ACP_EXPECT_MCP") == "1" {
				var params struct {
					Servers []MCPServer `json:"mcpServers"`
				}
				if json.Unmarshal(req.Params, &params) != nil || len(params.Servers) != 1 {
					os.Exit(24)
				}
				server := params.Servers[0]
				wantArgs := []string{"mcp", "--server", "https://gateway.test", "--project", "assigned-project",
					"--directory", "/saved/worker/checkout",
					"--conversation", "assigned-conversation", "--session", "assigned-session", "--instance",
					"rotated-instance", "--provider", "antigravity", "--os", "linux", "--api-key-env", "AMUX_API_KEY"}
				if server.Name != "amux" || server.Command != os.Getenv("AMUX_ACP_MCP_COMMAND") ||
					fmt.Sprint(server.Args) != fmt.Sprint(wantArgs) || len(server.Env) != 0 {
					os.Exit(25)
				}
				if os.Getenv("AMUX_ACP_EXECUTE_MCP") == "1" && runMCPServer(server) != nil {
					os.Exit(32)
				}
			}
			respond(map[string]any{"sessionId": "native-session"})
		case "session/resume", "session/load":
			var params struct {
				ID string `json:"sessionId"`
			}
			json.Unmarshal(req.Params, &params)
			if params.ID != "saved-session" {
				os.Exit(16)
			}
			respond(map[string]any{})
		case "session/prompt":
			promptID = req.ID
			cancelling = false
			var params struct {
				ID     string `json:"sessionId"`
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			}
			json.Unmarshal(req.Params, &params)
			if len(params.Prompt) == 0 {
				os.Exit(17)
			}
			if params.Prompt[0].Text == "wait for cancel" {
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": params.ID, "update": map[string]any{"sessionUpdate": "plan", "entries": []any{}}}})
				continue
			}
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": params.ID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "message-1", "content": map[string]any{"type": "text", "text": "Checking the task"}}}})
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": params.ID, "update": map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tool-1", "title": "Inspect files", "status": "pending"}}})
			permissionPending = true
			send(map[string]any{"jsonrpc": "2.0", "id": "permission-1", "method": "session/request_permission", "params": map[string]any{"sessionId": params.ID, "toolCall": map[string]any{"toolCallId": "tool-1"}, "options": []any{map[string]any{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"}, map[string]any{"optionId": "reject-once", "name": "Reject", "kind": "reject_once"}}}})
		case "session/cancel":
			cancelling = true
			if !permissionPending {
				send(map[string]any{"jsonrpc": "2.0", "id": promptID, "result": map[string]any{"stopReason": "cancelled"}})
			}
		default:
			if req.ID != nil && req.Method == "" {
				var permission struct {
					Outcome struct {
						Outcome string `json:"outcome"`
						Option  string `json:"optionId"`
					} `json:"outcome"`
				}
				json.Unmarshal(req.Result, &permission)
				permissionPending = false
				if cancelling {
					if permission.Outcome.Outcome != "cancelled" {
						os.Exit(19)
					}
					send(map[string]any{"jsonrpc": "2.0", "id": promptID, "result": map[string]any{"stopReason": "cancelled"}})
					continue
				}
				if permission.Outcome.Outcome != "selected" || permission.Outcome.Option != "reject-once" {
					os.Exit(18)
				}
				send(map[string]any{"jsonrpc": "2.0", "id": promptID, "result": map[string]any{"stopReason": "end_turn"}})
			}
		}
	}
	os.Exit(0)
}

func runMCPServer(server MCPServer) error {
	command := exec.Command(server.Command, server.Args...)
	command.Env = append(os.Environ(), "AMUX_MCP_TEST_SHIM=1")
	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return err
	}
	reader := bufio.NewReader(stdout)
	encoder := json.NewEncoder(stdin)
	call := func(id int, method string, params any) error {
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			return err
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return err
		}
		var response struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(line, &response); err != nil || response.ID != id || len(response.Error) > 0 {
			return fmt.Errorf("bad MCP response: %s", line)
		}
		return nil
	}
	if err := call(1, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "acp-fixture", "version": "1"}}); err != nil {
		command.Process.Kill()
		return err
	}
	if _, err := stdin.Write([]byte("{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n")); err != nil {
		command.Process.Kill()
		return err
	}
	calls := []struct {
		name string
		args map[string]string
	}{
		{"task_get", map[string]string{"id": "assigned-task"}},
		{"task_claim", map[string]string{"id": "assigned-task"}},
		{"task_comment", map[string]string{"id": "assigned-task", "body": "working"}},
		{"task_done", map[string]string{"id": "assigned-task"}},
	}
	for i, item := range calls {
		if err := call(i+2, "tools/call", map[string]any{"name": item.name, "arguments": item.args}); err != nil {
			command.Process.Kill()
			return err
		}
	}
	stdin.Close()
	if err := command.Wait(); err != nil {
		return fmt.Errorf("MCP test shim exited: %w: %s", err, stderr.String())
	}
	return nil
}

func TestACPConfiguredMCPServerReachesSessionNew(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := os.Args[0]
	t.Setenv("AMUX_API_KEY", "credential-value-must-not-enter-ACP-config")
	env := append(os.Environ(), "AMUX_ACP_FIXTURE=1", "AMUX_ACP_EXPECT_MCP=1", "AMUX_ACP_EXECUTE_MCP=1", "AMUX_ACP_MCP_COMMAND="+command)
	session, err := Start(ctx, Config{
		Command: []string{os.Args[0], "-test.run=^TestACPProcessFixture$"}, Directory: t.TempDir(), Env: env,
		MCPServers: []MCPServer{{Name: "amux", Command: command, Args: []string{"mcp", "--server", "https://gateway.test", "--project", "assigned-project", "--directory", "/saved/worker/checkout", "--conversation", "assigned-conversation", "--session", "assigned-session", "--instance", "rotated-instance", "--provider", "antigravity", "--os", "linux", "--api-key-env", "AMUX_API_KEY"}}},
	}, func(context.Context, conversation.Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func fixtureSession(t *testing.T, resume string) (*Session, <-chan conversation.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	events := make(chan conversation.Event, 32)
	config := Config{Command: []string{os.Args[0], "-test.run=^TestACPProcessFixture$"}, Directory: t.TempDir(), ResumeID: resume, Env: append(os.Environ(), "AMUX_ACP_FIXTURE=1")}
	session, err := Start(ctx, config, func(ctx context.Context, event conversation.Event) error {
		select {
		case events <- event:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session, events
}

func TestACPTextToolsAndBidirectionalPermission(t *testing.T) {
	session, events := fixtureSession(t, "")
	if session.ID() != "native-session" {
		t.Fatal(session.ID())
	}
	done := make(chan error, 1)
	go func() { _, err := session.Prompt(context.Background(), "Inspect this task"); done <- err }()
	var kinds []string
	for {
		select {
		case event := <-events:
			kinds = append(kinds, event.Kind)
			if event.Kind == "permission" {
				if err := session.ResolvePermission(context.Background(), event.RequestID, "invented-choice"); err == nil {
					t.Fatal("unoffered permission option accepted")
				}
				if err := session.ResolvePermission(context.Background(), event.RequestID, "reject-once"); err != nil {
					t.Fatal(err)
				}
			}
			if event.Kind == "turn_completed" {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if fmt.Sprint(kinds) != "[session_ready text_delta tool_call permission turn_completed]" {
					t.Fatalf("event order %v", kinds)
				}
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("provider stalled")
		}
	}
}

func TestACPExactResumeAndCancellation(t *testing.T) {
	session, events := fixtureSession(t, "saved-session")
	if session.ID() != "saved-session" {
		t.Fatal("resumed a different conversation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := session.Prompt(ctx, "wait for cancel"); done <- err }()
	for {
		select {
		case event := <-events:
			if event.Kind == "state" {
				cancel()
				goto waiting
			}
		case <-time.After(3 * time.Second):
			t.Fatal("prompt never entered provider")
		}
	}
waiting:
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation hidden")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not finish")
	}
}

func TestACPCancelPendingPermissionThenReuseSession(t *testing.T) {
	session, events := fixtureSession(t, "")
	for turn := 0; turn < 2; turn++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := session.Prompt(ctx, "Inspect this task"); done <- err }()
		var requestID string
		for requestID == "" {
			select {
			case event := <-events:
				if event.Kind == "permission" {
					requestID = event.RequestID
				}
			case <-time.After(3 * time.Second):
				cancel()
				t.Fatal("permission never arrived")
			}
		}
		cancel()
		select {
		case err := <-done:
			if err != context.Canceled {
				t.Fatalf("cancel result: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("provider did not acknowledge cancelled permission")
		}
		if err := session.ResolvePermission(context.Background(), requestID, "allow-once"); err == nil {
			t.Fatal("cancelled permission remained actionable")
		}
	}
}

func TestACPUnsupportedResumeDoesNotStartFresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	emitted := false
	session, err := Start(ctx, Config{
		Command:   []string{os.Args[0], "-test.run=^TestACPProcessFixture$"},
		Directory: t.TempDir(), ResumeID: "saved-session",
		Env: append(os.Environ(), "AMUX_ACP_FIXTURE=1", "AMUX_ACP_NO_RESUME=1"),
	}, func(context.Context, conversation.Event) error { emitted = true; return nil })
	if err == nil {
		session.Close()
		t.Fatal("unsupported resume started a conversation")
	}
	if emitted {
		t.Fatal("unsupported resume emitted a ready conversation")
	}
}
