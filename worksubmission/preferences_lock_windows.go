//go:build windows

package worksubmission

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockPreferencesFile(file *os.File) error {
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{})
}
