//go:build !linux && !darwin && !windows

package projectidentity

import (
	"fmt"
	"os"
)

func tryCloneLock(file *os.File) error {
	return fmt.Errorf("clone locking is unsupported on this OS")
}
