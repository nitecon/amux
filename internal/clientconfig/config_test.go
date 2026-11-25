package clientconfig

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecode(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "enrollment.json")
	checkout := filepath.Join(root, "checkout")
	document := fmt.Sprintf("enrolled_profile: %q\nprojects_directory: %q\nprojects:\n  sample: %q\nproviders: [codex, antigravity]\n", profile, root, checkout)
	config, err := Decode(strings.NewReader(document))
	if err != nil {
		t.Fatal(err)
	}
	if config.EnrolledProfile != profile || config.ProjectsDirectory != root || config.Projects["sample"] != checkout || len(config.Providers) != 2 {
		t.Fatalf("unexpected configuration: %+v", config)
	}
	for _, invalid := range []string{
		document + "typo: true\n",
		document + "---\n",
		strings.Replace(document, "[codex, antigravity]", "[codex, codex]", 1),
		strings.Replace(document, "[codex, antigravity]", "[unknown]", 1),
		strings.Replace(document, fmt.Sprintf("%q", checkout), "relative", 1),
	} {
		if _, err := Decode(strings.NewReader(invalid)); err == nil {
			t.Errorf("accepted invalid configuration: %s", invalid)
		}
	}
}

func TestSaveRejectsInvalidReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".amux", "config.yml")
	config := Config{EnrolledProfile: filepath.Join(root, "enrollment.json"), Providers: []string{"codex"}}
	if err := Save(path, config); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, Config{}); err == nil {
		t.Fatal("saved invalid configuration")
	}
	config, err := Load(path)
	if err != nil || len(config.Providers) != 1 || config.Providers[0] != "codex" {
		t.Fatalf("invalid replacement changed configuration: %+v, %v", config, err)
	}
	config.Providers = []string{"claude"}
	if err := Save(path, config); err != nil {
		t.Fatal(err)
	}
	config, err = Load(path)
	if err != nil || len(config.Providers) != 1 || config.Providers[0] != "claude" {
		t.Fatalf("valid replacement was not saved: %+v, %v", config, err)
	}
}
