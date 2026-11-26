package appupdate

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckInstallableRejectsFilesTheHelperCannotReplace(t *testing.T) {
	_, cli, gui, _, err := target()
	if err != nil || runtime.GOOS == "darwin" {
		t.Skip("the fixture uses a flat release")
	}
	dir := t.TempDir()
	for _, name := range []string{cli, gui} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckInstallable(filepath.Join(dir, cli)); err != nil {
		t.Fatalf("replaceable install rejected: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "models.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CheckInstallable(filepath.Join(dir, cli)); err == nil || !strings.Contains(err.Error(), "models.json") {
		t.Fatalf("irregular installed file error = %v", err)
	}
}

func TestCheckInstallableExplainsUnwritableInstallDirectory(t *testing.T) {
	_, cli, gui, _, err := target()
	if err != nil || runtime.GOOS == "darwin" || runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a flat release in a directory the user cannot write")
	}
	dir := t.TempDir()
	for _, name := range []string{cli, gui} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	check := func(name string) {
		err := CheckInstallable(filepath.Join(dir, cli))
		if err == nil || !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "chown") || !strings.Contains(err.Error(), "move amux") {
			t.Fatalf("%s: unwritable install error = %v", name, err)
		}
	}
	check("no lock")
	// A lock left by an earlier update still opens, but staging cannot be created.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".amux-update.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	check("existing lock")
}

func TestTakeFailureReportsAFailedInstallOnce(t *testing.T) {
	cache := t.TempDir()
	for tag, message := range map[string]string{"v1.0.0": "update completed\n", "v1.0.1": "cannot install amux: access denied\n"} {
		stage := filepath.Join(cache, "amux", "updates", tag)
		if err := os.MkdirAll(stage, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, "install.log"), []byte(message), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := takeFailureAt(cache); got != "cannot install amux: access denied" {
		t.Fatalf("failure = %q", got)
	}
	if got := takeFailureAt(cache); got != "" {
		t.Fatalf("failure reported twice: %q", got)
	}
}

func TestInstallFilesFailureRestoresPreviousFiles(t *testing.T) {
	root := t.TempDir()
	installed := filepath.Join(root, "install")
	stage := filepath.Join(root, "stage")
	if err := os.Mkdir(installed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"amux": "old cli", "amux-desktop": "old gui"} {
		if err := os.WriteFile(filepath.Join(installed, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, name), []byte("new "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stage, "BUILD.json"), []byte("new metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	rename := func(source, destination string) error {
		if filepath.Dir(source) == stage && filepath.Base(source) == "amux-desktop" {
			return errors.New("injected install failure")
		}
		return os.Rename(source, destination)
	}
	err := installFiles(installed, stage, []string{"amux", "BUILD.json", "amux-desktop"}, func() error { return nil }, rename)
	if err == nil || !strings.Contains(err.Error(), "injected install failure") || strings.Contains(err.Error(), "rollback also failed") {
		t.Fatalf("install error = %v", err)
	}
	for name, want := range map[string]string{"amux": "old cli", "amux-desktop": "old gui"} {
		got, readErr := os.ReadFile(filepath.Join(installed, name))
		if readErr != nil || string(got) != want {
			t.Fatalf("restored %s = %q, err=%v", name, got, readErr)
		}
	}
	if _, err := os.Stat(filepath.Join(installed, "BUILD.json")); !os.IsNotExist(err) {
		t.Fatalf("rollback retained previously missing metadata: %v", err)
	}
}

func TestInstallFilesRetainsBackupWhenRollbackFails(t *testing.T) {
	root := t.TempDir()
	installed := filepath.Join(root, "install")
	stage := filepath.Join(root, "stage")
	_ = os.Mkdir(installed, 0o700)
	_ = os.Mkdir(stage, 0o700)
	for _, name := range []string{"amux", "amux-desktop"} {
		_ = os.WriteFile(filepath.Join(installed, name), []byte("old "+name), 0o600)
		_ = os.WriteFile(filepath.Join(stage, name), []byte("new "+name), 0o600)
	}
	rename := func(source, destination string) error {
		if filepath.Dir(source) == stage && filepath.Base(source) == "amux-desktop" {
			return errors.New("injected install failure")
		}
		if strings.Contains(filepath.Dir(source), ".amux-update-backup-") {
			return errors.New("injected rollback failure")
		}
		return os.Rename(source, destination)
	}
	err := installFiles(installed, stage, []string{"amux", "amux-desktop"}, func() error { return nil }, rename)
	if err == nil || !strings.Contains(err.Error(), "rollback also failed") {
		t.Fatalf("install error = %v", err)
	}
	entries, readErr := os.ReadDir(installed)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var backup string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".amux-update-backup-") {
			backup = filepath.Join(installed, entry.Name())
		}
	}
	if backup == "" {
		t.Fatal("backup directory was removed after rollback failure")
	}
	for _, name := range []string{"amux", "amux-desktop"} {
		if _, err := os.Stat(filepath.Join(backup, name)); err != nil {
			t.Fatalf("previous %s was not retained in backup: %v", name, err)
		}
	}
}
