//go:build !linux && !darwin && !windows

package server

import (
	"fmt"
	"os"
)

func tryProfileLock(file *os.File) error {
	return fmt.Errorf("server profile locking is unsupported on this OS")
}

func makePrivateProfileDirectory(string) error { return nil }
