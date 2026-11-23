//go:build linux || darwin

package storage

import (
	"os"
	"syscall"
)

func tryPairingFileLock(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
