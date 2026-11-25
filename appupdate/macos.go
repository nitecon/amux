package appupdate

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func appBundle(binaryDir string) string {
	if runtime.GOOS != "darwin" || filepath.Base(binaryDir) != "MacOS" {
		return ""
	}
	contents := filepath.Dir(binaryDir)
	app := filepath.Dir(contents)
	if filepath.Base(contents) != "Contents" || filepath.Ext(app) != ".app" {
		return ""
	}
	return app
}

func verifyApp(app, cli, gui string) error {
	info, err := os.Lstat(app)
	if err != nil || !info.IsDir() {
		return errors.New("installed app must be a real directory")
	}
	args := []string{"--verify", "--strict", app}
	if output, err := exec.Command("/usr/bin/codesign", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("app code signature verification failed: %w: %s", err, output)
	}
	contents := filepath.Join(app, "Contents")
	for _, relative := range []string{
		filepath.Join("MacOS", cli),
		filepath.Join("MacOS", gui),
		filepath.Join("MacOS", "agent-tools"),
		filepath.Join("MacOS", "memory"),
		filepath.Join("Resources", "models.json"),
		filepath.Join("Resources", "BUILD.json"),
		filepath.Join("Resources", "LICENSE"),
	} {
		info, err := os.Lstat(filepath.Join(contents, relative))
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("app is missing regular file %s", relative)
		}
	}
	return nil
}

func replaceApp(installed, staged string) error {
	backupDir, err := os.MkdirTemp(filepath.Dir(installed), ".amux-update-backup-")
	if err != nil {
		return err
	}
	backupApp := filepath.Join(backupDir, filepath.Base(installed))
	if err := os.Rename(installed, backupApp); err != nil {
		return rollbackFailure(backupDir, nil, fmt.Errorf("cannot stage old app: %w", err))
	}
	if err := os.Rename(staged, installed); err != nil {
		return rollbackFailure(backupDir, os.Rename(backupApp, installed), fmt.Errorf("cannot install app: %w", err))
	}
	if err := os.RemoveAll(backupDir); err != nil {
		return fmt.Errorf("update installed, but previous app remains in %s: %w", backupDir, err)
	}
	return nil
}
