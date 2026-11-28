package amuxruntime

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/nitecon/amux/config"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestNativeACPHelper(t *testing.T) {
	if os.Getenv("AMUX_GO_ACP_TEST") != "1" {
		return
	}
	info, _ := os.Stdin.Stat()
	if info.Mode()&os.ModeCharDevice != 0 {
		os.Exit(10)
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	var promptID json.RawMessage
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(11)
		}
		respond := func(result any) { encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}) }
		switch req.Method {
		case "initialize":
			respond(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"sessionCapabilities": map[string]any{"resume": map[string]any{}}}})
		case "session/new":
			if os.Getenv("AMUX_GO_EXPECT_ENROLLED_TOOLS") == "1" && verifyEnrolledACPTools(req.Params) != nil {
				os.Exit(30)
			}
			respond(map[string]any{"sessionId": "native-headless"})
		case "session/resume":
			if os.Getenv("AMUX_GO_EXPECT_ENROLLED_TOOLS") == "1" && verifyEnrolledACPTools(req.Params) != nil {
				os.Exit(31)
			}
			var p struct {
				ID string `json:"sessionId"`
			}
			json.Unmarshal(req.Params, &p)
			if p.ID != "native-headless" {
				os.Exit(12)
			}
			os.WriteFile("resumed", []byte(p.ID), 0600)
			respond(map[string]any{})
		case "session/prompt":
			promptID = req.ID
			file, err := os.OpenFile("prompts", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				os.Exit(13)
			}
			file.WriteString("prompt\n")
			file.Close()
			encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "native-headless", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "Inspecting headless task"}}}})
			encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": "approval", "method": "session/request_permission", "params": map[string]any{"sessionId": "native-headless", "toolCall": map[string]any{"toolCallId": "tool-1", "title": "Inspect files"}, "options": []any{map[string]any{"optionId": "reject", "name": "Reject", "kind": "reject_once"}}}})
		case "":
			var result struct {
				Outcome struct {
					Outcome string `json:"outcome"`
					Option  string `json:"optionId"`
				} `json:"outcome"`
			}
			json.Unmarshal(req.Result, &result)
			if result.Outcome.Outcome != "selected" || result.Outcome.Option != "reject" {
				os.Exit(14)
			}
			encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": promptID, "result": map[string]any{"stopReason": "end_turn"}})
		}
	}
	os.WriteFile("closed", []byte("EOF"), 0600)
	os.Exit(0)
}

func verifyEnrolledACPTools(params json.RawMessage) error {
	if _, ok := os.LookupEnv("GATEWAY_API_KEY"); ok {
		return fmt.Errorf("bearer key reached enrolled native provider")
	}
	var request struct {
		Servers []struct {
			Name string   `json:"name"`
			Args []string `json:"args"`
			Env  []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"env"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(params, &request) != nil || len(request.Servers) != 1 {
		return fmt.Errorf("worker did not configure its native task MCP server")
	}
	server := request.Servers[0]
	manifest := os.Getenv("AMUX_GO_EXPECT_CLIENT_PROFILE")
	if server.Name != "amux" {
		return fmt.Errorf("worker MCP registration missing")
	}
	for _, arg := range server.Args {
		if arg == "--api-key-env" {
			return fmt.Errorf("worker MCP arguments included bearer configuration")
		}
	}
	for _, variable := range server.Env {
		if variable.Name == "GATEWAY_API_KEY" {
			return fmt.Errorf("worker MCP environment included bearer credential")
		}
	}
	path, err := config.DefaultClientPath()
	if err != nil {
		return err
	}
	settings, err := config.ReadClient(path)
	if err != nil || settings.EnrolledProfile != manifest || settings.APIKeyEnv != "" {
		return fmt.Errorf("worker MCP did not receive enrolled configuration: %v", err)
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"native-fixture","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"task_list","arguments":{}}}`,
		"",
	}, "\n")
	var output bytes.Buffer
	if err := runMCPIO(context.Background(), server.Args, io.NopCloser(strings.NewReader(input)), &output, io.Discard); err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var response struct {
			ID     int `json:"id"`
			Result struct {
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(line), &response) == nil && response.ID == 3 {
			if response.Result.IsError {
				return fmt.Errorf("paired-profile task_list returned an error: %s", line)
			}
			directory := ""
			for i := 0; i+1 < len(server.Args); i++ {
				if server.Args[i] == "--directory" {
					directory = server.Args[i+1]
					break
				}
			}
			if directory == "" {
				return fmt.Errorf("worker MCP args omitted checkout directory")
			}
			file, err := os.OpenFile(filepath.Join(directory, "enrolled-tool-calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, writeErr := file.WriteString("task_list\n")
			closeErr := file.Close()
			if writeErr != nil {
				return writeErr
			}
			if closeErr != nil {
				return closeErr
			}
			return nil
		}
	}
	return fmt.Errorf("paired-profile task_list response was missing: %s", output.String())
}

func hasCLIArgPair(args []string, name, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestHeadlessWorkerProcessPersistsPermissionsAndResumesExactly(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX signal smoke test; Windows service lifecycle is separate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if err = remote.InsertProject(ctx, storage.Project{Ident: "headless", ChannelName: "headless", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	goal := "headless-goal"
	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "headless", Title: "Headless work", Reporter: "user", Goal: &goal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(remote, "headless-secret")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	defer handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())
	client, err := gatewayclient.New(srv.URL, "headless-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "worker.db")
	checkout := t.TempDir()
	agent, _ := json.Marshal([]string{os.Args[0], "-test.run=^TestNativeACPHelper$"})
	start := func(args ...string) *workerProcess {
		return startWorkerProcess(t, ctx, state, map[string]string{"AMUX_GO_ACP_TEST": "1", "GATEWAY_API_KEY": "headless-secret"}, args...)
	}
	cmd := start("--server", srv.URL, "--project", "headless", "--task", task.ID, "--directory", checkout, "--goal", goal, "--agent-command", string(agent), "--prompt", "Inspect this task")
	runID, conversationID := cmd.runID, cmd.conversationID
	var permission storage.ConversationEvent
	for permission.ID == "" {
		changed := remote.ConversationChanges()
		events, err := remote.ConversationEvents(ctx, "headless", conversationID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Event.Kind == "permission" {
				permission = event
			}
		}
		if permission.ID == "" {
			select {
			case <-changed:
			case <-ctx.Done():
				t.Fatal("headless provider did not request permission")
			}
		}
	}
	if err = client.Enqueue(ctx, "headless", conversationID, uuid.NewString(), conversation.Command{Kind: "permission", PermissionEventID: permission.ID, OptionID: "reject"}); err != nil {
		t.Fatal(err)
	}
	for {
		changed := remote.ConversationChanges()
		commands, err := remote.ConversationCommands(ctx, "headless", conversationID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		finished := len(commands) == 2
		for _, command := range commands {
			if command.State != "done" {
				finished = false
			}
		}
		if finished {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("headless commands did not finish")
		}
	}
	stopWorkerProcess(t, ctx, cmd)
	local, err := storage.Open(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	savedRun, err := local.WorkerRun(ctx, runID)
	if err != nil || savedRun.Goal == nil || *savedRun.Goal != goal {
		t.Fatalf("selected conversation goal was not persisted: %+v %v", savedRun, err)
	}
	local.Close()
	if _, err = os.Stat(filepath.Join(checkout, "closed")); err != nil {
		t.Fatal("native provider did not receive EOF")
	}
	resumed := start("--run-id", runID)
	if resumed.runID != runID || resumed.conversationID != conversationID {
		t.Fatal("resume replaced task conversation")
	}
	for {
		changed := remote.ConversationChanges()
		events, err := remote.ConversationEvents(ctx, "headless", conversationID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		ready := 0
		for _, event := range events {
			if event.Event.Kind == "session_ready" {
				ready++
			}
		}
		if ready == 2 {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("worker did not resume native session")
		}
	}
	stopWorkerProcess(t, ctx, resumed)
	prompts, err := os.ReadFile(filepath.Join(checkout, "prompts"))
	if err != nil || string(prompts) != "prompt\n" {
		t.Fatalf("completed prompt repeated after restart: %q %v", prompts, err)
	}
	native, err := os.ReadFile(filepath.Join(checkout, "resumed"))
	if err != nil || string(native) != "native-headless" {
		t.Fatalf("native session not resumed exactly: %q %v", native, err)
	}
}

func TestWorkerCommandValidation(t *testing.T) {
	for _, args := range [][]string{{"worker", "run"}, {"worker", "run", "--state", "db"}, {"worker", "run", "--state", "db", "--run-id", uuid.NewString(), "--prompt", "again"}, {"worker", "run", "--state", "db", "--run-id", uuid.NewString(), "--goal", "release"}} {
		var output bytes.Buffer
		if err := run(context.Background(), args, &output, &output, func(string) string { return "secret" }); err == nil {
			t.Fatalf("invalid worker command accepted: %v", args)
		}
	}
}

func TestWorkerInvalidGoalFailsBeforeSavingRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if err := remote.InsertProject(ctx, storage.Project{Ident: "goal-worker", ChannelName: "goal-worker", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	actualGoal := "release"
	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "goal-worker", Title: "Scoped task", Reporter: "fixture", Goal: &actualGoal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(remote, "goal-secret")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	defer handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())
	state := filepath.Join(t.TempDir(), "worker.db")
	var output bytes.Buffer
	err = runWorker(ctx, []string{"--state", state, "--server", host.URL, "--project", "goal-worker", "--task", task.ID, "--directory", t.TempDir(), "--goal", "different"}, &output, &output, func(string) string { return "goal-secret" })
	if err == nil {
		t.Fatal("worker accepted a selected goal different from its task")
	}
	db, err := sql.Open("sqlite", state)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var runs int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_worker_runs").Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("invalid goal created %d saved worker runs", runs)
	}
}
