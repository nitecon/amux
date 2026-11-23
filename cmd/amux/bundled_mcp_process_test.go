package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nitecon/amux/mcpstdio"
)

func TestStandaloneMCPListsAndRunsOnlyAvailableBundledTools(t *testing.T) {
	binDirectory := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	copyBundledTestBinary(t, executable, filepath.Join(binDirectory, bundledToolFileName("memory")))
	copyBundledTestBinary(t, executable, filepath.Join(binDirectory, bundledToolFileName("agent-tools")))
	workDirectory := t.TempDir()
	marker := filepath.Join(t.TempDir(), "child.json")
	t.Setenv("AMUX_BUNDLED_DIR", binDirectory)
	t.Setenv("AMUX_AGENT_PROJECT", workDirectory)
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE", "1")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_MARKER", marker)
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDOUT", "fixture output")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDERR", "")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_EXIT", "0")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_WAIT", "")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_KEEP", "inherited")
	t.Setenv("AGENT_MEMORY_NO_UPDATE", "0")
	t.Setenv("AGENT_TOOLS_NO_UPDATE", "0")

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory_search","arguments":{"query":"-literal $(touch should-not-exist) λ"}}}`,
		"",
	}, "\n")
	var output, stderr bytes.Buffer
	if err := runMCP(context.Background(), nil, io.NopCloser(strings.NewReader(input)), &output, &stderr, os.Getenv); err != nil {
		t.Fatalf("standalone MCP: %v; stderr=%s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("MCP responses=%d, want initialize/list/call: %s", len(lines), output.String())
	}
	var listed struct {
		Result struct {
			Tools []mcpstdio.Tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Result.Tools) != len(bundledToolCatalog) {
		t.Fatalf("standalone catalog has %d tools, want %d: %s", len(listed.Result.Tools), len(bundledToolCatalog), lines[1])
	}
	for _, tool := range listed.Result.Tools {
		if strings.HasPrefix(tool.Name, "task_") || tool.Name == "submit_work" {
			t.Fatalf("standalone catalog exposed native tool %q", tool.Name)
		}
	}
	var called struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &called); err != nil || len(called.Result.Content) != 1 || called.Result.Content[0].Text != "fixture output" {
		t.Fatalf("bundled call result=%+v err=%v line=%s", called, err, lines[2])
	}
	record := readBundledRunnerFixtureRecord(t, marker)
	canonicalWorkDirectory, err := filepath.EvalSymlinks(workDirectory)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"search", "--", "-literal $(touch should-not-exist) λ"}
	if !reflect.DeepEqual(record.Args, wantArgs) || record.Directory != canonicalWorkDirectory || record.MemoryNoUpdate != "1" || record.ToolsNoUpdate != "1" || record.Inherited != "inherited" || !record.StdinEOF {
		t.Fatalf("bundled child record=%+v, want args=%q cwd=%q with inherited env and EOF stdin", record, wantArgs, canonicalWorkDirectory)
	}
	if _, err := os.Stat(filepath.Join(workDirectory, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("literal argument was executed as shell: stat error=%v", err)
	}
}

func TestNativeMCPRunsBundledToolsLocallyWithoutGatewayRequest(t *testing.T) {
	binDirectory := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	copyBundledTestBinary(t, executable, filepath.Join(binDirectory, bundledToolFileName("agent-tools")))
	workDirectory := t.TempDir()
	marker := filepath.Join(t.TempDir(), "native-child.json")
	var gatewayRequests atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayRequests.Add(1)
		http.Error(w, "bundled tools must stay local", http.StatusInternalServerError)
	}))
	defer trap.Close()
	t.Setenv("AMUX_MCP_PROCESS_TEST", "1")
	t.Setenv("AMUX_BUNDLED_DIR", binDirectory)
	t.Setenv("AMUX_AGENT_PROJECT", t.TempDir()) // explicit saved directory must take precedence
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE", "1")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_MARKER", marker)
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDOUT", "native local fixture")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDERR", "")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_EXIT", "0")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_WAIT", "")
	t.Setenv("GATEWAY_API_KEY", "fixture-key")

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"code_read","arguments":{"file":"README.md"}}}`,
		"",
	}, "\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "mcp", "--server", trap.URL, "--project", "native-project",
		"--conversation", "018f0000-0000-7000-8000-000000000001", "--session", "018f0000-0000-7000-8000-000000000002",
		"--instance", "018f0000-0000-7000-8000-000000000003", "--provider", "codex", "--os", "linux", "--directory", workDirectory)
	command.Env = os.Environ()
	command.Stdin = strings.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("native MCP process: %v stderr=%s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("MCP responses=%d, want initialize/list/call: %s", len(lines), output)
	}
	var listed struct {
		Result struct {
			Tools []mcpstdio.Tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &listed); err != nil {
		t.Fatal(err)
	}
	toolNames := make(map[string]bool, len(listed.Result.Tools))
	for _, tool := range listed.Result.Tools {
		toolNames[tool.Name] = true
	}
	if !toolNames["task_list"] || !toolNames["submit_work"] || !toolNames["code_read"] {
		t.Fatalf("native catalog lost task, submission, or bundled tools: %v", toolNames)
	}
	var called struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &called); err != nil || called.Result.IsError || len(called.Result.Content) != 1 || called.Result.Content[0].Text != "native local fixture" {
		t.Fatalf("native bundled call response=%+v err=%v line=%s", called, err, lines[2])
	}
	if got := gatewayRequests.Load(); got != 0 {
		t.Fatalf("bundled call made %d gateway requests, want 0", got)
	}
	record := readBundledRunnerFixtureRecord(t, marker)
	canonicalWorkDirectory, err := filepath.EvalSymlinks(workDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if record.Directory != canonicalWorkDirectory || !record.StdinEOF || record.MemoryNoUpdate != "1" || record.ToolsNoUpdate != "1" {
		t.Fatalf("native bundled child context=%+v", record)
	}
}

func TestNativeMCPInitializeUsesMemoryOnlyRecallAndCapabilityHints(t *testing.T) {
	binDirectory := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	copyBundledTestBinary(t, executable, filepath.Join(binDirectory, bundledToolFileName("memory")))
	workDirectory := t.TempDir()
	if err := os.Mkdir(filepath.Join(workDirectory, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDirectory, ".claude", "settings.json"), []byte(`{"hooks":["memory hook --agent claude"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(workDirectory, ".claude"))
	marker := filepath.Join(t.TempDir(), "startup-memory.json")
	t.Setenv("AMUX_MCP_PROCESS_TEST", "1")
	t.Setenv("AMUX_BUNDLED_DIR", binDirectory)
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE", "1")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_MARKER", marker)
	const recalled = "durable memory fixture\none\n two\r\nthree"
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDOUT", recalled)
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDERR", "")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_EXIT", "0")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_WAIT", "")
	t.Setenv("GATEWAY_API_KEY", "fixture-key")
	if err := os.Unsetenv("AMUX_DESKTOP_GUIDANCE"); err != nil {
		t.Fatal(err)
	}
	projectName := filepath.Base(workDirectory)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		"",
	}, "\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "mcp", "--server", "https://gateway.example", "--project", "native-project",
		"--conversation", "018f0000-0000-7000-8000-000000000001", "--session", "018f0000-0000-7000-8000-000000000002",
		"--instance", "018f0000-0000-7000-8000-000000000003", "--provider", "claude", "--os", "linux", "--directory", workDirectory)
	command.Env = os.Environ()
	command.Stdin = strings.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("native MCP initialize: %v stderr=%s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("MCP responses=%d, want initialize/list: %s", len(lines), output)
	}
	var initialized struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initialized); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[Amux session guidance]", "amux memory_search", "memory_update", recalled} {
		if !strings.Contains(initialized.Result.Instructions, want) {
			t.Fatalf("initialize instructions missing %q: %q", want, initialized.Result.Instructions)
		}
	}
	if strings.Contains(initialized.Result.Instructions, "code_symbols") || strings.Contains(lines[1], `"name":"code_symbols"`) {
		t.Fatalf("memory-only install advertised code capability: instructions=%q tools=%s", initialized.Result.Instructions, lines[1])
	}
	if strings.Contains(initialized.Result.Instructions, "WorkingContext") {
		t.Fatal("startup guidance advertised shared handoff context")
	}
	if !strings.Contains(lines[1], `"name":"memory_search"`) || !strings.Contains(lines[1], `"name":"memory_update"`) {
		t.Fatalf("memory-only install omitted memory tools: %s", lines[1])
	}
	record := readBundledRunnerFixtureRecord(t, marker)
	if !reflect.DeepEqual(record.Args, []string{"search", "--limit=5", "--", projectName + " project decisions and conventions"}) {
		t.Fatalf("startup memory argv=%q", record.Args)
	}
	canonicalDirectory, err := filepath.EvalSymlinks(workDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if record.Directory != canonicalDirectory || !record.StdinEOF || record.MemoryNoUpdate != "1" || record.ToolsNoUpdate != "1" {
		t.Fatalf("startup memory process context=%+v", record)
	}
}

func TestMCPRejectsPartialNativeConfigurationAndInvalidExplicitDirectory(t *testing.T) {
	for _, args := range [][]string{{"--server", "https://gateway.example"}, {"--goal", "release"}} {
		var output bytes.Buffer
		err := runMCP(context.Background(), args, io.NopCloser(strings.NewReader("")), &output, &bytes.Buffer{}, func(string) string { return "" })
		if err == nil || !strings.Contains(err.Error(), "native MCP requires") || output.Len() != 0 {
			t.Errorf("runMCP(%q) = %v output=%s, want partial native config error before serving", args, err, output.String())
		}
	}
	t.Setenv("AMUX_AGENT_PROJECT", t.TempDir())
	missing := filepath.Join(t.TempDir(), "missing-checkout")
	var output bytes.Buffer
	err := runMCP(context.Background(), []string{"--directory", missing}, io.NopCloser(strings.NewReader("")), &output, &bytes.Buffer{}, os.Getenv)
	if err == nil || !strings.Contains(err.Error(), "existing directory") || output.Len() != 0 {
		t.Fatalf("invalid explicit directory = %v output=%s, want early error despite valid environment fallback", err, output.String())
	}
}
