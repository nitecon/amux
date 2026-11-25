//go:build desktop && windows

package main

import (
	"os/exec"
	"syscall"
)

func hideClientWindow(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
