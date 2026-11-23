//go:build windows

package bundledtools

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}
