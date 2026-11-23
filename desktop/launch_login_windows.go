//go:build windows

package desktop

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func launchAtLoginEnabled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	_, _, err = key.GetStringValue("Amux")
	return err == nil
}

func setLaunchAtLogin(enabled bool) error {
	if !enabled {
		key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
		if err != nil {
			if err == registry.ErrNotExist {
				return nil
			}
			return err
		}
		defer key.Close()
		err = key.DeleteValue("Amux")
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := key.SetStringValue("Amux", fmt.Sprintf("\"%s\"", executable)); err != nil {
		return err
	}
	return nil
}
