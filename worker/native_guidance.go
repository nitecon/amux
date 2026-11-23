package worker

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nitecon/amux/bundledtools"
	"github.com/nitecon/amux/providers/acp"
	"github.com/nitecon/amux/startupguidance"
)

func nativeMCPEnvironment() map[string]string {
	value := os.Getenv("AMUX_DESKTOP_GUIDANCE")
	if value == "" {
		return nil
	}
	return map[string]string{"AMUX_DESKTOP_GUIDANCE": value}
}

func nativeACPEnvironment() []acp.EnvVar {
	env := nativeMCPEnvironment()
	if len(env) == 0 {
		return nil
	}
	return []acp.EnvVar{{Name: "AMUX_DESKTOP_GUIDANCE", Value: env["AMUX_DESKTOP_GUIDANCE"]}}
}

func nativeCodexDeveloperInstructions(project string) string {
	if _, present := os.LookupEnv("AMUX_DESKTOP_GUIDANCE"); present || codexConfigHasDeveloperInstructions(project) {
		return ""
	}
	query, ok := startupguidance.MemoryQuery(project)
	if !ok {
		return ""
	}
	_, codeTools := bundledtools.Path("", os.Getenv("AMUX_BUNDLED_DIR"), "agent-tools")
	_, memoryTools := bundledtools.Path("", os.Getenv("AMUX_BUNDLED_DIR"), "memory")
	memories := bundledtools.DurableMemories(project, query)
	return startupguidance.Context(codeTools, memoryTools, memories)
}

func codexConfigHasDeveloperInstructions(project string) bool {
	paths := []string{}
	configHome := os.Getenv("CODEX_HOME")
	if configHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			configHome = filepath.Join(home, ".codex")
		}
	}
	if configHome != "" {
		paths = append(paths, filepath.Join(configHome, "config.toml"))
	}
	for directory := project; directory != ""; directory = filepath.Dir(directory) {
		paths = append(paths, filepath.Join(directory, ".codex", "config.toml"))
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
	}
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(contents), "developer_instructions") {
			return true
		}
	}
	return false
}
