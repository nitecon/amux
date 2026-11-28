package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/nitecon/amux/bundledtools"
	"github.com/nitecon/amux/storage"
)

type bundledToolRunner struct {
	Executable, OverrideDirectory, Directory string
}

func (r bundledToolRunner) AvailableTools() []storage.TaskTool {
	available := make([]storage.TaskTool, 0, len(bundledToolCatalog))
	for _, spec := range bundledToolCatalog {
		if _, ok := r.binaryPath(spec.Binary); ok {
			available = append(available, bundledToolDefinition(spec))
		}
	}
	return available
}

func (r bundledToolRunner) Call(ctx context.Context, name string, raw json.RawMessage) (storage.ToolResult, error) {
	var spec *bundledToolSpec
	for i := range bundledToolCatalog {
		if bundledToolCatalog[i].Name == name {
			spec = &bundledToolCatalog[i]
			break
		}
	}
	if spec == nil {
		return bundledToolError("unknown tool: " + name), nil
	}
	path, ok := r.binaryPath(spec.Binary)
	if !ok {
		return bundledToolError("unknown tool: " + name), nil
	}
	argv, err := bundledToolArgv(*spec, raw)
	if err != nil {
		return bundledToolError(err.Error()), nil
	}
	command := exec.CommandContext(ctx, path, argv...)
	command.Dir = r.workingDirectory()
	command.Env = bundledToolEnvironment()
	bundledtools.ConfigureProcess(command)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.Output()
	if err == nil {
		return storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: bundledToolText(stdout)}}}, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return bundledToolError(bundledToolText(stdout) + bundledToolText(stderr.Bytes())), nil
	}
	return bundledToolError(fmt.Sprintf("could not run %s: %v", bundledToolFileName(spec.Binary), err)), nil
}

func (r bundledToolRunner) binaryPath(binary string) (string, bool) {
	return bundledtools.Path(r.Executable, r.OverrideDirectory, binary)
}

func bundledToolFileName(binary string) string {
	return bundledtools.FileName(binary)
}

func (r bundledToolRunner) workingDirectory() string {
	if r.Directory != "" {
		return r.Directory
	}
	if project := os.Getenv("AMUX_AGENT_PROJECT"); project != "" {
		if info, err := os.Stat(project); err == nil && info.IsDir() {
			return project
		}
	}
	if current, err := os.Getwd(); err == nil {
		return current
	}
	return "."
}

func bundledToolEnvironment() []string {
	return bundledtools.Environment()
}

func bundledToolError(text string) storage.ToolResult {
	return storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: text}}, IsError: true}
}

func bundledToolText(output []byte) string {
	return strings.ToValidUTF8(string(output), "\uFFFD")
}
