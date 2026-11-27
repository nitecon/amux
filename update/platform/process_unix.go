//go:build !windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

type UnixProcess struct{}

func (UnixProcess) LockUpdateFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func (UnixProcess) WaitForProcessExit(pid int) error {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for process %d to exit", pid)
}
