package bundledtools_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/amux/bundledtools"
)

func TestDurableMemoriesChildProcessContract(t *testing.T) {
	if testing.Short() {
		t.Skip("uses the real five-second bundled-memory deadline")
	}
	root := installRecallFixture(t)
	project := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AMUX_BUNDLED_DIR", root)
	t.Setenv("AMUX_RECALL_FIXTURE", "1")
	t.Setenv("AMUX_RECALL_FIXTURE_RECORD", filepath.Join(t.TempDir(), "child.json"))
	t.Setenv("AGENT_MEMORY_NO_UPDATE", "0")
	t.Setenv("AGENT_TOOLS_NO_UPDATE", "0")

	t.Run("argv cwd stdin and no-update environment", func(t *testing.T) {
		t.Setenv("AMUX_RECALL_FIXTURE_MODE", "normal")
		const query = "-literal query; $(never run)"
		if got := bundledtools.DurableMemories(project, query); got != "durable result\n" {
			t.Fatalf("DurableMemories = %q", got)
		}
		record := readRecallFixtureRecord(t)
		if want := []string{"search", "--limit=5", "--", query}; !reflect.DeepEqual(record.Args, want) {
			t.Fatalf("child argv = %#v, want %#v", record.Args, want)
		}
		wantDirectory, err := filepath.EvalSymlinks(project)
		if err != nil {
			t.Fatal(err)
		}
		if record.Directory != wantDirectory {
			t.Fatalf("child cwd = %q, want %q", record.Directory, wantDirectory)
		}
		if record.Stdin != "" {
			t.Fatalf("child stdin = %q, want EOF", record.Stdin)
		}
		if !reflect.DeepEqual(record.MemoryNoUpdate, []string{"1"}) ||
			!reflect.DeepEqual(record.ToolsNoUpdate, []string{"1"}) {
			t.Fatalf("child updater environment = memory %#v tools %#v", record.MemoryNoUpdate, record.ToolsNoUpdate)
		}
	})

	t.Run("full output through byte limit", func(t *testing.T) {
		t.Setenv("AMUX_RECALL_FIXTURE_MODE", "large")
		got := bundledtools.DurableMemories(project, "large")
		if len(got) != 10_000 || got != strings.Repeat("x", 10_000) {
			t.Fatalf("output length/content = %d/%q, want 10000 x bytes", len(got), got[:min(len(got), 32)])
		}
	})

	t.Run("truncated UTF-8 is repaired at byte limit", func(t *testing.T) {
		t.Setenv("AMUX_RECALL_FIXTURE_MODE", "utf8-edge")
		got := bundledtools.DurableMemories(project, "utf8")
		want := strings.Repeat("a", 9_999) + "\uFFFD"
		if got != want {
			t.Fatalf("UTF-8 edge output length/content = %d/%q, want repaired 10002-byte string", len(got), got[len(got)-min(len(got), 16):])
		}
	})

	t.Run("failure and blank output return empty", func(t *testing.T) {
		for _, mode := range []string{"fail", "blank"} {
			t.Run(mode, func(t *testing.T) {
				t.Setenv("AMUX_RECALL_FIXTURE_MODE", mode)
				if got := bundledtools.DurableMemories(project, "query"); got != "" {
					t.Fatalf("DurableMemories = %q, want empty", got)
				}
			})
		}
	})

	t.Run("missing binary returns empty", func(t *testing.T) {
		t.Setenv("AMUX_BUNDLED_DIR", t.TempDir())
		if got := bundledtools.DurableMemories(project, "query"); got != "" {
			t.Fatalf("DurableMemories = %q, want empty", got)
		}
	})

	t.Run("real timeout kills and waits for child", func(t *testing.T) {
		t.Setenv("AMUX_BUNDLED_DIR", root)
		t.Setenv("AMUX_RECALL_FIXTURE_MODE", "sleep")
		started := time.Now()
		if got := bundledtools.DurableMemories(project, "slow query"); got != "" {
			t.Fatalf("DurableMemories = %q, want empty after timeout", got)
		}
		elapsed := time.Since(started)
		if elapsed < 4*time.Second || elapsed > 8*time.Second {
			t.Fatalf("timeout returned after %s, want the real five-second deadline and prompt child reap", elapsed)
		}
	})
}

func readRecallFixtureRecord(t *testing.T) recallFixtureRecord {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("AMUX_RECALL_FIXTURE_RECORD"))
	if err != nil {
		t.Fatal(err)
	}
	var record recallFixtureRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}
