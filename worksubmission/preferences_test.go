package worksubmission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestPreferencesPathUsesTrimmedOverrideThenEnvironment(t *testing.T) {
	override := filepath.Join(t.TempDir(), "override.json")
	configured := filepath.Join(t.TempDir(), "environment.json")
	got, err := PreferencesPath(" "+override+" ", configured)
	want, _ := filepath.Abs(override)
	if err != nil || got != want {
		t.Fatalf("explicit preference path = %q, %v; want %q", got, err, want)
	}
	got, err = PreferencesPath("", " "+configured+" ")
	want, _ = filepath.Abs(configured)
	if err != nil || got != want {
		t.Fatalf("environment preference path = %q, %v; want %q", got, err, want)
	}
}

func TestOptionalPreferencesPathKeepsExplicitPinsAndAllowsHeadlessDefault(t *testing.T) {
	clearUserConfigEnvironment(t)
	configured := filepath.Join(t.TempDir(), "environment.json")
	got, err := OptionalPreferencesPath(" \t ", " "+configured+" ")
	want, absErr := filepath.Abs(configured)
	if err != nil || absErr != nil || got != want {
		t.Fatalf("explicit environment path = %q, %v; want %q", got, err, want)
	}
	override := filepath.Join(t.TempDir(), "override.json")
	got, err = OptionalPreferencesPath(override, configured)
	want, absErr = filepath.Abs(override)
	if err != nil || absErr != nil || got != want {
		t.Fatalf("explicit override path = %q, %v; want %q", got, err, want)
	}
	got, err = OptionalPreferencesPath(" \t ", " \t ")
	if err != nil || got != "" {
		t.Fatalf("unconfigured headless preferences = %q, %v; want empty path without error", got, err)
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

func TestSharedPreferencesDefaultsNestedFieldsAndPreservesOtherRootData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "work.json")
	if got, err := LoadPreferences(path); err != nil || got != DefaultPreferences() {
		t.Fatalf("missing document = %+v, %v", got, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := `{"mode":"client","enrolled":{"node_id":"saved"},"work_submission":{}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadPreferences(path); err != nil || got != DefaultPreferences() {
		t.Fatalf("omitted nested values = %+v, %v", got, err)
	}
	want := Preferences{Mode: PullRequestMode, BranchPrefix: "review/"}
	if err := SavePreferences(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPreferences(path)
	if err != nil || got != want {
		t.Fatalf("saved preferences = %+v, %v; want %+v", got, err, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	var enrolled map[string]string
	if string(root["mode"]) != `"client"` || json.Unmarshal(root["enrolled"], &enrolled) != nil || enrolled["node_id"] != "saved" {
		t.Fatalf("unknown root fields were not preserved: %s", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("saved file permissions = %v, %v; want 0600", info, err)
		}
	}
}

func TestSharedPreferencesRejectNullCorruptOversizedAndUnknownFieldsWithoutOverwrite(t *testing.T) {
	for name, contents := range map[string]string{
		"nested null":   `{"work_submission":null}`,
		"mode null":     `{"work_submission":{"mode":null}}`,
		"prefix null":   `{"work_submission":{"branch_prefix":null}}`,
		"unknown field": `{"work_submission":{"extra":true}}`,
		"invalid root":  `[]`,
		"trailing":      `{} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "work.json")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadPreferences(path); err == nil {
				t.Fatal("invalid preferences were accepted")
			}
			if err := SavePreferences(path, DefaultPreferences()); err == nil {
				t.Fatal("invalid existing preferences were overwritten")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != contents {
				t.Fatalf("invalid source changed: %q, %v", data, err)
			}
		})
	}
	large := filepath.Join(t.TempDir(), "large.json")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", maxPreferencesBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPreferences(large); err == nil || !strings.Contains(err.Error(), "exceed 1 MiB") {
		t.Fatalf("oversized document error = %v", err)
	}
	if err := SavePreferences(large, DefaultPreferences()); err == nil {
		t.Fatal("oversized document was overwritten")
	}
	valid := filepath.Join(t.TempDir(), "valid.json")
	if err := SavePreferences(valid, DefaultPreferences()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(valid)
	tooLarge := DefaultPreferences()
	tooLarge.BranchPrefix = strings.Repeat("p", maxPreferencesBytes)
	if err := SavePreferences(valid, tooLarge); err == nil {
		t.Fatal("oversized write was accepted")
	}
	after, _ := os.ReadFile(valid)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("oversized write changed the existing preferences")
	}
}
