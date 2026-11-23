package bundledtools_test

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recallFixtureRecord struct {
	Args           []string `json:"args"`
	Directory      string   `json:"directory"`
	Stdin          string   `json:"stdin"`
	MemoryNoUpdate []string `json:"memory_no_update"`
	ToolsNoUpdate  []string `json:"tools_no_update"`
}

// TestMain doubles as a portable executable fixture when the test binary is
// copied to the bundled memory filename and launched by DurableMemories.
func TestMain(m *testing.M) {
	basename := strings.ToLower(filepath.Base(os.Args[0]))
	if os.Getenv("AMUX_RECALL_FIXTURE") == "1" &&
		(basename == "memory" || basename == "memory.exe") {
		os.Exit(runRecallFixture())
	}
	os.Exit(m.Run())
}

func runRecallFixture() int {
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 91
	}
	directory, err := os.Getwd()
	if err != nil {
		return 92
	}
	record := recallFixtureRecord{
		Args:      append([]string(nil), os.Args[1:]...),
		Directory: directory,
		Stdin:     string(stdin),
	}
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch name {
		case "AGENT_MEMORY_NO_UPDATE":
			record.MemoryNoUpdate = append(record.MemoryNoUpdate, value)
		case "AGENT_TOOLS_NO_UPDATE":
			record.ToolsNoUpdate = append(record.ToolsNoUpdate, value)
		}
	}
	if path := os.Getenv("AMUX_RECALL_FIXTURE_RECORD"); path != "" {
		encoded, err := json.Marshal(record)
		if err != nil || os.WriteFile(path, encoded, 0o600) != nil {
			return 93
		}
	}

	switch os.Getenv("AMUX_RECALL_FIXTURE_MODE") {
	case "fail":
		_, _ = io.WriteString(os.Stdout, "partial result before failure\n")
		return 17
	case "blank":
		_, _ = io.WriteString(os.Stdout, " \t\r\n ")
		return 0
	case "sleep":
		time.Sleep(30 * time.Second)
	case "utf8-edge":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("a", 9_999)+"éafter-limit")
	case "large":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", 10_001))
	default:
		_, _ = io.WriteString(os.Stdout, "durable result\n")
	}
	return 0
}

func installRecallFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bin := "memory"
	if filepath.Ext(os.Args[0]) == ".exe" {
		bin += ".exe"
	}
	path := filepath.Join(root, bin)
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}
