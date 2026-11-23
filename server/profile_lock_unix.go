//go:build linux || darwin

package server

import (
	"os"
	"path/filepath"
	"syscall"
)

func tryProfileLock(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func makePrivateProfileDirectory(path string) error {
	return os.Chmod(filepath.Clean(path), 0700)
}
