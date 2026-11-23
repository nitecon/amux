//go:build !linux && !darwin && !windows

package desktop

import "fmt"

func launchAtLoginEnabled() bool { return false }

func setLaunchAtLogin(bool) error {
	return fmt.Errorf("launch at sign in is not supported on this platform")
}
