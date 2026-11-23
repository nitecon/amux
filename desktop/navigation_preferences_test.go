package desktop

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestNavigationPreferencesRoundTripAndPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	want := NavigationPreferences{
		Favorites:           map[string]bool{"favorite-key": true},
		WorkerPools:         map[string]string{"https://gateway.test": "saved-pool-id"},
		ProjectsDirectories: map[string]string{"https://gateway.test": "/work/projects"},
		ConversationLaunch:  map[string]ConversationLaunchPreferences{"gateway\x00workspace\x00one\x00provider\x00claude": {Model: "sonnet", Persona: "reviewer"}},
		Selections: map[string]NavigationSelection{
			"https://gateway.test": {
				Project: "demo", CheckoutPath: "/work/demo", CheckoutOrigin: stringPtrDesktop("https://github.com/example/demo.git"), Task: "task-1", Conversation: "conv-1",
				ProjectFilter: "api", TaskFilter: "release", Status: "in_progress", MainView: "Conversations",
			},
		},
	}
	if err := SaveNavigationPreferences(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadNavigationPreferences(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %04o, want 0600", info.Mode().Perm())
	}
}

func TestNavigationPreferencesBeforeWorkerSelectionStillLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old-prefs.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"preferences":{"favorites":{},"selections":{"https://gateway.test":{"project":"demo"}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	prefs, err := LoadNavigationPreferences(path)
	if err != nil || prefs.WorkerPools != nil || prefs.Selections["https://gateway.test"].Project != "demo" {
		t.Fatalf("old preferences changed: %+v %v", prefs, err)
	}
}

func TestLoadMissingPreferencesReturnsInitializedEmptyValue(t *testing.T) {
	got, err := LoadNavigationPreferences(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Favorites == nil || got.Selections == nil || len(got.Favorites) != 0 || len(got.Selections) != 0 {
		t.Fatalf("missing file preferences = %#v, want initialized empty maps", got)
	}
}

func TestFirstSaveCreatesNestedPreferencesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config", "prefs.json")
	if _, err := LoadNavigationPreferences(path); err != nil {
		t.Fatal(err)
	}
	want := NavigationPreferences{Favorites: map[string]bool{"nested": true}, Selections: map[string]NavigationSelection{}}
	if err := SaveNavigationPreferences(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadNavigationPreferences(path)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("nested first-save round trip = %#v, err %v", got, err)
	}
	info, err := os.Stat(filepath.Dir(filepath.Dir(path)))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("created directory mode = %04o, want 0700", info.Mode().Perm())
	}
}

func TestNavigationPreferencesRejectsCorruptOversizedAndUnknownVersionWithoutOverwrite(t *testing.T) {
	for name, data := range map[string][]byte{
		"corrupt":  []byte(`{"version":1,"preferences":`),
		"trailing": []byte(`{"version":1,"preferences":{}} {}`),
		"version":  []byte(`{"version":2,"preferences":{}}`),
		"unknown":  []byte(`{"version":1,"preferences":{},"extra":true}`),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "prefs.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadNavigationPreferences(path)
			if err == nil {
				t.Fatalf("LoadNavigationPreferences returned %#v without error", got)
			}
			if name == "version" && !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("unknown version error = %v", err)
			}
			original, _ := os.ReadFile(path)
			if !reflect.DeepEqual(original, data) {
				t.Fatal("load modified corrupt preferences")
			}
		})
	}

	path := filepath.Join(t.TempDir(), "large.json")
	if err := os.WriteFile(path, make([]byte, maxNavigationPreferencesSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadNavigationPreferences(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized load error = %v, want size error", err)
	}
}

func TestNavigationPreferencesRejectsSymlinkAndNonRegularPaths(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte(`{"version":1,"preferences":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := LoadNavigationPreferences(link); err == nil {
		t.Fatal("loaded preferences through symlink")
	}
	if err := SaveNavigationPreferences(link, NavigationPreferences{}); err == nil {
		t.Fatal("saved preferences through symlink")
	}
	if _, err := LoadNavigationPreferences(dir); err == nil {
		t.Fatal("loaded preferences from a directory")
	}
	if err := SaveNavigationPreferences(dir, NavigationPreferences{}); err == nil {
		t.Fatal("saved preferences to a directory")
	}
}

func TestSaveNavigationPreferencesCanRetryAndKeepsPreviousFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.json")
	first := NavigationPreferences{Favorites: map[string]bool{"first": true}, Selections: map[string]NavigationSelection{}}
	if err := SaveNavigationPreferences(path, first); err != nil {
		t.Fatal(err)
	}
	blockedParent := filepath.Join(dir, "file-parent")
	if err := os.WriteFile(blockedParent, []byte("obstacle"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(blockedParent, "prefs.json")
	if err := SaveNavigationPreferences(blocked, NavigationPreferences{}); err == nil {
		t.Fatal("save through a non-directory parent unexpectedly succeeded")
	}
	got, err := LoadNavigationPreferences(path)
	if err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("failed save disturbed prior file: got %#v, err %v", got, err)
	}
	second := NavigationPreferences{Favorites: map[string]bool{}, Selections: map[string]NavigationSelection{"gateway": {Project: "next"}}}
	retry := filepath.Join(dir, "new", "prefs.json")
	if err := SaveNavigationPreferences(retry, second); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	got, err = LoadNavigationPreferences(retry)
	if err != nil || !reflect.DeepEqual(got, second) {
		t.Fatalf("retry round trip = %#v, err %v", got, err)
	}
}

func TestNavigationPreferencesRejectsEmptyPathAndOversizedSave(t *testing.T) {
	if _, err := LoadNavigationPreferences(""); err == nil {
		t.Fatal("empty load path accepted")
	}
	if err := SaveNavigationPreferences("", NavigationPreferences{}); err == nil {
		t.Fatal("empty save path accepted")
	}
	prefs := NavigationPreferences{Favorites: map[string]bool{strings.Repeat("k", maxNavigationPreferencesSize): true}}
	if err := SaveNavigationPreferences(filepath.Join(t.TempDir(), "prefs.json"), prefs); err == nil {
		t.Fatal("oversized save accepted")
	}
}
