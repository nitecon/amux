package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/providers"
	"github.com/nitecon/amux/providers/process"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worksubmission"
)

// NativeToolsConfig identifies the amux MCP CLI and the credential variable
// that the child MCP process may inherit.
type NativeToolsConfig struct {
	Executable, Server, APIKeyEnv string
	EnrolledProfile               string
	WorkPreferencesPath           string
	LocalConversation             bool
}

// OpenWithNativeTools returns an opener that registers one scoped amux MCP
// process for each saved worker run.
func OpenWithNativeTools(config NativeToolsConfig) Open {
	preferencesPath, preferencesErr := nativeWorkPreferencesPath(config.WorkPreferencesPath)
	config.WorkPreferencesPath = preferencesPath
	open := func(ctx context.Context, run storage.WorkerRun, sink conversation.Sink) (providers.Session, error) {
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
		if config.Server == "" || (!config.LocalConversation && config.Server != run.Server) || (config.LocalConversation && run.Server != "" && config.Server != run.Server) {
			return nil, fmt.Errorf("amux MCP server must match the saved worker gateway")
		}
		if config.LocalConversation {
			if run.Server == "" {
				run.Server = config.Server
			}
			if run.Project == "" || run.ConversationID == "" || run.Owner.SessionID == "" || run.Owner.InstanceID == "" || run.Owner.Provider == "" || run.Owner.OS == "" {
				return nil, fmt.Errorf("local conversation MCP requires project, conversation ID, and complete agent identity")
			}
		}
		if config.EnrolledProfile == "" && !validEnvName(config.APIKeyEnv) {
			return nil, fmt.Errorf("amux MCP API key environment name must be valid")
		}
		if config.EnrolledProfile == "" {
			if _, ok := os.LookupEnv(config.APIKeyEnv); !ok {
				return nil, fmt.Errorf("amux MCP API key environment variable %q is not set", config.APIKeyEnv)
			}
		}
		args := nativeMCPArgs(run, config.LocalConversation)
		env := nativeProviderEnv(os.Environ(), run, config.APIKeyEnv, config.EnrolledProfile != "")
		mcpEnv := nativeMCPEnvironment()
		if mcpEnv == nil {
			mcpEnv = make(map[string]string, 1)
		}
		mcpEnv[process.ManagedMCPEnv] = "1"
		provider, ok := providers.Lookup(run.Owner.Provider)
		if !ok {
			return nil, fmt.Errorf("provider %q is not implemented", run.Owner.Provider)
		}
		mcp := &process.MCPConfig{
			Command: config.Executable, Args: args, Env: mcpEnv, EnvVars: nativeMCPEnvVars(nativeMCPAPIKeyEnv(config)),
		}
		session, err := provider.Start(ctx, providers.StartOptions{
			Command: run.Command, Directory: run.Directory, ResumeID: run.NativeSessionID, Env: env, MCP: mcp,
		}, sink)
		if err != nil {
			return nil, err
		}
		identity, _ := json.Marshal(agent.Current(run.Owner))
		return &identitySession{Session: session, identity: string(identity)}, nil
	}
	return OpenWithToolApprovals(open, config.WorkPreferencesPath)
}

// toolApprovals answers permission requests that match a rule the user saved
// with "Always allow", and forwards every other request to the user.
type toolApprovals struct {
	path      string
	directory string
	session   atomic.Pointer[providers.Session]
}

func (a *toolApprovals) sink(next conversation.Sink) conversation.Sink {
	return func(ctx context.Context, event conversation.Event) error {
		if event.Kind == "permission" && event.RequestID != "" {
			if session := a.session.Load(); session != nil {
				directory := worksubmission.ToolApprovalDirectory(event, a.directory)
				rules, err := worksubmission.LoadDirectoryToolApprovals(a.path, directory)
				if option := worksubmission.ToolApprovalOption(rules, event); err == nil && option != "" &&
					(*session).ResolvePermission(ctx, event.RequestID, option) == nil {
					return nil
				}
			}
		}
		return next(ctx, event)
	}
}

// OpenWithToolApprovals applies saved global and exact-directory approval
// rules to any provider opener, including standalone local conversations.
func OpenWithToolApprovals(open Open, preferencesPath string) Open {
	path, pathErr := nativeWorkPreferencesPath(preferencesPath)
	return func(ctx context.Context, run storage.WorkerRun, sink conversation.Sink) (providers.Session, error) {
		if pathErr != nil {
			return nil, pathErr
		}
		approvals := &toolApprovals{path: path, directory: run.Directory}
		session, err := open(ctx, run, approvals.sink(sink))
		if err != nil {
			return nil, err
		}
		approvals.session.Store(&session)
		return session, nil
	}
}

type identitySession struct {
	providers.Session
	identity string
}

// Attach current host facts to the first assignment or reminder handled by this provider process.
func (s *identitySession) Prompt(ctx context.Context, text string) (string, error) {
	if s.identity != "" && (strings.HasPrefix(text, storage.AssignmentMCPIntro) ||
		strings.HasPrefix(text, "Please evaluate this task and perform it") ||
		strings.HasPrefix(text, "[Amux delegated completion inspection]") ||
		strings.HasPrefix(text, "[amux notification]")) {
		text = "[Current Amux worker identity]\n" + s.identity +
			"\nUse these values when reporting your identity. The amux MCP agent_identity tool returns the same facts.\n\n" + text
		s.identity = ""
	}
	return s.Session.Prompt(ctx, text)
}

func nativeMCPEnvVars(apiKeyEnv string) []string {
	names := []string{
		"AMUX_AGENT_SESSION_ID", "AMUX_AGENT_INSTANCE_ID", "AMUX_AGENT_PROVIDER", "AMUX_AGENT_OS",
		"AMUX_AGENT_PROJECT", "AMUX_GATEWAY_PROJECT", "AMUX_GATEWAY_URL", "AMUX_GATEWAY_API_KEY_ENV",
		"AMUX_BUNDLED_DIR", "AMUX_DESKTOP_GUIDANCE", "HOME", "USERPROFILE",
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

func nativeMCPArgs(run storage.WorkerRun, localConversation bool) []string {
	goal := ""
	if run.Goal != nil {
		goal = *run.Goal
	}
	args := []string{"--project", run.Project,
		"--directory", run.Directory, "--conversation", run.ConversationID,
		"--session", run.Owner.SessionID, "--instance", run.Owner.InstanceID,
		"--provider", run.Owner.Provider, "--os", run.Owner.OS}
	if localConversation {
		args = append(args, "--local-conversation")
	}
	if run.LocalTaskID != "" {
		args = append(args, "--local-task", run.LocalTaskID)
	}
	return append(args, "--goal", goal)
}
