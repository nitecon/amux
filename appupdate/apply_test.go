package appupdate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	rename := func(source, destination string) error {
		if filepath.Dir(source) == stage && filepath.Base(source) == "amux-desktop" {
			return errors.New("injected install failure")
		}
		return os.Rename(source, destination)
	}
	err := installFiles(installed, stage, []string{"amux", "amux-desktop"}, func() error { return nil }, rename)
	if err == nil || !strings.Contains(err.Error(), "injected install failure") || strings.Contains(err.Error(), "rollback also failed") {
		t.Fatalf("install error = %v", err)
	}
	for name, want := range map[string]string{"amux": "old cli", "amux-desktop": "old gui"} {
		got, readErr := os.ReadFile(filepath.Join(installed, name))
		if readErr != nil || string(got) != want {
			t.Fatalf("restored %s = %q, err=%v", name, got, readErr)
		}
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
