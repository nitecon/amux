package amuxruntime

import (
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

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
