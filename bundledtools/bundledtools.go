// Package bundledtools resolves and runs binaries shipped beside amux.
package bundledtools

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	MemoryNoUpdate = "AGENT_MEMORY_NO_UPDATE=1"
	ToolsNoUpdate  = "AGENT_TOOLS_NO_UPDATE=1"
	MemoryLimit    = 10_000
	MemoryTimeout  = 5 * time.Second
)

// Path resolves a shipped binary from the explicit override or beside amux.
func Path(executable, overrideDirectory, binary string) (string, bool) {
	directory := overrideDirectory
	if directory == "" {
		if executable == "" {
			var err error
			executable, err = os.Executable()
			if err != nil {
				return "", false
			}
		}
		directory = filepath.Dir(executable)
	}
	path, err := filepath.Abs(filepath.Join(directory, FileName(binary)))
	if err != nil {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return path, true
}

// FileName returns the platform filename of a bundled executable.
func FileName(binary string) string {
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(binary), ".exe") {
		return binary + ".exe"
	}
	return binary
}

// Environment preserves the caller's environment while disabling bundled self-updaters.
func Environment() []string {
	environment := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "AGENT_MEMORY_NO_UPDATE" || name == "AGENT_TOOLS_NO_UPDATE" {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, MemoryNoUpdate, ToolsNoUpdate)
}

// ConfigureProcess applies platform-specific settings to a bundled child.
func ConfigureProcess(command *exec.Cmd) { configureProcess(command) }

// DurableMemories returns bounded durable recall, or an empty string on failure.
// The child writes to a temporary file, so output beyond the read limit cannot
// block the process and is never accumulated in memory.
func DurableMemories(project, query string) string {
	return DurableMemoriesContext(context.Background(), project, query)
}

// DurableMemoriesContext stops recall when the caller is canceled.
func DurableMemoriesContext(ctx context.Context, project, query string) string {
	path, ok := Path("", os.Getenv("AMUX_BUNDLED_DIR"), "memory")
	if !ok {
		return ""
	}
	output, err := os.CreateTemp("", "amux-memory-recall-*")
	if err != nil {
		return ""
	}
	defer func() {
		name := output.Name()
		_ = output.Close()
		_ = os.Remove(name)
	}()

	ctx, cancel := context.WithTimeout(ctx, MemoryTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, path, "search", "--limit=5", "--", query)
	command.Dir = project
	command.Env = Environment()
	command.Stdout = output
	command.Stderr = io.Discard
	ConfigureProcess(command)
	if err := command.Run(); err != nil {
		return ""
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	bytes, err := io.ReadAll(io.LimitReader(output, MemoryLimit))
	if err != nil {
		return ""
	}
	text := strings.ToValidUTF8(string(bytes), "\uFFFD")
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return text
}
