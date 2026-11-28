package process

import (
	"os/exec"
	"syscall"
)

func configureParentDeath(command *exec.Cmd) { command.SysProcAttr.Pdeathsig = syscall.SIGKILL }
