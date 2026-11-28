//go:build linux || darwin

package project

import (
	"os"
	"syscall"
)

func tryCloneLock(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
