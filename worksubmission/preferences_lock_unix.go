//go:build linux || darwin

package worksubmission

import (
	"os"
	"syscall"
)

func lockPreferencesFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
}
