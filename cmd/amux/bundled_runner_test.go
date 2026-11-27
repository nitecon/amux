package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/amux/mcpstdio"

	"github.com/nitecon/amux/internal/testfixture"
)

func TestBundledRunnerAvailabilityUsesOnlySiblingOrOverrideFiles(t *testing.T) {
	pathDirectory := t.TempDir()
	writeBundledFixtureFile(t, filepath.Join(pathDirectory, bundledToolFileName("agent-tools")))
	t.Setenv("PATH", pathDirectory)

	executableDirectory := t.TempDir()
	runner := bundledToolRunner{Executable: filepath.Join(executableDirectory, "amux")}
	if tools := runner.AvailableTools(); len(tools) != 0 {
		t.Fatalf("PATH-only tools were exposed: %v", bundledToolNames(tools))
	}
	result, err := runner.Call(context.Background(), "code_read", json.RawMessage(`{"file":"README.md"}`))
	if err != nil || !result.IsError || !strings.Contains(result.Content[0].Text, "unknown tool: code_read") {
		t.Fatalf("missing sibling call = (%+v, %v), want MCP error", result, err)
	}

	writeBundledFixtureFile(t, filepath.Join(executableDirectory, bundledToolFileName("agent-tools")))
	if got := len(runner.AvailableTools()); got != 8 {
		t.Fatalf("sibling agent-tools exposed %d tools, want 8", got)
	}
	if err := os.Remove(filepath.Join(executableDirectory, bundledToolFileName("agent-tools"))); err != nil {
		t.Fatal(err)
	}
	if tools := runner.AvailableTools(); len(tools) != 0 {
		t.Fatalf("removed sibling remained available: %v", bundledToolNames(tools))
	}

	overrideDirectory := t.TempDir()
	writeBundledFixtureFile(t, filepath.Join(overrideDirectory, bundledToolFileName("memory")))
	runner.OverrideDirectory = overrideDirectory
	if got := len(runner.AvailableTools()); got != 4 {
		t.Fatalf("override memory exposed %d tools, want 4", got)
	}
}

func TestBundledRunnerExecutesSiblingWithOwnedContextAndLiteralArgv(t *testing.T) {
	directory := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	copyBundledTestBinary(t, executable, filepath.Join(directory, bundledToolFileName("agent-tools")))
	workDirectory := t.TempDir()
	runner := bundledToolRunner{Executable: filepath.Join(directory, "amux"), Directory: workDirectory}

	t.Setenv("AGENT_MEMORY_NO_UPDATE", "0")
	t.Setenv("AGENT_TOOLS_NO_UPDATE", "0")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE", "1")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_KEEP", "inherited")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDOUT", "runner output λ")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDERR", "ignored success stderr")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDERR_SIZE", "")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_EXIT", "0")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_WAIT", "")

	marker := filepath.Join(t.TempDir(), "marker.json")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_MARKER", marker)
	file := "-literal $(touch should-not-exist) λ.txt"
	result, err := runner.Call(context.Background(), "code_read", json.RawMessage(`{"file":"-literal $(touch should-not-exist) λ.txt","lines":"2:4"}`))
	if err != nil || result.IsError || len(result.Content) != 1 || result.Content[0].Text != "runner output λ" {
		t.Fatalf("bundled success = (%+v, %v)", result, err)
	}
	record := readBundledRunnerFixtureRecord(t, marker)
	wantArgs := []string{"read", "--lines=2:4", "--", file}
	if !reflect.DeepEqual(record.Args, wantArgs) {
		t.Fatalf("child argv=%q, want %q", record.Args, wantArgs)
	}
	canonicalWorkDirectory, err := filepath.EvalSymlinks(workDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if !testfixture.SameDirectory(record.Directory, canonicalWorkDirectory) || record.MemoryNoUpdate != "1" || record.ToolsNoUpdate != "1" || record.Inherited != "inherited" || !record.StdinEOF {
		t.Fatalf("child process context=%+v", record)
	}
	if _, err := os.Stat(filepath.Join(workDirectory, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("literal argument executed as shell: stat error=%v", err)
	}

	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDOUT", "stdout")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDERR", "")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDERR_SIZE", "131072")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_EXIT", "9")
	failed, err := runner.Call(context.Background(), "code_read", json.RawMessage(`{"file":"README.md"}`))
	wantFailureText := "stdout" + strings.Repeat("e", 131072) + "END"
	if err != nil || !failed.IsError || len(failed.Content) != 1 || failed.Content[0].Text != wantFailureText {
		t.Fatalf("bundled failure = (%+v, %v), want stdout+stderr error", failed, err)
	}

	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_WAIT", "1")
	cancelMarker := filepath.Join(t.TempDir(), "cancel-marker.json")
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_MARKER", cancelMarker)
	t.Setenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDERR_SIZE", "")
	// Cancel only once the child has written its marker, so slow process
	// startup under parallel load cannot race a fixed cancellation deadline.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callDone := make(chan struct{})
	canceledAt := make(chan time.Time, 1)
	go func() {
		defer cancel()
		deadline := time.After(30 * time.Second)
		for {
			if _, err := os.Stat(cancelMarker); err == nil {
				canceledAt <- time.Now()
				return
			}
			select {
			case <-callDone:
				return
			case <-deadline:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	canceled, err := runner.Call(ctx, "code_read", json.RawMessage(`{"file":"README.md"}`))
	returned := time.Now()
	close(callDone)
	var cancelTime time.Time
	select {
	case cancelTime = <-canceledAt:
	default:
		t.Fatalf("cancellation fixture did not start: result=%+v err=%v", canceled, err)
	}
	if elapsed := returned.Sub(cancelTime); err != nil || !canceled.IsError || elapsed > 2*time.Second {
		t.Fatalf("canceled child result=%+v err=%v elapsed after cancel=%s", canceled, err, elapsed)
	}
}

type bundledRunnerFixtureRecord struct {
	Args           []string `json:"args"`
	Directory      string   `json:"directory"`
	MemoryNoUpdate string   `json:"memory_no_update"`
	ToolsNoUpdate  string   `json:"tools_no_update"`
	Inherited      string   `json:"inherited"`
	StdinEOF       bool     `json:"stdin_eof"`
}

// TestMain dispatches this only for copied `agent-tools` or `memory` test
// binaries with AMUX_BUNDLED_RUNNER_FIXTURE=1. The marker JSON contract is
// {args,directory,memory_no_update,tools_no_update,inherited,stdin_eof}; the
// *_STDOUT, *_STDERR, *_STDERR_SIZE, *_EXIT and *_WAIT control the fixture result.
func runBundledRunnerFixtureProcess() int {
	stdin, _ := io.ReadAll(os.Stdin)
	directory, _ := os.Getwd()
	record := bundledRunnerFixtureRecord{
		Args: os.Args[1:], Directory: directory,
		MemoryNoUpdate: os.Getenv("AGENT_MEMORY_NO_UPDATE"),
		ToolsNoUpdate:  os.Getenv("AGENT_TOOLS_NO_UPDATE"),
		Inherited:      os.Getenv("AMUX_BUNDLED_RUNNER_FIXTURE_KEEP"),
		StdinEOF:       len(stdin) == 0,
	}
	if marker := os.Getenv("AMUX_BUNDLED_RUNNER_FIXTURE_MARKER"); marker != "" {
		encoded, err := json.Marshal(record)
		if err != nil {
			return 90
		}
		if err := os.WriteFile(marker, encoded, 0600); err != nil {
			return 91
		}
	}
	_, _ = io.WriteString(os.Stdout, os.Getenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDOUT"))
	stderr := os.Getenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDERR")
	if count, err := strconv.Atoi(os.Getenv("AMUX_BUNDLED_RUNNER_FIXTURE_STDERR_SIZE")); err == nil && count > 0 {
		stderr += strings.Repeat("e", count) + "END"
	}
	_, _ = io.WriteString(os.Stderr, stderr)
	if os.Getenv("AMUX_BUNDLED_RUNNER_FIXTURE_WAIT") == "1" {
		// Sleep instead of select{}: the runtime aborts an empty select as a
		// deadlock when no other goroutine exists, which fakes a fast exit.
		time.Sleep(time.Hour)
	}
	code, err := strconv.Atoi(os.Getenv("AMUX_BUNDLED_RUNNER_FIXTURE_EXIT"))
	if err != nil {
		return 0
	}
	return code
}

func bundledToolNames(tools []mcpstdio.Tool) []string {
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	return names
}

func writeBundledFixtureFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
}

func copyBundledTestBinary(t *testing.T, source, target string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0700); err != nil {
		t.Fatal(err)
	}
}

func readBundledRunnerFixtureRecord(t *testing.T, path string) bundledRunnerFixtureRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record bundledRunnerFixtureRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestBundledToolFileNameUsesWindowsExecutableSuffix(t *testing.T) {
	name := bundledToolFileName("agent-tools")
	if runtime.GOOS == "windows" && name != "agent-tools.exe" {
		t.Fatalf("Windows bundled filename=%q", name)
	}
	if runtime.GOOS != "windows" && name != "agent-tools" {
		t.Fatalf("non-Windows bundled filename=%q", name)
	}
}
