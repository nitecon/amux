package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPStartupInstructionsRespectCodexAndDesktopPrecedence(t *testing.T) {
	key := "AMUX_DESKTOP_GUIDANCE"
	prior, hadPrior := os.LookupEnv(key)
	defer func() {
		if hadPrior {
			_ = os.Setenv(key, prior)
		} else {
			_ = os.Unsetenv(key)
		}
	}()
	runner := bundledToolRunner{Executable: filepath.Join(t.TempDir(), "amux")}

	_ = os.Unsetenv(key)
	if got := mcpStartupInstructionsFor(context.Background(), "", "codex", runner, false); got != "" {
		t.Fatalf("standalone Codex got duplicate MCP instructions: %q", got)
	}
	if got := mcpStartupInstructionsFor(context.Background(), "claude", "codex", runner, false); !strings.Contains(got, "Finish code with amux submit-work") {
		t.Fatal("explicit native provider did not take precedence over inherited provider")
	}
	if err := os.Setenv(key, ""); err != nil {
		t.Fatal(err)
	}
	if got := mcpStartupInstructionsFor(context.Background(), "claude", "", runner, false); got != "" {
		t.Fatalf("present empty desktop guidance did not suppress MCP instructions: %q", got)
	}
}

func TestMCPStartupGuidanceIsNotSuppressedByMemoryHookConfiguration(t *testing.T) {
	project := t.TempDir()
	configDirectory := filepath.Join(project, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", configDirectory)
	if err := os.MkdirAll(configDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDirectory, "settings.json"), []byte(`{"hooks":["memory hook --agent claude"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUX_BUNDLED_DIR", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("AMUX_DESKTOP_GUIDANCE", "")
	if err := os.Unsetenv("AMUX_DESKTOP_GUIDANCE"); err != nil {
		t.Fatal(err)
	}
	got := nativeMCPStartupInstructions(context.Background(), bundledToolRunner{Directory: project, Executable: filepath.Join(project, "amux")}, false)
	if !strings.Contains(got, "[Amux session guidance]") {
		t.Fatalf("hook config suppressed Amux startup guidance: %q", got)
	}
}
