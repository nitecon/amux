//go:build !linux && !darwin && !windows

package project

import (
	"fmt"
	"os"
)

func tryCloneLock(file *os.File) error {
	return fmt.Errorf("clone locking is unsupported on this OS")
}
