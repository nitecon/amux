//go:build windows

package windows

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/nitecon/amux/update/platform"
	"golang.org/x/sys/windows"
)

type Updater struct{ platform.Flat }

func (Updater) LockUpdateFile(file *os.File) error {
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

func (Updater) WaitForProcessExit(pid int) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(handle)
	result, err := windows.WaitForSingleObject(handle, 2*60*1000)
	if err != nil {
		return err
	}
	if result != uint32(windows.WAIT_OBJECT_0) {
		return fmt.Errorf("timed out waiting for process %d to exit", pid)
	}
	time.Sleep(200 * time.Millisecond)
	return nil
}

// Avoid names that trigger Windows installer elevation heuristics.
func (Updater) HelperPattern() string { return "amux-handoff-*.exe" }

func (Updater) WriteAccessAdvice() string { return "Give your user write access to that directory" }

func (Updater) PrepareHelper(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
