package config

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
	config, err := DecodeClient(strings.NewReader(document))
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
		if _, err := DecodeClient(strings.NewReader(invalid)); err == nil {
			t.Errorf("accepted invalid configuration: %s", invalid)
		}
	}
}

func TestSaveRejectsInvalidReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".amux", "config.yml")
	config := Client{EnrolledProfile: filepath.Join(root, "enrollment.json"), Providers: []string{"codex"}}
	if err := SaveClient(path, config); err != nil {
		t.Fatal(err)
	}
	if err := SaveClient(path, Client{}); err == nil {
		t.Fatal("saved invalid configuration")
	}
	config, err := ReadClient(path)
	if err != nil || len(config.Providers) != 1 || config.Providers[0] != "codex" {
		t.Fatalf("invalid replacement changed configuration: %+v, %v", config, err)
	}
	config.Providers = []string{"claude"}
	if err := SaveClient(path, config); err != nil {
		t.Fatal(err)
	}
	config, err = ReadClient(path)
	if err != nil || len(config.Providers) != 1 || config.Providers[0] != "claude" {
		t.Fatalf("valid replacement was not saved: %+v, %v", config, err)
	}
}
