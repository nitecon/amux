//go:build desktop && unix

package main

import (
	"os/exec"
	"syscall"
)

func hideClientWindow(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
