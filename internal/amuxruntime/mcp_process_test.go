package amuxruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nitecon/amux/providers/acp"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"

	"github.com/nitecon/amux/internal/testfixture"
)

func TestMain(m *testing.M) {
	if os.Getenv(ManagedMCPEnv) == "1" {
		if err := ServeManagedMCP(context.Background(), os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	testfixture.RunGH()
	if os.Getenv("AMUX_BUNDLED_RUNNER_FIXTURE") == "1" {
		switch filepath.Base(os.Args[0]) {
		case "memory", "memory.exe", "agent-tools", "agent-tools.exe":
			os.Exit(runBundledRunnerFixtureProcess())
		}
	}
	if len(os.Args) > 1 && ((os.Getenv("AMUX_MCP_PROCESS_TEST") == "1" && os.Args[1] == "mcp") ||
		(os.Getenv("AMUX_MESSAGES_CLI_PROCESS_TEST") == "1" && os.Args[1] == "messages")) {
		if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, os.Getenv); err != nil {
			fmt.Fprintln(os.Stderr, "amux:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestNativeACPTaskToolHelper(t *testing.T) {
	if os.Getenv("AMUX_MCP_PROCESS_TEST") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	var tool acp.MCPServer
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(41)
		}
		respond := func(value any) {
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": value})
		}
		switch request.Method {
		case "initialize":
			respond(map[string]any{"protocolVersion": 1})
		case "session/new":
			var params struct {
				MCPServers []acp.MCPServer `json:"mcpServers"`
			}
			if json.Unmarshal(request.Params, &params) != nil || len(params.MCPServers) != 1 {
				os.Exit(42)
			}
			tool = params.MCPServers[0]
			respond(map[string]any{"sessionId": "native-task-tools"})
		case "session/prompt":
			if err := runNativeMCPTaskFixture(tool); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(43)
			}
			respond(map[string]any{"stopReason": "end_turn"})
		}
	}
	os.Exit(0)
}

func runNativeMCPTaskFixture(tool acp.MCPServer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, tool.Command, tool.Args...)
	command.Env = os.Environ()
	for _, entry := range tool.Env {
		command.Env = append(command.Env, entry.Name+"="+entry.Value)
	}
	input, err := command.StdinPipe()
	if err != nil {
		return err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr synchronizedBuffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return err
	}
	defer func() { _ = input.Close(); _ = command.Wait() }()
	encoder := json.NewEncoder(input)
	scanner := bufio.NewScanner(output)
	call := func(id int, method string, params any, expectedToolError ...string) (string, error) {
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			return "", err
		}
		if !scanner.Scan() {
			return "", fmt.Errorf("MCP response missing: %s", stderr.String())
		}
		var response struct {
			Error  json.RawMessage `json:"error"`
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			return "", err
		}
		if len(response.Error) > 0 {
			return "", fmt.Errorf("MCP %s failed: %s", method, scanner.Text())
		}
		if response.Result.IsError {
			if len(expectedToolError) == 0 {
				return "", fmt.Errorf("MCP %s returned an unexpected tool error: %s", method, scanner.Text())
			}
			text := ""
			if len(response.Result.Content) > 0 {
				text = response.Result.Content[0].Text
			}
			if !strings.Contains(text, expectedToolError[0]) {
				return "", fmt.Errorf("MCP %s error %q does not contain %q", method, text, expectedToolError[0])
			}
			return text, nil
		}
		if len(expectedToolError) > 0 {
			return "", fmt.Errorf("MCP %s unexpectedly succeeded; want error containing %q", method, expectedToolError[0])
		}
		text := ""
		if len(response.Result.Content) > 0 {
			text = response.Result.Content[0].Text
		}
		return text, nil
	}
	if _, err := call(1, "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "native-fixture", "version": "1"}}); err != nil {
		return err
	}
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		return err
	}
	if _, err := call(2, "tools/list", map[string]any{}); err != nil {
		return err
	}
	taskID := os.Getenv("AMUX_MCP_TASK_ID")
	secondTaskID := os.Getenv("AMUX_MCP_NULL_TASK_ID")
	toolCall := func(id int, name string, args map[string]any, expectedError ...string) (string, error) {
		return call(id, "tools/call", map[string]any{"name": name, "arguments": args}, expectedError...)
	}
	if _, err := toolCall(3, "task_get", map[string]any{"id": taskID}); err != nil {
		return err
	}
	if _, err := toolCall(4, "task_claim", map[string]any{"id": taskID}); err != nil {
		return err
	}
	if _, err := toolCall(5, "task_comment", map[string]any{"id": taskID, "body": "Native task tools completed this work"}); err != nil {
		return err
	}
	if _, err := toolCall(6, "task_done", map[string]any{"id": taskID}, "Did this task mutate any files?"); err != nil {
		return err
	}
	if _, err := toolCall(7, "task_confirm_mutation", map[string]any{"id": taskID}); err != nil {
		return err
	}
	if _, err := toolCall(8, "task_set_changeset", map[string]any{"id": taskID}, "changeset is required"); err != nil {
		return err
	}
	if _, err := toolCall(9, "task_set_changeset", map[string]any{"id": taskID, "changeset": []string{"docs/two words.md"}}); err != nil {
		return err
	}
	beforeRejectedChangeset, err := toolCall(10, "task_get", map[string]any{"id": taskID})
	if err != nil {
		return err
	}
	if _, err := toolCall(11, "task_set_changeset", map[string]any{"id": taskID, "changeset": true}, "changeset must be null or a non-empty array"); err != nil {
		return err
	}
	afterRejectedChangeset, err := toolCall(12, "task_get", map[string]any{"id": taskID})
	if err != nil {
		return err
	}
	if beforeRejectedChangeset != afterRejectedChangeset {
		return fmt.Errorf("invalid changeset changed task state: before=%s after=%s", beforeRejectedChangeset, afterRejectedChangeset)
	}
	if _, err := toolCall(13, "task_claim", map[string]any{"id": secondTaskID}); err != nil {
		return err
	}
	if _, err := toolCall(14, "task_done", map[string]any{"id": secondTaskID}, "Did this task mutate any files?"); err != nil {
		return err
	}
	if _, err := toolCall(15, "task_set_changeset", map[string]any{"id": secondTaskID, "changeset": nil}); err != nil {
		return err
	}
	return nil
}

func TestNativeWorkerTaskToolsCompleteAssignedTask(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX process signal integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if err := remote.InsertProject(ctx, storage.Project{Ident: "tools", ChannelName: "tools", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "tools", Title: "Native tool workflow", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	noMutationTask, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "tools", Title: "Native no-mutation workflow", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := server.NewHandler(remote, "task-tool-secret")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(h)
	defer host.Close()
	defer h.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())
	command, _ := json.Marshal([]string{os.Args[0], "-test.run=^TestNativeACPTaskToolHelper$"})
	p := startWorkerProcess(t, ctx, filepath.Join(t.TempDir(), "worker.db"), map[string]string{"AMUX_MCP_PROCESS_TEST": "1", "AMUX_MCP_TASK_ID": task.ID, "AMUX_MCP_NULL_TASK_ID": noMutationTask.ID, "GATEWAY_API_KEY": "task-tool-secret"}, "--server", host.URL, "--project", "tools", "--task", task.ID, "--directory", t.TempDir(), "--agent-command", string(command), "--prompt", "Complete the task through native tools", "--no-task-tools=false")
	for {
		changed := remote.TaskEventChanges()
		current, err := remote.Task(ctx, "tools", task.ID)
		if err != nil {
			t.Fatal(err)
		}
		noMutation, noMutationErr := remote.Task(ctx, "tools", noMutationTask.ID)
		if current.Task.Status == "done" && noMutationErr == nil && noMutation.Task.Status == "done" {
			commentFound := false
			for _, comment := range current.Comments {
				if comment.Origin != nil && current.Task.OwnerOrigin != nil && *comment.Origin == *current.Task.OwnerOrigin && comment.Content == "Native task tools completed this work" {
					commentFound = true
				}
			}
			if current.Task.OwnerOrigin == nil || !commentFound || len(current.Task.Changeset) != 1 || current.Task.Changeset[0] != "docs/two words.md" {
				t.Fatalf("native tool task result %+v", current)
			}
			if len(noMutation.Task.Changeset) != 0 {
				t.Fatalf("null changeset recorded paths: %+v", noMutation.Task)
			}
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("native task tools did not complete task: %s", p.stderr)
		}
	}
	stopWorkerProcess(t, ctx, p)
}

func TestManagedMCPUsesSDKStdioAndScopesAgentIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const identity = "a0000000-0000-4000-8000-000000000001"
	cmd := exec.CommandContext(ctx, os.Args[0], "--server", "http://127.0.0.1:1",
		"--project", "example", "--conversation", "b0000000-0000-4000-8000-000000000001",
		"--session", identity, "--instance", "c0000000-0000-4000-8000-000000000001",
		"--provider", "codex", "--os", "linux", "--directory", t.TempDir())
	cmd.Env = append(os.Environ(), ManagedMCPEnv+"=1", "GATEWAY_API_KEY=test-key")
	client := sdk.NewClient(&sdk.Implementation{Name: "amux-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "agent_identity"})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || len(result.Content) != 1 {
		t.Fatalf("unexpected agent identity result: %+v", result)
	}
	content, ok := result.Content[0].(*sdk.TextContent)
	if !ok || !strings.Contains(content.Text, identity) {
		t.Fatalf("wrong agent identity: %+v", result.Content[0])
	}
}
