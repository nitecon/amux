//go:build windows

package bundledtools

import (
	"os/exec"

	"github.com/nitecon/amux/internal/hidewindow"
)

func configureProcess(command *exec.Cmd) {
	hidewindow.Apply(command)
}
