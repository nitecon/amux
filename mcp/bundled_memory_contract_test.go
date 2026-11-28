package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/amux/bundledtools"
	_ "modernc.org/sqlite"
)

func TestPinnedBundledMemoryUpdateRetainsTombstoneThroughMCP(t *testing.T) {
	directory := pinnedBundleContractDirectory(t)
	root, conn, memoryDirectory := isolatedPinnedMemoryContract(t, directory)
	defer conn.Close()
	id := "aaaaaaaa-0000-1111-2222-000000000901"
	insertPinnedTombstone(t, conn, id, "amux")

	request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_update","arguments":{"id":"` + id + `","content":"replacement lesson","tags":"transactions"}}}`
	output := runPinnedBundledMCPStdio(t, directory, root, []string{
		"AGENT_MEMORY_DIR=" + memoryDirectory,
		"AGENT_MEMORY_GATEWAY_AUTO_SYNC=false",
	},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		request,
		"",
	)
	lines := bytes.Split(bytes.TrimSpace(output), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("pinned memory_update responses=%d, want initialize and one tool response: %s", len(lines), output)
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
	if !response.Result.IsError || len(response.Result.Content) != 1 {
		t.Fatalf("memory_update response=%s, want one MCP tool error", lines[1])
	}
	text := response.Result.Content[0].Text
	if !strings.Contains(text, "tombstoned gateway identity") || !strings.Contains(text, "memory copy --id") || strings.Contains(text, `status="updated"`) {
		t.Fatalf("memory_update recovery response=%q", text)
	}
	assertPinnedTombstoneRetained(t, conn, id)
}

func TestPinnedBundledMemoryPushAllContinuesPastTombstones(t *testing.T) {
	directory := pinnedBundleContractDirectory(t)
	root, conn, memoryDirectory := isolatedPinnedMemoryContract(t, directory)
	defer conn.Close()
	insertPinnedTombstone(t, conn, "aaaaaaaa-0000-1111-2222-000000000901", "a-project")
	insertPinnedTombstone(t, conn, "aaaaaaaa-0000-1111-2222-000000000902", "z-project")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, filepath.Join(directory, bundledtools.FileName("memory")), "push", "--all")
	command.Dir = root
	command.Env = append(pinnedBundleContractEnvironment(t.TempDir()),
		"AGENT_MEMORY_DIR="+memoryDirectory,
		"AGENT_MEMORY_GATEWAY_AUTO_SYNC=false",
		"AGENT_MEMORY_GATEWAY_URL=http://127.0.0.1:9",
		"AGENT_MEMORY_GATEWAY_API_KEY=isolated-test-key",
		"AMUX_BUNDLED_DIR="+directory,
	)
	output, err := command.CombinedOutput()
	text := string(output)
	if err != nil {
		t.Fatalf("pinned memory push --all: %v: %s", err, text)
	}
	for _, want := range []string{"push_all_complete", `projects="2"`, `blocked="2"`, `rejected="0"`} {
		if !strings.Contains(text, want) {
			t.Errorf("push --all output omits %q: %s", want, text)
		}
	}
	if strings.Contains(text, "memory copy --id") {
		t.Errorf("push --all requested tombstone recovery: %s", text)
	}
	var retained int
	if err := conn.QueryRow("SELECT COUNT(*) FROM memory_gateway_sync WHERE tombstone_deleted = 1").Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 2 {
		t.Fatalf("retained tombstones=%d, want 2", retained)
	}
}

func isolatedPinnedMemoryContract(t *testing.T, directory string) (string, *sql.DB, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	memoryDirectory := filepath.Join(root, "memory")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, filepath.Join(directory, bundledtools.FileName("memory")), "list", "--limit", "1")
	command.Dir = root
	command.Env = append(pinnedBundleContractEnvironment(home),
		"AGENT_MEMORY_DIR="+memoryDirectory,
		"AGENT_MEMORY_GATEWAY_AUTO_SYNC=false",
		"AMUX_BUNDLED_DIR="+directory,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("initialize isolated pinned memory database: %v: %s", err, output)
	}
	db, err := sql.Open("sqlite", filepath.Join(memoryDirectory, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		t.Fatal(err)
	}
	return root, db, memoryDirectory
}

func insertPinnedTombstone(t *testing.T, db *sql.DB, id, project string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO memories (id, content, content_raw, tags, project,
		created_at, updated_at, memory_type)
		VALUES (?, 'retained lesson', 'original lesson', '["undo"]', ?,
		'2026-01-01', '2026-01-01', 'project')`, id, project)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO memory_gateway_sync (local_memory_id, project,
		gateway_memory_id, last_seen_server_revision, sync_state,
		tombstone_deleted, created_at, updated_at)
		VALUES (?, ?, ?, 10, 'tombstone', 1, '2026-01-01', '2026-01-01')`, id, project, id)
	if err != nil {
		t.Fatal(err)
	}
}

func assertPinnedTombstoneRetained(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	var content, raw, tags, updated string
	var tombstone bool
	err := db.QueryRow(`SELECT m.content, m.content_raw, m.tags, m.updated_at,
		s.tombstone_deleted FROM memories m JOIN memory_gateway_sync s
		ON s.local_memory_id = m.id WHERE m.id = ?`, id).
		Scan(&content, &raw, &tags, &updated, &tombstone)
	if err != nil {
		t.Fatal(err)
	}
	if content != "retained lesson" || raw != "original lesson" || tags != `["undo"]` || updated != "2026-01-01" || !tombstone {
		t.Fatalf("retained row=(%q, %q, %q, %q, %v)", content, raw, tags, updated, tombstone)
	}
}
