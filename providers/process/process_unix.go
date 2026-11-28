//go:build linux || darwin

package process

import (
	"os/exec"
	"syscall"
)

func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	configureParentDeath(command)
}
func ownProcess(command *exec.Cmd) (func(), func() error, error) {
	kill := func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	return func() {}, kill, nil
}
