//go:build darwin

package mac

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/nitecon/amux/update/platform"
)

type Updater struct {
	platform.Flat
	platform.UnixProcess
}

func (Updater) AppBundle(binaryDir string) string {
	if filepath.Base(binaryDir) != "MacOS" {
		return ""
	}
	contents := filepath.Dir(binaryDir)
	app := filepath.Dir(contents)
	if filepath.Base(contents) != "Contents" || filepath.Ext(app) != ".app" {
		return ""
	}
	return app
}

func (Updater) VerifyApp(app, cli, gui string) error {
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
		return platform.RollbackFailure(backupDir, nil, fmt.Errorf("cannot stage old app: %w", err))
	}
	if err := os.Rename(staged, installed); err != nil {
		return platform.RollbackFailure(backupDir, os.Rename(backupApp, installed), fmt.Errorf("cannot install app: %w", err))
	}
	if err := os.RemoveAll(backupDir); err != nil {
		return fmt.Errorf("update installed, but previous app remains in %s: %w", backupDir, err)
	}
	return nil
}

func (Updater) ArchiveBinaryDir(root string) string {
	return filepath.Join(root, "amux.app", "Contents", "MacOS")
}

func (Updater) MetadataDir(app string) string {
	return filepath.Join(app, "Contents", "Resources")
}

func (Updater) CheckDestination(dir, app string) error {
	if app == "" {
		if _, err := os.Lstat(filepath.Join(dir, "amux.app")); !os.IsNotExist(err) {
			return errors.New("an app already exists at the amux.app update destination")
		}
	}
	return nil
}

func (Updater) InstallBundle(dir, app, stage string) error {
	staged := filepath.Join(stage, "amux.app")
	if app != "" {
		return replaceApp(app, staged)
	}
	return installNewMacApp(staged, filepath.Join(dir, "amux.app"))
}

func (u Updater) Relaunch(executable string, args []string) error {
	bundle := u.AppBundle(filepath.Dir(executable))
	if bundle == "" {
		return u.Flat.Relaunch(executable, args)
	}
	openArgs := []string{"-n", bundle}
	if len(args) > 0 {
		openArgs = append(openArgs, "--args")
		openArgs = append(openArgs, args...)
	}
	return exec.Command("/usr/bin/open", openArgs...).Run()
}

func (u Updater) CheckInstall(dir string) error {
	if os.Getenv("APP_SANDBOX_CONTAINER_ID") != "" {
		return errors.New("this installation is managed by the Mac App Store")
	}
	if app := u.AppBundle(dir); app != "" {
		for _, name := range []string{"_MASReceipt", "embedded.provisionprofile"} {
			if _, err := os.Lstat(filepath.Join(app, "Contents", name)); !os.IsNotExist(err) {
				return errors.New("this installation is managed by the Mac App Store")
			}
		}
	}
	return nil
}

func installNewMacApp(staged, destination string) error {
	// Reserve a new app directory so an existing application is never replaced.
	if err := os.Mkdir(destination, 0o755); err != nil {
		return fmt.Errorf("could not create the amux app destination: %w", err)
	}
	if err := os.Rename(filepath.Join(staged, "Contents"), filepath.Join(destination, "Contents")); err != nil {
		return errors.Join(err, os.Remove(destination))
	}
	return nil
}
