// Package hidewindow keeps console programs started by the windowed desktop
// from opening their own console windows.
package hidewindow

import (
	"os/exec"
	"syscall"
)

// Apply starts command without a console window.
func Apply(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
}
