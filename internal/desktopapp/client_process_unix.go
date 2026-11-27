//go:build desktop && unix

package desktopapp

import (
	"os/exec"
	"syscall"
)

func hideClientWindow(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
