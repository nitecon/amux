package amuxruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/nitecon/amux/config"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worksubmission"
)

func TestSubmitWorkMCPFiltersBlankPathsAndSubmitsLiteralNames(t *testing.T) {
	f := newSubmissionEndToEnd(t, nil)
	for name, contents := range map[string]string{"space name.txt": "spaced change\n", "-dash.txt": "dash change\n"} {
		if err := os.WriteFile(filepath.Join(f.root, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	request, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "submit_work", "arguments": map[string]any{
			"task_id": f.task.ID, "message": "- Fix it",
			"paths": []string{"space name.txt", "", " \t ", "-dash.txt"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		string(request),
		"",
	}, "\n")
	args := f.mcpArgs(t, f.preferences)
	var stdout, stderr bytes.Buffer
	if err := runMCP(t, context.Background(), args, io.NopCloser(strings.NewReader(input)), &stdout, &stderr, submissionFixtureEnv); err != nil {
		t.Fatalf("native MCP submission: %v %s", err, stderr.String())
	}
	var response struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()[bytes.IndexByte(stdout.Bytes(), '\n')+1:]), &response); err != nil {
		t.Fatalf("decode MCP submit response: %v: %s", err, stdout.String())
	}
	if response.Result.IsError || len(response.Result.Content) != 1 {
		t.Fatalf("MCP submit failed: %+v", response.Result)
	}
	var receipt worksubmission.Receipt
	if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &receipt); err != nil {
		t.Fatal(err)
	}
	want := []string{"space name.txt", "-dash.txt"}
	if !reflect.DeepEqual(receipt.Files, want) {
		t.Fatalf("submitted paths=%q, want %q", receipt.Files, want)
	}
	for _, name := range want {
		if got := submissionFixtureGit(t, f.remote, "show", "main:"+name); got == "" {
			t.Fatalf("submitted path %q has no published content", name)
		}
	}
	message := submissionFixtureGit(t, f.remote, "log", "-1", "--format=%B", "main")
	if !strings.HasPrefix(message, "- Fix it\n") {
		t.Fatalf("leading-dash MCP message changed in published commit: %q", message)
	}
}

func TestSubmitWorkMCPForwardsResumeCommit(t *testing.T) {
	f := newSubmissionEndToEnd(t, nil)
	localHead := submissionFixtureGit(t, f.root, "rev-parse", "HEAD")
	remoteHead := submissionFixtureGit(t, f.remote, "rev-parse", "refs/heads/main")
	request, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "submit_work", "arguments": map[string]any{
			"task_id": f.task.ID, "message": "Resume existing task commit",
			"paths": []string{"a.txt"}, "resume_commit": localHead,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		string(request),
		"",
	}, "\n")
	args := f.mcpArgs(t, f.preferences)
	var stdout, stderr bytes.Buffer
	if err := runMCP(t, context.Background(), args, io.NopCloser(strings.NewReader(input)), &stdout, &stderr, submissionFixtureEnv); err != nil {
		t.Fatalf("native MCP resume request: %v %s", err, stderr.String())
	}
	lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("MCP response count=%d: %s", len(lines), stdout.String())
	}
	var response struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(lines[1], &response); err != nil {
		t.Fatal(err)
	}
	if !response.Result.IsError || len(response.Result.Content) != 1 ||
		!strings.Contains(response.Result.Content[0].Text, "resume commit must have exactly one matching AmuxID trailer") {
		t.Fatalf("resume_commit did not reach resume validation: %s", lines[1])
	}
	if got := submissionFixtureGit(t, f.root, "rev-parse", "HEAD"); got != localHead {
		t.Fatalf("local HEAD changed from %s to %s", localHead, got)
	}
	if got := submissionFixtureGit(t, f.remote, "rev-parse", "refs/heads/main"); got != remoteHead {
		t.Fatalf("remote HEAD changed from %s to %s", remoteHead, got)
	}
}

func TestSubmitWorkMCPRejectsInvalidPathShapesBeforeHTTPOrGit(t *testing.T) {
	var requests atomic.Int64
	f := newSubmissionEndToEnd(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			next.ServeHTTP(w, r)
		})
	})
	trace := filepath.Join(t.TempDir(), "git-trace")
	t.Setenv("GIT_TRACE", trace)
	for _, pathsJSON := range []string{`[null]`, `[1]`, `[]`, `["", " \t "]`, `null`, `"a.rs"`} {
		t.Run(pathsJSON, func(t *testing.T) {
			args := json.RawMessage(`{"task_id":"` + f.task.ID + `","message":"submit","paths":` + pathsJSON + `,"resume_commit":"abc123"}`)
			result, err := callSubmitWorkMCP(t, f, "", args)
			if err != nil || !result.IsError || len(result.Content) != 1 {
				t.Fatalf("invalid paths response=%+v err=%v", result, err)
			}
			if strings.Contains(result.Content[0].Text, "unknown submit_work argument") {
				t.Fatalf("resume_commit was not accepted: %s", result.Content[0].Text)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("invalid MCP path shapes made %d gateway requests", got)
	}
	if _, err := os.Stat(trace); err == nil {
		data, readErr := os.ReadFile(trace)
		if readErr != nil || len(data) != 0 {
			t.Fatalf("invalid MCP path shapes invoked Git: %s %v", data, readErr)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func callSubmitWorkMCP(t *testing.T, f submissionEndToEnd, preferences string, args json.RawMessage) (storage.ToolResult, error) {
	t.Helper()
	var result storage.ToolResult
	var out bytes.Buffer
	flags := f.mcpArgs(t, preferences)
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"submit_work","arguments":` + string(args) + "}}\n"
	if err := runMCP(t, context.Background(), flags, io.NopCloser(strings.NewReader(input)), &out, io.Discard, submissionFixtureEnv); err != nil {
		return result, err
	}
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		return result, fmt.Errorf("expected two MCP responses, got %s", out.String())
	}
	var reply struct {
		Result storage.ToolResult `json:"result"`
	}
	err := json.Unmarshal(lines[1], &reply)
	return reply.Result, err
}

func (f submissionEndToEnd) mcpArgs(t *testing.T, preferences string) []string {
	t.Helper()
	writeMCPTestSettings(t, config.Client{GatewayEndpoint: f.client.Endpoint(), WorkPreferences: preferences})
	return []string{"--project", "demo", "--conversation", f.conversation, "--directory", f.root,
		"--session", f.origin.SessionID, "--instance", f.origin.InstanceID, "--provider", f.origin.Provider, "--os", f.origin.OS}
}
