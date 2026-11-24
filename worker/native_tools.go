package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/providerprocess"
	"github.com/nitecon/amux/providers/acp"
	"github.com/nitecon/amux/providers/claude"
	"github.com/nitecon/amux/providers/codex"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worksubmission"
)

// NativeToolsConfig identifies the amux MCP CLI and the credential variable
// that the child MCP process may inherit.
type NativeToolsConfig struct {
	Executable, Server, APIKeyEnv string
	EnrolledProfile               string
	WorkPreferencesPath           string
}

// OpenWithNativeTools returns an opener that registers one scoped amux MCP
// process for each saved worker run.
func OpenWithNativeTools(config NativeToolsConfig) Open {
	preferencesPath, preferencesErr := nativeWorkPreferencesPath(config.WorkPreferencesPath)
	config.WorkPreferencesPath = preferencesPath
	return func(ctx context.Context, run storage.WorkerRun, sink conversation.Sink) (Session, error) {
		if preferencesErr != nil {
			return nil, preferencesErr
		}
		if !filepath.IsAbs(config.Executable) {
			return nil, fmt.Errorf("amux MCP executable must be an absolute path")
		}
		info, err := os.Stat(config.Executable)
		if err != nil || info.IsDir() || runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0 {
			return nil, fmt.Errorf("amux MCP executable must be an existing executable file")
		}
		if config.Server == "" || config.Server != run.Server {
			return nil, fmt.Errorf("amux MCP server must match the saved worker gateway")
		}
		if config.EnrolledProfile == "" && !validEnvName(config.APIKeyEnv) {
			return nil, fmt.Errorf("amux MCP API key environment name must be valid")
		}
		if config.EnrolledProfile == "" {
			if _, ok := os.LookupEnv(config.APIKeyEnv); !ok {
				return nil, fmt.Errorf("amux MCP API key environment variable %q is not set", config.APIKeyEnv)
			}
		}
		args := nativeMCPArgs(config, run)
		env := nativeProviderEnv(os.Environ(), run, config.APIKeyEnv, config.EnrolledProfile != "")
		mcp := providerprocess.MCPConfig{Command: config.Executable, Args: args, Env: nativeMCPEnvironment()}
		switch run.Owner.Provider {
		case "antigravity":
			return acp.Start(ctx, acp.Config{Directory: run.Directory, ResumeID: run.NativeSessionID,
				Command: run.Command, Env: env,
				MCPServers: []acp.MCPServer{{Name: "amux", Command: config.Executable, Args: args, Env: nativeACPEnvironment()}}}, sink)
		case "codex":
			mcp.EnvVars = nativeMCPEnvVars(nativeMCPAPIKeyEnv(config))
			return codex.Start(ctx, codex.Config{Directory: run.Directory, ResumeID: run.NativeSessionID,
				Command: run.Command, Env: env, MCP: &mcp,
				DeveloperInstructions: nativeCodexDeveloperInstructions(run.Directory)}, sink)
		case "claude":
			return claude.Start(ctx, claude.Config{Directory: run.Directory, ResumeID: run.NativeSessionID,
				Command: run.Command, Env: env, MCP: &mcp}, sink)
		default:
			return nil, fmt.Errorf("provider %q is not implemented", run.Owner.Provider)
		}
	}
}

func nativeMCPEnvVars(apiKeyEnv string) []string {
	names := []string{
		"AMUX_AGENT_SESSION_ID", "AMUX_AGENT_INSTANCE_ID", "AMUX_AGENT_PROVIDER", "AMUX_AGENT_OS",
		"AMUX_AGENT_PROJECT", "AMUX_GATEWAY_PROJECT", "AMUX_GATEWAY_URL", "AMUX_GATEWAY_API_KEY_ENV",
		"AMUX_BUNDLED_DIR", "AMUX_DESKTOP_GUIDANCE",
		"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST",
		"GIT_SSH", "GIT_SSH_COMMAND", "GIT_SSH_VARIANT", "GIT_ASKPASS", "SSH_AUTH_SOCK",
	}
	result := make([]string, 0, len(names)+1)
	seen := make(map[string]struct{}, len(names)+1)
	for _, name := range names {
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	if apiKeyEnv != "" {
		if _, exists := seen[apiKeyEnv]; !exists {
			result = append(result, apiKeyEnv)
		}
	}
	return result
}

func nativeProviderEnv(inherited []string, run storage.WorkerRun, apiKeyEnv string, enrolled bool) []string {
	identity := [][2]string{
		{"AMUX_AGENT_SESSION_ID", run.Owner.SessionID},
		{"AMUX_AGENT_INSTANCE_ID", run.Owner.InstanceID},
		{"AMUX_AGENT_PROVIDER", run.Owner.Provider},
		{"AMUX_AGENT_OS", run.Owner.OS},
		{"AMUX_AGENT_PROJECT", run.Directory},
		{"AMUX_GATEWAY_PROJECT", run.Project},
		{"AMUX_GATEWAY_URL", run.Server},
	}
	if apiKeyEnv != "" && !enrolled {
		identity = append(identity, [2]string{"AMUX_GATEWAY_API_KEY_ENV", apiKeyEnv})
	}
	overridden := make(map[string]struct{}, len(identity))
	for _, entry := range identity {
		overridden[entry[0]] = struct{}{}
	}
	if enrolled {
		overridden["AMUX_GATEWAY_API_KEY_ENV"] = struct{}{}
		overridden["GATEWAY_API_KEY"] = struct{}{}
		overridden["AMUX_GATEWAY_KEY"] = struct{}{}
	}
	result := make([]string, 0, len(inherited)+len(identity))
	for _, entry := range inherited {
		name, _, ok := strings.Cut(entry, "=")
		if enrolled && (name == apiKeyEnv || name == "GATEWAY_API_KEY" || name == "AMUX_GATEWAY_KEY") {
			continue
		}
		if _, replace := overridden[name]; ok && !replace {
			result = append(result, entry)
		}
	}
	for _, entry := range identity {
		result = append(result, entry[0]+"="+entry[1])
	}
	return result
}

func nativeMCPAPIKeyEnv(config NativeToolsConfig) string {
	if config.EnrolledProfile != "" {
		return ""
	}
	return config.APIKeyEnv
}

func nativeWorkPreferencesPath(configured string) (string, error) {
	return worksubmission.OptionalPreferencesPath(configured, os.Getenv("AMUX_WORK_SUBMISSION_PREFERENCES"))
}

func validEnvName(name string) bool {
	if name == "" || !(name[0] == '_' || name[0] >= 'A' && name[0] <= 'Z' || name[0] >= 'a' && name[0] <= 'z') {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if c != '_' && !(c >= 'A' && c <= 'Z') && !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func nativeMCPArgs(config NativeToolsConfig, run storage.WorkerRun) []string {
	goal := ""
	if run.Goal != nil {
		goal = *run.Goal
	}
	args := []string{"mcp", "--server", config.Server, "--project", run.Project,
		"--directory", run.Directory,
		"--conversation", run.ConversationID, "--session", run.Owner.SessionID,
		"--instance", run.Owner.InstanceID, "--provider", run.Owner.Provider,
		"--os", run.Owner.OS}
	if run.LocalTaskID != "" {
		args = append(args, "--local-task", run.LocalTaskID)
	}
	if config.EnrolledProfile != "" {
		args = append(args, "--enrolled-profile", config.EnrolledProfile)
	} else {
		args = append(args, "--api-key-env", config.APIKeyEnv)
	}
	if config.WorkPreferencesPath != "" {
		args = append(args, "--work-preferences", config.WorkPreferencesPath)
	}
	return append(args, "--goal", goal)
}
