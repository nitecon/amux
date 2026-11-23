//go:build linux

package desktop

import (
	"os"
	"path/filepath"
)

func loginItemPath() (string, error) {
	config, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(config, "autostart", "amux.desktop"), nil
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
	return launchItemFile(path, executable, linuxLaunchItemContent(executable))
}
