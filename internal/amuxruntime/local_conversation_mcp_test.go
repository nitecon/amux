package amuxruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/internal/clientconfig"
	"github.com/nitecon/amux/internal/localconversation"
	"github.com/nitecon/amux/mcpstdio"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestLocalConversationMCPListsAndExecutesGatewayAndBundledTools(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertProject(ctx, storage.Project{Ident: "target-demo", ChannelName: "target-demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	msg, err := store.CreateHumanProjectMessage(ctx, "demo", storage.HumanProjectMessageInput{Content: "Check local conversation MCP"})
	if err != nil {
		t.Fatal(err)
	}
	msgID := msg.ID
	handler, err := server.NewHandler(store, "local-mcp-key")
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(handler)
	defer gateway.Close()

	binDir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	copyBundledTestBinary(t, executable, filepath.Join(binDir, bundledToolFileName("agent-tools")))
	workDir := t.TempDir()
	t.Setenv("AMUX_DESKTOP_GUIDANCE", "")
	if err := os.Unsetenv("AMUX_DESKTOP_GUIDANCE"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "bundled.json")
	t.Setenv("AMUX_BUNDLED_DIR", binDir)
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE", "1")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_MARKER", marker)
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDOUT", "bundled local output")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDERR", "")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_EXIT", "0")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_WAIT", "")

	convID, sessionID, instanceID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	args := []string{
		"--server", gateway.URL, "--project", "demo", "--directory", workDir,
		"--conversation", convID, "--session", sessionID, "--instance", instanceID,
		"--provider", "claude", "--os", "macos", "--local-conversation", "--api-key-env", "LOCAL_MCP_KEY",
	}
	getenv := func(name string) string {
		if name == "LOCAL_MCP_KEY" {
			return "local-mcp-key"
		}
		return os.Getenv(name)
	}

	callMCP := func(requests ...string) []string {
		t.Helper()
		lines := append([]string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		}, requests...)
		lines = append(lines, "")
		var output, stderr bytes.Buffer
		if err := runMCP(ctx, args, io.NopCloser(strings.NewReader(strings.Join(lines, "\n"))), &output, &stderr, getenv); err != nil {
			t.Fatalf("runMCP failed: %v stderr=%s", err, stderr.String())
		}
		return strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	}

	parseToolResult := func(line string) mcpstdio.Result {
		t.Helper()
		var envelope struct {
			Result mcpstdio.Result `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("unmarshal response %s: %v", line, err)
		}
		return envelope.Result
	}

	step := func(id int, name string, params map[string]any) string {
		raw, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": params},
		})
		return string(raw)
	}

	// 1. Verify catalog, local schemas, supported read/create/bundled tools, and omitted tools.
	responses := callMCP(
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		step(3, "task_create", map[string]any{"title": "Local chat task", "root_reason": "user-directed", "specification": "Verify local conversation MCP"}),
		step(4, "code_read", map[string]any{"file": "README.md"}),
		step(5, "project_message_thread", map[string]any{"id": msgID}),
		step(6, "agent_identity", map[string]any{}),
		step(7, "task_set_requester", map[string]any{"id": uuid.NewString()}),
		step(8, "task_claim", map[string]any{"id": uuid.NewString()}),
		step(9, "submit_work", map[string]any{"task_id": uuid.NewString(), "message": "nope", "paths": []string{"a.go"}}),
		step(10, "task_finish_review", map[string]any{"id": uuid.NewString(), "outcome": "approved", "summary": "nope"}),
		step(11, "task_reassign", map[string]any{"id": uuid.NewString(), "recipient_session_id": uuid.NewString()}),
	)
	if len(responses) != 11 {
		t.Fatalf("responses=%d, want 11: %v", len(responses), responses)
	}
	var initialized struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(responses[0]), &initialized); err != nil {
		t.Fatal(err)
	}
	instructions := initialized.Result.Instructions
	for _, want := range []string{"Local conversations", "task_list", "task_get", "task_comment", "task_create", "task_delegate", "Use a task worker", "code_read"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("local conversation MCP instructions missing %q: %q", want, instructions)
		}
	}
	for _, forbidden := range []string{"claim before starting", "submit-work"} {
		if strings.Contains(instructions, forbidden) {
			t.Errorf("local conversation MCP instructions contain worker guidance %q: %q", forbidden, instructions)
		}
	}
	var listed struct {
		Result struct {
			Tools []mcpstdio.Tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(responses[1]), &listed); err != nil {
		t.Fatal(err)
	}
	toolsByName := make(map[string]mcpstdio.Tool, len(listed.Result.Tools))
	for _, tool := range listed.Result.Tools {
		toolsByName[tool.Name] = tool
	}
	for _, want := range []string{"task_get", "project_message_thread", "task_list", "task_create", "task_delegate", "task_comment", "agent_identity", "task_agents", "code_read"} {
		if _, ok := toolsByName[want]; !ok {
			t.Fatalf("local conversation MCP catalog missing %q: %v", want, toolsByName)
		}
	}
	for _, forbidden := range []string{
		"task_set_requester", "task_claim", "task_started", "task_release",
		"task_user_review", "task_done", "task_confirm_mutation", "task_set_changeset",
		"task_finish_review", "task_update_delegation", "task_reassign", "submit_work",
	} {
		if _, ok := toolsByName[forbidden]; ok {
			t.Fatalf("local conversation MCP catalog exposed unsupported tool %q", forbidden)
		}
		if strings.Contains(instructions, forbidden) {
			t.Errorf("local conversation MCP instructions mention unsupported tool %q: %q", forbidden, instructions)
		}
	}
	// Verify local schemas match executable behavior (explicit parent required for task_delegate; no notify_results).
	var createSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(toolsByName["task_create"].InputSchema, &createSchema); err != nil {
		t.Fatal(err)
	}
	if _, hasNotify := createSchema.Properties["notify_results"]; hasNotify {
		t.Fatalf("local task_create schema must omit notify_results: %s", toolsByName["task_create"].InputSchema)
	}
	var delegateSchema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(toolsByName["task_delegate"].InputSchema, &delegateSchema); err != nil {
		t.Fatal(err)
	}
	if _, hasNotify := delegateSchema.Properties["notify_results"]; hasNotify {
		t.Fatalf("local task_delegate schema must omit notify_results: %s", toolsByName["task_delegate"].InputSchema)
	}
	if strings.Contains(strings.ToLower(toolsByName["task_delegate"].Description), "defaults to") ||
		strings.Contains(strings.ToLower(delegateSchema.Properties["parent"].Description), "defaults to") {
		t.Fatalf("local task_delegate schema must document explicit parent: desc=%q prop=%q",
			toolsByName["task_delegate"].Description, delegateSchema.Properties["parent"].Description)
	}
	hasRequiredParent := false
	for _, req := range delegateSchema.Required {
		hasRequiredParent = hasRequiredParent || req == "parent"
	}
	if !hasRequiredParent {
		t.Fatalf("local task_delegate schema must require parent: %v", delegateSchema.Required)
	}

	createdResult := parseToolResult(responses[2])
	if createdResult.IsError || len(createdResult.Content) != 1 {
		t.Fatalf("task_create failed: %+v", createdResult)
	}
	var createdTask storage.Task
	if err := json.Unmarshal([]byte(createdResult.Content[0].Text), &createdTask); err != nil || createdTask.ID == "" {
		t.Fatalf("task_create payload invalid: %q (%v)", createdResult.Content[0].Text, err)
	}
	if bundledResult := parseToolResult(responses[3]); bundledResult.IsError || len(bundledResult.Content) != 1 || bundledResult.Content[0].Text != "bundled local output" {
		t.Fatalf("bundled code_read failed: %+v", bundledResult)
	}
	if threadResult := parseToolResult(responses[4]); threadResult.IsError || !strings.Contains(threadResult.Content[0].Text, "Check local conversation MCP") {
		t.Fatalf("project_message_thread failed: %+v", threadResult)
	}
	if idResult := parseToolResult(responses[5]); idResult.IsError || !strings.Contains(idResult.Content[0].Text, sessionID) {
		t.Fatalf("agent_identity failed: %+v", idResult)
	}
	for idx, forbidden := range []string{"task_set_requester", "task_claim", "submit_work", "task_finish_review", "task_reassign"} {
		resp := responses[6+idx]
		if !strings.Contains(resp, `"error"`) || !strings.Contains(resp, "unknown tool: "+forbidden) {
			t.Fatalf("expected JSON-RPC unknown tool error for %s: %s", forbidden, resp)
		}
	}

	// 2. Verify list, get, comment, explicit-parent delegation, and rejection of omitted parent / notify_results.
	workflow := callMCP(
		step(2, "task_list", map[string]any{}),
		step(3, "task_get", map[string]any{"id": createdTask.ID}),
		step(4, "task_comment", map[string]any{"id": createdTask.ID, "body": "Progress from local conversation"}),
		step(5, "task_create", map[string]any{"title": "No subscriptions", "parent": createdTask.ID, "notify_results": true}),
		step(6, "task_delegate", map[string]any{"title": "Missing parent", "target_project": "target-demo"}),
		step(7, "task_delegate", map[string]any{"title": "No subscriptions", "target_project": "target-demo", "parent": createdTask.ID, "notify_results": true}),
		step(8, "task_delegate", map[string]any{"title": "Delegated from local chat", "target_project": "target-demo", "parent": createdTask.ID, "specification": "Cross-project handoff"}),
	)
	if len(workflow) != 8 {
		t.Fatalf("workflow responses=%d, want 8: %v", len(workflow), workflow)
	}
	if res := parseToolResult(workflow[1]); res.IsError || !strings.Contains(res.Content[0].Text, createdTask.ID) {
		t.Fatalf("task_list failed: %+v", res)
	}
	if res := parseToolResult(workflow[2]); res.IsError || !strings.Contains(res.Content[0].Text, "Local chat task") {
		t.Fatalf("task_get failed: %+v", res)
	}
	if res := parseToolResult(workflow[3]); res.IsError || !strings.Contains(res.Content[0].Text, "Progress from local conversation") {
		t.Fatalf("task_comment failed: %+v", res)
	}
	if res := parseToolResult(workflow[4]); !res.IsError || !strings.Contains(res.Content[0].Text, `unknown argument "notify_results"`) {
		t.Fatalf("task_create with notify_results should be rejected: %+v", res)
	}
	if res := parseToolResult(workflow[5]); !res.IsError || !strings.Contains(res.Content[0].Text, "parent is required") {
		t.Fatalf("task_delegate without parent should be rejected: %+v", res)
	}
	if res := parseToolResult(workflow[6]); !res.IsError || !strings.Contains(res.Content[0].Text, `unknown argument "notify_results"`) {
		t.Fatalf("task_delegate with notify_results should be rejected: %+v", res)
	}
	if res := parseToolResult(workflow[7]); res.IsError || !strings.Contains(res.Content[0].Text, `"target_project_ident":"target-demo"`) {
		t.Fatalf("task_delegate with explicit parent failed: %+v", res)
	}
}

func TestLocalConversationMCPRejectsInvalidFlagCombinations(t *testing.T) {
	if err := runMCP(context.Background(), []string{"--local-conversation"}, io.NopCloser(strings.NewReader("")), &bytes.Buffer{}, &bytes.Buffer{}, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "native MCP requires") {
		t.Fatalf("partial --local-conversation = %v, want native MCP validation error", err)
	}
	combined := []string{
		"--server", "https://gateway.example", "--project", "demo",
		"--conversation", uuid.NewString(), "--session", uuid.NewString(),
		"--instance", uuid.NewString(), "--provider", "codex", "--os", "macos",
		"--local-conversation", "--local-task", uuid.NewString(),
	}
	if err := runMCP(context.Background(), combined, io.NopCloser(strings.NewReader("")), &bytes.Buffer{}, &bytes.Buffer{}, func(string) string { return "key" }); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("combined --local-conversation and --local-task = %v, want conflict error", err)
	}
}

func TestConfiguredClientLocalConversationsRegisterAmuxMCP(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("uses POSIX provider shims")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LOCAL_CHAT_MCP_KEY", "test-key")
	// Codex startup runs bundled memory recall first; skip it so a slow recall
	// cannot delay the shim past the stream window below.
	t.Setenv("AMUX_BUNDLED_DIR", filepath.Join(t.TempDir(), "missing"))

	const project = "local-mcp-demo"
	remoteURL := "https://github.com/nitecon/local-mcp-demo"
	checkoutDir := makeConfiguredCheckout(t, t.TempDir(), "local-mcp-demo", remoteURL)

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: project, ChannelName: project, Kind: "repo", CanonicalRemote: &remoteURL}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(handler)
	defer gateway.Close()

	// Create tiny shims for claude and codex on PATH that capture their exact argv and exit.
	binDir := t.TempDir()
	claudeArgsFile := filepath.Join(t.TempDir(), "claude-args.txt")
	codexArgsFile := filepath.Join(t.TempDir(), "codex-args.txt")
	writeShim := func(name, outFile string) {
		t.Helper()
		script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + outFile + "\"\nexit 1\n"
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeShim("claude", claudeArgsFile)
	writeShim("codex", codexArgsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	configPath := filepath.Join(home, ".amux", "config.yml")
	journal := filepath.Join(t.TempDir(), "workers.db")
	cfg := clientconfig.Config{
		GatewayEndpoint: gateway.URL,
		APIKeyEnv:       "LOCAL_CHAT_MCP_KEY",
		WorkerJournal:   journal,
		Projects:        map[string]string{project: checkoutDir},
		Providers:       []string{},
	}
	if err := clientconfig.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	clientCtx, stopClient := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runHeadlessClient(clientCtx, nil, io.Discard, io.Discard) }()
	defer func() {
		stopClient()
		if err := <-done; err != nil {
			t.Errorf("configured client: %v", err)
		}
	}()

	for {
		if _, err := os.Stat(configPath + ".control"); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("client control socket did not appear")
		case <-time.After(25 * time.Millisecond):
		}
	}

	remote := localconversation.Remote{ConfigPath: configPath}
	openAndCapture := func(provider, outFile string) []string {
		t.Helper()
		info, err := remote.Create(ctx, project, provider, "", localconversation.Preferences{})
		if err != nil {
			t.Fatalf("create %s chat: %v", provider, err)
		}
		streamCtx, stopStream := context.WithTimeout(ctx, 5*time.Second)
		defer stopStream()
		_ = remote.Stream(streamCtx, project, info.ID, gatewayclient.Cursor{}, func(frame gatewayclient.Frame) error {
			if frame.Type == "local_failed" || frame.Type == "local_ready" {
				stopStream()
			}
			return nil
		})
		raw, err := os.ReadFile(outFile)
		if err != nil {
			t.Fatalf("%s shim did not run: %v", provider, err)
		}
		return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	}

	claudeArgs := openAndCapture("claude", claudeArgsFile)
	mcpConfigJSON := ""
	for i := 0; i+1 < len(claudeArgs); i++ {
		if claudeArgs[i] == "--mcp-config" {
			mcpConfigJSON = claudeArgs[i+1]
			break
		}
	}
	if mcpConfigJSON == "" {
		t.Fatalf("Claude standard conversation omitted --mcp-config: %q", claudeArgs)
	}
	var claudeMCP struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(mcpConfigJSON), &claudeMCP); err != nil {
		t.Fatalf("invalid Claude --mcp-config JSON %q: %v", mcpConfigJSON, err)
	}
	amuxServer, ok := claudeMCP.Servers["amux"]
	if !ok || amuxServer.Command == "" {
		t.Fatalf("Claude --mcp-config missing amux server: %+v", claudeMCP)
	}
	claudeJoined := strings.Join(amuxServer.Args, "\x00")
	for _, want := range []string{"mcp", "--server\x00" + gateway.URL, "--project\x00" + project, "--directory\x00" + checkoutDir, "--local-conversation", "--api-key-env\x00LOCAL_CHAT_MCP_KEY"} {
		if !strings.Contains(claudeJoined, want) {
			t.Fatalf("Claude amux MCP args missing %q: %q", want, amuxServer.Args)
		}
	}

	codexArgs := openAndCapture("codex", codexArgsFile)
	codexJoined := strings.Join(codexArgs, "\n")
	for _, want := range []string{
		"mcp_servers.amux.command=",
		"mcp_servers.amux.args=[",
		`"--local-conversation"`,
		`"--project","` + project + `"`,
		`mcp_servers.amux.default_tools_approval_mode="approve"`,
		"tools.update_plan.enabled=false",
	} {
		if !strings.Contains(codexJoined, want) {
			t.Fatalf("Codex standard conversation args missing %q: %q", want, codexArgs)
		}
	}
}
