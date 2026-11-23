package main

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

	"github.com/nitecon/amux/providers/acp"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestNativeACPProjectMessageHelper(t *testing.T) {
	if os.Getenv("AMUX_MCP_PROJECT_MESSAGE_PROCESS_TEST") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	encoder := json.NewEncoder(os.Stdout)
	var tool acp.MCPServer
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(51)
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
				os.Exit(52)
			}
			tool = params.MCPServers[0]
			respond(map[string]any{"sessionId": "native-project-message"})
		case "session/prompt":
			if err := runNativeMCPProjectMessageFixture(tool); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(53)
			}
			respond(map[string]any{"stopReason": "end_turn"})
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(54)
	}
}

func runNativeMCPProjectMessageFixture(tool acp.MCPServer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return err
	}
	defer func() { _ = input.Close(); _ = command.Wait() }()
	encoder := json.NewEncoder(input)
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	rpc := func(id int, method string, params any) (json.RawMessage, error) {
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			return nil, err
		}
		if !scanner.Scan() {
			return nil, fmt.Errorf("MCP %s response missing: %w", method, scanner.Err())
		}
		var response struct {
			Error  json.RawMessage `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			return nil, err
		}
		if len(response.Error) != 0 && string(response.Error) != "null" {
			return nil, fmt.Errorf("MCP %s error: %s", method, response.Error)
		}
		return response.Result, nil
	}
	if _, err := rpc(1, "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "native-project-message-fixture", "version": "1"}}); err != nil {
		return err
	}
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		return err
	}
	listRaw, err := rpc(2, "tools/list", map[string]any{})
	if err != nil {
		return err
	}
	var list struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listRaw, &list); err != nil {
		return err
	}
	found := false
	for _, item := range list.Tools {
		if item.Name != "project_message_thread" {
			continue
		}
		found = true
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(item.InputSchema, &schema); err != nil {
			return err
		}
		if _, hasProject := schema.Properties["project"]; hasProject {
			return fmt.Errorf("project_message_thread exposes caller-controlled project")
		}
	}
	if !found {
		return fmt.Errorf("project_message_thread is missing from native MCP tools")
	}
	barrier := os.Getenv("AMUX_MCP_PROJECT_MESSAGE_BARRIER")
	for {
		if _, err := os.Stat(barrier); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	owner, err := os.ReadFile(barrier)
	if err != nil {
		return err
	}
	messageID := os.Getenv("AMUX_MCP_PROJECT_MESSAGE_ID")
	var id int64
	if _, err := fmt.Sscan(messageID, &id); err != nil || id <= 0 {
		return fmt.Errorf("invalid fixture message ID")
	}
	resultRaw, err := rpc(3, "tools/call", map[string]any{"name": "project_message_thread", "arguments": map[string]any{"id": id}})
	if err != nil {
		return err
	}
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		return err
	}
	if result.IsError || len(result.Content) != 1 || result.Content[0].Type != "text" {
		return fmt.Errorf("project_message_thread MCP call did not return one successful text item")
	}
	var thread storage.ProjectMessageThread
	if err := json.Unmarshal([]byte(result.Content[0].Text), &thread); err != nil {
		return err
	}
	expectedTail := os.Getenv("AMUX_MCP_PROJECT_MESSAGE_TAIL")
	if thread.Root.ID != id || thread.Root.ProjectIdent != "tools" || len(thread.Root.Content) < 64*1024 || !strings.HasSuffix(thread.Root.Content, expectedTail) || thread.State != "acknowledged" || len(thread.Confirmations) != 1 || thread.Confirmations[0].AgentID != string(owner) {
		return fmt.Errorf("thread response lost full scoped original or inbox acknowledgement: project=%s id=%d bytes=%d state=%s confirmations=%+v", thread.Root.ProjectIdent, thread.Root.ID, len(thread.Root.Content), thread.State, thread.Confirmations)
	}
	marker := os.Getenv("AMUX_MCP_PROJECT_MESSAGE_MARKER")
	return os.WriteFile(marker, []byte("validated"), 0600)
}

func TestNativeWorkerProjectMessageThreadToolReadsAcknowledgedOriginalThroughMCP(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX process signal integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if err := remote.InsertProject(ctx, storage.Project{Ident: "tools", ChannelName: "internal", RoomID: "tools", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "tools", Title: "Read project thread", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const tail = "complete-original-after-inbox-ack"
	content := strings.Repeat("original message body remains available; ", 2200) + tail
	message, err := remote.CreateHumanProjectMessage(ctx, "tools", storage.HumanProjectMessageInput{Content: content})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(remote, "project-message-tool-secret")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	defer handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())
	temp := t.TempDir()
	barrier := filepath.Join(temp, "acknowledged")
	marker := filepath.Join(temp, "validated")
	command, _ := json.Marshal([]string{os.Args[0], "-test.run=^TestNativeACPProjectMessageHelper$"})
	process := startWorkerProcess(t, ctx, filepath.Join(temp, "worker.db"), map[string]string{
		"AMUX_MCP_PROCESS_TEST": "1", "AMUX_MCP_PROJECT_MESSAGE_PROCESS_TEST": "1", "AMUX_MCP_PROJECT_MESSAGE_ID": fmt.Sprint(message.ID),
		"AMUX_MCP_PROJECT_MESSAGE_TAIL": tail, "AMUX_MCP_PROJECT_MESSAGE_BARRIER": barrier, "AMUX_MCP_PROJECT_MESSAGE_MARKER": marker,
		"GATEWAY_API_KEY": "project-message-tool-secret",
	}, "--server", host.URL, "--project", "tools", "--task", task.ID, "--directory", temp, "--agent-command", string(command), "--prompt", "Read the project message thread", "--no-task-tools=false")
	for {
		changed := remote.TaskEventChanges()
		current, err := remote.Task(ctx, "tools", task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Task.OwnerOrigin != nil {
			if confirmed, err := remote.ConfirmProjectMessage(ctx, "tools", current.Task.OwnerOrigin.SessionID, message.ID); err != nil || !confirmed {
				t.Fatalf("confirm inbox before MCP read=%t err=%v", confirmed, err)
			}
			if err := os.WriteFile(barrier, []byte(current.Task.OwnerOrigin.SessionID), 0600); err != nil {
				t.Fatal(err)
			}
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("worker did not own thread-read task: %s", process.stderr)
		}
	}
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("native MCP thread read did not complete: %s", process.stderr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	stopWorkerProcess(t, ctx, process)
}
