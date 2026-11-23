//go:build windows

package storage

import (
	"os"

	"golang.org/x/sys/windows"
)

func tryPairingFileLock(file *os.File) error {
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}
