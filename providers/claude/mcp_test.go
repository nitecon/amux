package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nitecon/amux/providerprocess"
)

func TestMCPConfigIsNativeAndContainsNoCredentialValue(t *testing.T) {
	secret := "credential-value-must-not-appear"
	t.Setenv("AMUX_API_KEY", secret)
	config := &providerprocess.MCPConfig{Command: "/opt/amux tools/amux", Args: []string{"mcp", "--project", "project with spaces", "--api-key-env", "AMUX_API_KEY"}, Env: map[string]string{"AMUX_DESKTOP_GUIDANCE": "provided by desktop"}}
	got := appendMCPConfig([]string{"claude"}, config)
	if len(got) != 5 || got[1] != "--settings" || got[3] != "--mcp-config" {
		t.Fatalf("Claude args = %#v", got)
	}
	if got[2] != sessionSettings {
		t.Fatalf("Claude session settings = %s", got[2])
	}
	if strings.Contains(got[4], secret) {
		t.Fatal("credential value was written to Claude MCP configuration")
	}
	var parsed struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(got[4]), &parsed); err != nil {
		t.Fatal(err)
	}
	server := parsed.Servers["amux"]
	if server.Command != config.Command || strings.Join(server.Args, "\x00") != strings.Join(config.Args, "\x00") || server.Env["AMUX_DESKTOP_GUIDANCE"] != "provided by desktop" {
		t.Fatalf("Claude MCP server config = %#v", server)
	}
}

func TestClaudeSettingsRequireConfiguredAmuxMCP(t *testing.T) {
	command := []string{"claude", "--print"}
	if got := appendMCPConfig(command, nil); strings.Join(got, "\x00") != strings.Join(command, "\x00") {
		t.Fatalf("settings were added without MCP: %#v", got)
	}
}

func TestMCPConfigForwardsOnlyConfiguredDesktopGuidance(t *testing.T) {
	config := &providerprocess.MCPConfig{
		Command: "/opt/amux", Args: []string{"mcp"}, Env: map[string]string{"AMUX_DESKTOP_GUIDANCE": "desktop text"},
	}
	args := appendMCPConfig([]string{"claude"}, config)
	var parsed struct {
		Servers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(args[4]), &parsed); err != nil {
		t.Fatal(err)
	}
	if got := parsed.Servers["amux"].Env; len(got) != 1 || got["AMUX_DESKTOP_GUIDANCE"] != "desktop text" {
		t.Fatalf("Claude MCP environment=%#v", got)
	}
}
