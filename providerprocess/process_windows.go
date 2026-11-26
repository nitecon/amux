package providerprocess

import (
	"fmt"
	"os/exec"
	"unsafe"

	"github.com/nitecon/amux/internal/hidewindow"
	"golang.org/x/sys/windows"
)

func configureProcess(command *exec.Cmd) {
	hidewindow.Apply(command)
}
func ownProcess(command *exec.Cmd) (func(), func() error, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
		return nil, nil, err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return nil, nil, err
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		windows.CloseHandle(job)
		return nil, nil, fmt.Errorf("own provider process: %w", err)
	}
	return func() { windows.CloseHandle(job) }, func() error { return windows.TerminateJobObject(job, 1) }, nil
}
