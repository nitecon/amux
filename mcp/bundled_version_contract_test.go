package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nitecon/amux/bundledtools"
	"github.com/nitecon/amux/config"
	"github.com/nitecon/amux/storage"
)

// This opt-in check uses the checksum-verified release artifacts fetched by
// scripts/fetch_bundled_tools.py. Synthetic runners cannot prove CLI version
// compatibility, and ordinary test runs must not execute installed tools.
func TestPinnedBundledToolCommandsAndOptionsExist(t *testing.T) {
	directory := pinnedBundleContractDirectory(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	home := t.TempDir()
	for _, tool := range bundledToolCatalog {
		t.Run(tool.Name, func(t *testing.T) {
			binary := bundledtools.FileName(tool.Binary)
			command := exec.CommandContext(ctx, filepath.Join(directory, binary), append(append([]string(nil), tool.Command...), "--help")...)
			command.Dir = home
			command.Env = pinnedBundleContractEnvironment(home)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("pinned %s %v --help failed: %v: %s", binary, tool.Command, err, output)
			}
			words := strings.FieldsFunc(string(output), func(r rune) bool {
				return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t'
			})
			for _, argument := range tool.Args {
				if argument.Kind != bundledOption {
					continue
				}
				found := false
				for _, word := range words {
					if word == argument.Flag {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("pinned %s %v help omits %s", binary, tool.Command, argument.Flag)
				}
			}
		})
	}
}

func pinnedBundleContractDirectory(t *testing.T) string {
	t.Helper()
	directory := os.Getenv("AMUX_BUNDLED_CONTRACT_DIR")
	if directory == "" {
		t.Skip("set AMUX_BUNDLED_CONTRACT_DIR to the verified release-artifact directory")
	}
	if !filepath.IsAbs(directory) {
		workingDirectory, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		repositoryRoot, err := filepath.Abs(filepath.Join(workingDirectory, ".."))
		if err != nil {
			t.Fatal(err)
		}
		directory = filepath.Join(repositoryRoot, directory)
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		t.Fatalf("pinned bundle directory %q is unavailable", directory)
	}
	return directory
}

func pinnedBundleContractEnvironment(home string) []string {
	return []string{
		// These checks exercise pinned tool commands, not startup memory recall.
		"AMUX_DESKTOP_GUIDANCE=1",
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "USERPROFILE=" + home,
		"XDG_CONFIG_HOME=" + home, "XDG_DATA_HOME=" + home, "APPDATA=" + home,
		"AGENT_MEMORY_NO_UPDATE=1", "AGENT_TOOLS_NO_UPDATE=1", "NO_COLOR=1",
	}
}

func TestPinnedBundledMCPReadsGoModuleOverRealStdio(t *testing.T) {
	directory := pinnedBundleContractDirectory(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join(cwd, ".."))
	if err != nil {
		t.Fatal(err)
	}
	output := runPinnedBundledMCPStdio(t, directory, root, nil, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"code_read","arguments":{"file":"go.mod","lines":"1:2"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"code_read","arguments":{}}}`,
		"",
	}, "\n"))
	lines := bytes.Split(bytes.TrimSpace(output), []byte("\n"))
	if len(lines) != 4 {
		t.Fatalf("expected four MCP responses, got %d: %s", len(lines), output)
	}
	var listed struct {
		Result struct {
			Tools []storage.TaskTool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(lines[1], &listed); err != nil || len(listed.Result.Tools) != len(bundledToolCatalog) {
		t.Fatalf("pinned tool listing: %s err=%v", lines[1], err)
	}
	var actualNames, expectedNames []string
	for _, tool := range listed.Result.Tools {
		actualNames = append(actualNames, tool.Name)
	}
	for _, tool := range bundledToolCatalog {
		expectedNames = append(expectedNames, tool.Name)
	}
	slices.Sort(actualNames)
	slices.Sort(expectedNames)
	if !slices.Equal(actualNames, expectedNames) {
		t.Fatalf("listed tools = %v, want %v", actualNames, expectedNames)
	}
	var read, missing struct {
		Result storage.ToolResult `json:"result"`
	}
	if err := json.Unmarshal(lines[2], &read); err != nil || read.Result.IsError || len(read.Result.Content) != 1 || !strings.Contains(read.Result.Content[0].Text, "module github.com/nitecon/amux") {
		t.Fatalf("real code_read: %s err=%v", lines[2], err)
	}
	if err := json.Unmarshal(lines[3], &missing); err != nil || !missing.Result.IsError {
		t.Fatalf("missing argument did not return a tool error: %s err=%v", lines[3], err)
	}
}

func TestPinnedBundledMCPReturnsCodeReadOutputAboveFiveMiB(t *testing.T) {
	directory := pinnedBundleContractDirectory(t)
	workDirectory := t.TempDir()
	payload := strings.Repeat("x", (5<<20)+4096)
	path := filepath.Join(workDirectory, "large.txt")
	if err := os.WriteFile(path, []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	call, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "code_read", "arguments": map[string]string{"file": path}},
	})
	if err != nil {
		t.Fatal(err)
	}
	output := runPinnedBundledMCPStdio(t, directory, workDirectory, nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		string(call),
		"",
	)
	lines := bytes.Split(bytes.TrimSpace(output), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("pinned code_read responses=%d, want initialize and one tool response", len(lines))
	}
	if len(lines[1]) <= 5<<20 {
		t.Fatalf("pinned code_read response size=%d, want above 5 MiB", len(lines[1]))
	}
	var response struct {
		ID     json.RawMessage    `json:"id"`
		Result storage.ToolResult `json:"result"`
	}
	if err := json.Unmarshal(lines[1], &response); err != nil {
		t.Fatal(err)
	}
	if string(response.ID) != "2" || response.Result.IsError || len(response.Result.Content) != 1 {
		t.Fatalf("pinned code_read response id/error/content=%s/%v/%d", response.ID, response.Result.IsError, len(response.Result.Content))
	}
	if response.Result.Content[0].Text != payload {
		t.Fatalf("pinned code_read body length=%d, want %d", len(response.Result.Content[0].Text), len(payload))
	}
}

func runPinnedBundledMCPStdio(t *testing.T, directory, workDirectory string, extraEnvironment []string, requests ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	home := t.TempDir()
	if err := config.SaveClient(filepath.Join(home, ".amux", "config.yaml"), config.Client{
		GatewayEndpoint: "https://gateway.example", APIKeyEnv: "GATEWAY_API_KEY",
	}); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, os.Args[0], "mcp", "--directory", workDirectory)
	command.Dir = home
	command.Env = append(pinnedBundleContractEnvironment(home), "AMUX_MCP_PROCESS_TEST=1", "AMUX_BUNDLED_DIR="+directory)
	command.Env = append(command.Env, extraEnvironment...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	client := sdk.NewClient(&sdk.Implementation{Name: "amux-bundle-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatalf("connect to bundled stdio process: %v stderr=%s", err, stderr.String())
	}
	defer session.Close()
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	for line := range strings.SplitSeq(strings.Join(requests, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var request struct {
			ID     json.RawMessage
			Method string
			Params struct {
				Name      string
				Arguments json.RawMessage
			}
		}
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			t.Fatal(err)
		}
		var result any
		switch request.Method {
		case "initialize":
			result = session.InitializeResult()
		case "notifications/initialized":
			continue
		case "tools/list":
			result, err = session.ListTools(ctx, nil)
		case "tools/call":
			result, err = session.CallTool(ctx, &sdk.CallToolParams{Name: request.Params.Name, Arguments: request.Params.Arguments})
		default:
			t.Fatalf("unexpected fixture method %q", request.Method)
		}
		if err != nil {
			t.Fatalf("bundled stdio request %s: %v", request.Method, err)
		}
		if err := encoder.Encode(struct {
			ID     json.RawMessage `json:"id"`
			Result any             `json:"result"`
		}{request.ID, result}); err != nil {
			t.Fatal(err)
		}
	}
	return output.Bytes()
}
