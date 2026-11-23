//go:build darwin

package desktop

import (
	"os"
	"path/filepath"
)

func loginItemPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", "com.nitecon.amux.plist"), nil
}

func launchAtLoginEnabled() bool {
	path, err := loginItemPath()
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func setLaunchAtLogin(enabled bool) error {
	path, err := loginItemPath()
	if err != nil {
		return err
	}
	if !enabled {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	return launchItemFile(path, executable, macLaunchItemContent(executable))
}
