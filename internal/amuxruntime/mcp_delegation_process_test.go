package amuxruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/config"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestNativeMCPProcessDelegatesWithoutAgentTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "delegation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, project := range []string{"source", "target"} {
		if err := store.InsertProject(ctx, storage.Project{Ident: project, ChannelName: "none", Kind: "adhoc"}); err != nil {
			t.Fatal(err)
		}
	}
	actor := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	rootReason, details := "user-directed", "Delegate a scoped subtask"
	parent, err := store.InsertTask(ctx, storage.TaskInsert{Project: "source", Title: "Parent scope", Reporter: actor.SessionID, RootReason: &rootReason, Details: &details}, &actor)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := store.CreateConversation(ctx, "source", parent.ID, &actor)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "mcp-delegation-key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	defer handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeMCPTestSettings(t, config.Client{GatewayEndpoint: host.URL})
	command := exec.CommandContext(ctx, executable, "mcp", "--project", "source", "--conversation", conversation.ID,
		"--session", actor.SessionID, "--instance", actor.InstanceID, "--provider", actor.Provider, "--os", actor.OS)
	command.Env = workerTestEnvironment(t, map[string]string{"AMUX_MCP_PROCESS_TEST": "1", "GATEWAY_API_KEY": "mcp-delegation-key", "PATH": "", "HOME": os.Getenv("HOME"), "USERPROFILE": os.Getenv("USERPROFILE")})
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &synchronizedBuffer{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !finished && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	encoder := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	send := func(request any) map[string]json.RawMessage {
		t.Helper()
		if err := encoder.Encode(request); err != nil {
			t.Fatalf("write MCP request: %v", err)
		}
		if !scanner.Scan() {
			t.Fatalf("MCP response missing: %s (scanner error %v)", stderr.String(), scanner.Err())
		}
		var response map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatalf("decode MCP response %q: %v", scanner.Text(), err)
		}
		if len(response["error"]) != 0 {
			t.Fatalf("MCP request failed: %s", scanner.Text())
		}
		return response
	}
	initialized := send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "native-delegation-test", "version": "1"},
	}})
	if len(initialized["result"]) == 0 {
		t.Fatalf("MCP initialize response omitted result: %s", scanner.Text())
	}
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		t.Fatal(err)
	}
	response := send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{
		"name": "task_delegate", "arguments": map[string]any{"title": "Native handoff", "target_project": "target", "specification": "Keep literal $(text)"},
	}})
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response["result"], &envelope.Result); err != nil || envelope.Result.IsError || len(envelope.Result.Content) != 1 {
		t.Fatalf("MCP delegation response %s err=%v stderr=%s", response["result"], err, stderr.String())
	}
	var receipt storage.TaskDelegationResult
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &receipt); err != nil {
		t.Fatalf("invalid MCP delegation receipt %q: %v", envelope.Result.Content[0].Text, err)
	}
	if receipt.SourceTask.Parent == nil || receipt.SourceTask.Parent.ID != parent.ID || receipt.SourceTask.Parent.ProjectIdent != "source" || receipt.SourceTask.OwnerOrigin == nil || *receipt.SourceTask.OwnerOrigin != actor {
		t.Fatalf("source task lost conversation parent or origin: %+v", receipt.SourceTask)
	}
	if receipt.TargetTask.Parent == nil || receipt.TargetTask.Parent.ID != receipt.SourceTask.ID || receipt.TargetTask.Parent.ProjectIdent != "source" || receipt.TargetTask.ProjectIdent != "target" || receipt.TargetTask.Status != "todo" || receipt.TargetTask.OwnerOrigin != nil || receipt.TargetTask.OwnerAgentID != nil {
		t.Fatalf("target was not left queued and unowned: %+v", receipt.TargetTask)
	}
	storedSource, err := store.Task(ctx, "source", receipt.SourceTask.ID)
	if err != nil || storedSource == nil || storedSource.Task.OwnerOrigin == nil || *storedSource.Task.OwnerOrigin != actor {
		t.Fatalf("stored source task owner=%+v err=%v", storedSource, err)
	}
	storedTarget, err := store.Task(ctx, "target", receipt.TargetTask.ID)
	if err != nil || storedTarget == nil || storedTarget.Task.OwnerOrigin != nil || storedTarget.Task.OwnerAgentID != nil {
		t.Fatalf("stored target task=%+v err=%v", storedTarget, err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("native MCP process failed: %v: %s", err, stderr.String())
	}
	finished = true
}
