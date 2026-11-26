package worker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestNativeMCPArgsUseSavedRunIdentity(t *testing.T) {
	config := NativeToolsConfig{Executable: "/opt/amux path/amux", Server: "https://gateway.example/path with space", APIKeyEnv: "AMUX_API_KEY", WorkPreferencesPath: "/saved/config/work-preferences.json"}
	goal := "release"
	run := storage.WorkerRun{Server: config.Server, Project: "project with space", Directory: "/saved checkout/worker", ConversationID: "conversation-id", Owner: storage.AgentOrigin{
		SessionID: "session-id", InstanceID: "new-instance-after-cas", Provider: "codex", OS: "darwin",
	}, Goal: &goal}
	want := []string{"mcp", "--server", config.Server, "--project", run.Project,
		"--directory", run.Directory,
		"--conversation", run.ConversationID, "--session", run.Owner.SessionID,
		"--instance", run.Owner.InstanceID, "--provider", run.Owner.Provider,
		"--os", run.Owner.OS, "--api-key-env", config.APIKeyEnv,
		"--work-preferences", config.WorkPreferencesPath, "--goal", goal}
	if got := nativeMCPArgs(config, run); !reflect.DeepEqual(got, want) {
		t.Fatalf("native MCP args = %#v, want %#v", got, want)
	}
}

func TestNativeMCPArgsPassOnlyEnrolledManifest(t *testing.T) {
	config := NativeToolsConfig{Executable: "/opt/amux", Server: "https://gateway.example", EnrolledProfile: "/private/profile/local-client.json"}
	run := storage.WorkerRun{Server: config.Server, Project: "demo", Directory: "/checkout", ConversationID: "conversation", Owner: storage.AgentOrigin{SessionID: "session", InstanceID: "instance", Provider: "codex", OS: "linux"}}
	args := nativeMCPArgs(config, run)
	for _, arg := range args {
		if arg == "--api-key-env" {
			t.Fatalf("enrolled MCP args included bearer credential configuration: %#v", args)
		}
	}
	if !containsPair(args, "--enrolled-profile", config.EnrolledProfile) {
		t.Fatalf("enrolled MCP args omitted manifest path: %#v", args)
	}
	if got := nativeMCPEnvVars(nativeMCPAPIKeyEnv(config)); len(got) != len(nativeMCPEnvVars("")) {
		t.Fatalf("enrolled MCP environment added a bearer variable: %#v", got)
	}
	for _, entry := range nativeProviderEnv([]string{"PATH=/bin", "CUSTOM_API_KEY=secret", "GATEWAY_API_KEY=default-secret", "AMUX_GATEWAY_KEY=legacy-secret"}, run, "CUSTOM_API_KEY", true) {
		if entry == "CUSTOM_API_KEY=secret" || entry == "GATEWAY_API_KEY=default-secret" || entry == "AMUX_GATEWAY_KEY=legacy-secret" {
			t.Fatalf("enrolled native provider inherited bearer credential: %#v", entry)
		}
	}
}

func containsPair(args []string, name, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestNativeWorkPreferencesPathPinsConfiguredEnvironmentPath(t *testing.T) {
	clearUserConfigEnvironment(t)
	configured := filepath.Join(t.TempDir(), "configured-work-preferences.json")
	t.Setenv("AMUX_WORK_SUBMISSION_PREFERENCES", configured)
	got, err := nativeWorkPreferencesPath("")
	want, absErr := filepath.Abs(configured)
	if err != nil || absErr != nil || got != want {
		t.Fatalf("configured preference path = %q, %v; want %q", got, err, want)
	}
	override := filepath.Join(t.TempDir(), "explicit-work-preferences.json")
	got, err = nativeWorkPreferencesPath(override)
	want, absErr = filepath.Abs(override)
	if err != nil || absErr != nil || got != want {
		t.Fatalf("explicit preference override = %q, %v; want %q", got, err, want)
	}
}

func TestNativeWorkPreferencesPathFallsBackOnlyWithoutExplicitSettings(t *testing.T) {
	clearUserConfigEnvironment(t)
	t.Setenv("AMUX_WORK_SUBMISSION_PREFERENCES", " \t ")
	got, err := nativeWorkPreferencesPath(" \t ")
	if err != nil || got != "" {
		t.Fatalf("headless default path = %q, %v; want empty path without error", got, err)
	}
}

func clearUserConfigEnvironment(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Setenv("APPDATA", "")
		t.Setenv("USERPROFILE", "")
		return
	}
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
}

func TestNativeMCPEnvVarsIncludeSavedIdentityGitAuthAndConfiguredAPIName(t *testing.T) {
	want := []string{
		"AMUX_AGENT_SESSION_ID", "AMUX_AGENT_INSTANCE_ID", "AMUX_AGENT_PROVIDER", "AMUX_AGENT_OS",
		"AMUX_AGENT_PROJECT", "AMUX_GATEWAY_PROJECT", "AMUX_GATEWAY_URL", "AMUX_GATEWAY_API_KEY_ENV",
		"AMUX_BUNDLED_DIR", "AMUX_DESKTOP_GUIDANCE",
		"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST",
		"GIT_SSH", "GIT_SSH_COMMAND", "GIT_SSH_VARIANT", "GIT_ASKPASS", "SSH_AUTH_SOCK",
		"CUSTOM_API_KEY",
	}
	if got := nativeMCPEnvVars("CUSTOM_API_KEY"); !reflect.DeepEqual(got, want) {
		t.Fatalf("MCP env allowlist = %#v, want names %#v", got, want)
	}
	withDuplicate := nativeMCPEnvVars("GH_TOKEN")
	count := 0
	for _, name := range withDuplicate {
		if name == "GH_TOKEN" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("configured API env name was not deduplicated: %#v", withDuplicate)
	}
}

func TestNativeProviderEnvUsesSavedRunIdentityWithoutDuplicateOverrides(t *testing.T) {
	run := storage.WorkerRun{
		Directory: "/saved/canonical-checkout", Project: "saved-project", Server: "https://gateway.example",
		Owner: storage.AgentOrigin{SessionID: "saved-session", InstanceID: "saved-instance", Provider: "codex", OS: "linux"},
	}
	inherited := []string{
		"PATH=/usr/bin", "AMUX_AGENT_SESSION_ID=stale-session", "CUSTOM_API_KEY=secret-value",
		"AMUX_GATEWAY_URL=https://wrong.example", "AMUX_AGENT_SESSION_ID=duplicate-session",
		"AMUX_GATEWAY_API_KEY_ENV=WRONG_KEY", "UNRELATED_SETTING=preserved",
	}
	want := []string{
		"PATH=/usr/bin", "CUSTOM_API_KEY=secret-value", "UNRELATED_SETTING=preserved",
		"AMUX_AGENT_SESSION_ID=saved-session", "AMUX_AGENT_INSTANCE_ID=saved-instance",
		"AMUX_AGENT_PROVIDER=codex", "AMUX_AGENT_OS=linux", "AMUX_AGENT_PROJECT=/saved/canonical-checkout",
		"AMUX_GATEWAY_PROJECT=saved-project", "AMUX_GATEWAY_URL=https://gateway.example",
		"AMUX_GATEWAY_API_KEY_ENV=CUSTOM_API_KEY",
	}
	if got := nativeProviderEnv(inherited, run, "CUSTOM_API_KEY", false); !reflect.DeepEqual(got, want) {
		t.Fatalf("provider environment did not deterministically override saved identity keys (got %d entries, want %d)", len(got), len(want))
	}
}

func TestNativeMCPArgsEncodeUnscopedGoalExplicitly(t *testing.T) {
	config := NativeToolsConfig{Server: "https://gateway.example", APIKeyEnv: "GATEWAY_API_KEY"}
	args := nativeMCPArgs(config, storage.WorkerRun{Server: config.Server})
	if len(args) < 2 || args[len(args)-2] != "--goal" || args[len(args)-1] != "" {
		t.Fatalf("unscoped goal was not explicit in MCP args: %#v", args)
	}
}

func TestValidEnvName(t *testing.T) {
	for _, name := range []string{"AMUX_API_KEY", "_TOKEN", "Key2"} {
		if !validEnvName(name) {
			t.Errorf("valid environment name %q rejected", name)
		}
	}
	for _, name := range []string{"", "2TOKEN", "AMUX-KEY", "AMUX=KEY", "AMUX\x00KEY"} {
		if validEnvName(name) {
			t.Errorf("invalid environment name %q accepted", name)
		}
	}
}

func TestAntigravityDefaultsApproveOnlyKnownAmuxTools(t *testing.T) {
	for name, allowed := range map[string]bool{
		"mcp_amux_task_get": true, "mcp_amux_task_done": true, "mcp_amux_memory_get": true,
		"mcp_other_task_get": false, "mcp_amux_unknown": false, "mcp_amux_task_get_more": false,
		"terminal_exec": false, "mcp_amux_": false, "": false,
	} {
		if antigravityAmuxTool(name) != allowed {
			t.Errorf("incorrect permission decision for %q", name)
		}
	}
}

func TestLocalConversationMCPArgsAndValidation(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "amux")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	config := NativeToolsConfig{
		Executable: executable, Server: "https://gateway.example",
		EnrolledProfile: "/private/profile/local-client.json", LocalConversation: true,
	}
	run := storage.WorkerRun{
		Project: "demo", Directory: t.TempDir(), ConversationID: "018f0000-0000-7000-8000-000000000001",
		Owner: storage.AgentOrigin{
			SessionID: "018f0000-0000-7000-8000-000000000002", InstanceID: "018f0000-0000-7000-8000-000000000003",
			Provider: "codex", OS: "macos",
		},
	}
	args := nativeMCPArgs(config, run)
	foundLocalFlag := false
	for _, arg := range args {
		if arg == "--local-conversation" {
			foundLocalFlag = true
			break
		}
	}
	if !foundLocalFlag {
		t.Fatalf("local conversation MCP args omitted --local-conversation: %#v", args)
	}
	open := OpenWithNativeTools(config)
	if _, err := open(context.Background(), storage.WorkerRun{Server: "https://other.example", Project: "demo", ConversationID: run.ConversationID, Owner: run.Owner}, nil); err == nil {
		t.Fatal("accepted mismatched gateway server for local conversation")
	}
	missingOwner := run
	missingOwner.Owner.SessionID = ""
	if _, err := open(context.Background(), missingOwner, nil); err == nil {
		t.Fatal("accepted incomplete agent identity for local conversation")
	}
	unsupported := run
	unsupported.Owner.Provider = "unsupported"
	if _, err := open(context.Background(), unsupported, nil); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("local conversation with empty run.Server should fill config.Server before provider dispatch; got %v", err)
	}
}
