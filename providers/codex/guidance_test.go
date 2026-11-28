package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexDeveloperInstructionsPreserveUserAndAncestorConfiguration(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "repo", "nested")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	configHome := filepath.Join(root, "codex-home")
	t.Setenv("CODEX_HOME", configHome)
	t.Setenv("AMUX_BUNDLED_DIR", filepath.Join(root, "no-bundles"))
	t.Setenv("AMUX_DESKTOP_GUIDANCE", "")
	if got := DeveloperInstructions(project); got != "" {
		t.Fatalf("present desktop guidance should suppress launch injection: %q", got)
	}
	if err := os.Unsetenv("AMUX_DESKTOP_GUIDANCE"); err != nil {
		t.Fatal(err)
	}
	if got := DeveloperInstructions(project); !strings.Contains(got, "[Amux session guidance]") {
		t.Fatalf("missing default Amux launch guidance: %q", got)
	}

	if err := os.MkdirAll(configHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "config.toml"), []byte("[profiles.work]\ndeveloper_instructions = 'keep mine'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := DeveloperInstructions(project); got != "" {
		t.Fatalf("user profile developer instructions were replaced: %q", got)
	}
	if err := os.Remove(filepath.Join(configHome, "config.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "config.toml"), []byte("developer_instructions = 'preserve direct user instructions'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := DeveloperInstructions(project); got != "" {
		t.Fatalf("direct user developer instructions were replaced: %q", got)
	}
	if err := os.Remove(filepath.Join(configHome, "config.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configHome, 0700); err != nil {
		t.Fatal(err)
	}
	hookConfig := filepath.Join(configHome, "config.toml")
	if err := os.WriteFile(hookConfig, []byte("[[hooks.UserPromptSubmit]]\ncommand = 'memory hook --agent codex'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := DeveloperInstructions(project); !strings.Contains(got, "[Amux session guidance]") {
		t.Fatalf("memory hook config suppressed source-required Codex startup context: %q", got)
	}
	if err := os.Remove(hookConfig); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "repo", ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "repo", ".codex", "config.toml"), []byte("developer_instructions = 'project instruction'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := DeveloperInstructions(project); got != "" {
		t.Fatalf("ancestor project developer instructions were replaced: %q", got)
	}
}
