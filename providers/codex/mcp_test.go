package codex

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nitecon/amux/providers/process"
)

func TestMCPConfigIsNativeAndContainsNoCredentialValue(t *testing.T) {
	secret := "credential-value-must-not-appear"
	t.Setenv("AMUX_API_KEY", secret)
	config := &process.MCPConfig{Command: "/opt/amux tools/amux", Args: []string{"mcp", "--project", "project with spaces", "--api-key-env", "AMUX_API_KEY"}, EnvVars: []string{"AMUX_API_KEY"}}
	got := appendMCPConfig([]string{"codex", "app-server"}, config)
	joined := strings.Join(got, " ")
	if strings.Contains(joined, secret) {
		t.Fatal("credential value was written to Codex CLI configuration")
	}
	for _, want := range []string{`mcp_servers.amux.command="/opt/amux tools/amux"`, `mcp_servers.amux.args=["mcp","--project","project with spaces","--api-key-env","AMUX_API_KEY"]`, `mcp_servers.amux.env_vars=["AMUX_API_KEY"]`, `mcp_servers.amux.default_tools_approval_mode="approve"`, `tools.update_plan.enabled=false`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Codex config %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "approval_policy=") || strings.Contains(joined, "sandbox_mode=") {
		t.Fatalf("Amux MCP settings broadened provider execution policy: %q", joined)
	}
}

func TestCodexMCPPolicySettingsRequireConfiguredAmuxServer(t *testing.T) {
	command := []string{"codex", "app-server"}
	if got := appendMCPConfig(command, nil); !reflect.DeepEqual(got, command) {
		t.Fatalf("Codex settings were added without MCP: %#v", got)
	}
}

func TestMCPConfigPreservesExplicitPolicyOverrides(t *testing.T) {
	config := &process.MCPConfig{Command: "/opt/amux", Args: []string{"mcp"}}
	forms := []struct {
		name string
		args func(string, string) []string
	}{
		{name: "split short", args: func(key, value string) []string { return []string{"-c", key + "=" + value} }},
		{name: "attached short", args: func(key, value string) []string { return []string{"-c" + key + "=" + value} }},
		{name: "split long", args: func(key, value string) []string { return []string{"--config", key + "=" + value} }},
		{name: "attached long", args: func(key, value string) []string { return []string{"--config=" + key + "=" + value} }},
	}
	policies := []struct {
		key, value string
	}{
		{key: "mcp_servers.amux.default_tools_approval_mode", value: `"deny"`},
		{key: "tools.update_plan.enabled", value: "true"},
	}
	for _, policy := range policies {
		for _, form := range forms {
			t.Run(policy.key+"/"+form.name, func(t *testing.T) {
				command := append([]string{"codex", "app-server"}, form.args(policy.key, policy.value)...)
				got := appendMCPConfig(command, config)
				joined := strings.Join(got, " ")
				if count := strings.Count(joined, policy.key+"="); count != 1 {
					t.Fatalf("explicit policy occurs %d times in %q, want exactly once", count, joined)
				}
				if !strings.Contains(joined, policy.key+"="+policy.value) {
					t.Fatalf("explicit policy override was not retained: %q", joined)
				}
				other := "tools.update_plan.enabled=false"
				if policy.key == "tools.update_plan.enabled" {
					other = `mcp_servers.amux.default_tools_approval_mode="approve"`
				}
				if !strings.Contains(joined, other) {
					t.Fatalf("unoverridden native default %q is missing: %q", other, joined)
				}
				if strings.Contains(joined, "approval_policy=") || strings.Contains(joined, "sandbox_mode=") {
					t.Fatalf("MCP config broadened provider execution policy: %q", joined)
				}
			})
		}
	}
}

func TestMCPConfigEscapesControlCharactersAsTOMLStrings(t *testing.T) {
	got := appendMCPConfig([]string{"codex", "app-server"}, &process.MCPConfig{
		Command: "/tmp/amux\x01binary", Args: []string{"mcp", "--project", "line\nbreak"},
	})
	joined := strings.Join(got, " ")
	if strings.Contains(joined, `\x01`) || !strings.Contains(joined, `\u0001`) || !strings.Contains(joined, `line\nbreak`) {
		t.Fatalf("unsafe TOML escaping: %q", joined)
	}
}

func TestDeveloperInstructionsPreserveExplicitCommandLineOverride(t *testing.T) {
	for _, command := range [][]string{
		{"codex", "app-server", "-c", "developer_instructions='user value'"},
		{"codex", "app-server", "-cdeveloper_instructions='user value'"},
		{"codex", "app-server", "--config", "developer_instructions='user value'"},
		{"codex", "app-server", "--config=developer_instructions='user value'"},
	} {
		got := appendDeveloperInstructions(command, "amux value")
		if !reflect.DeepEqual(got, command) {
			t.Fatalf("user config override changed: got=%q want=%q", got, command)
		}
	}
	got := appendDeveloperInstructions([]string{"codex", "app-server"}, "amux value")
	if len(got) != 4 || got[2] != "--config" || !strings.HasPrefix(got[3], "developer_instructions=") {
		t.Fatalf("missing Amux developer instruction config: %q", got)
	}
}
