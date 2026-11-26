//go:build windows

package gatewayprocess

import (
	"os/exec"

	"github.com/nitecon/amux/internal/hidewindow"
)

func prepareProcess(command *exec.Cmd) {
	hidewindow.Apply(command)
}
