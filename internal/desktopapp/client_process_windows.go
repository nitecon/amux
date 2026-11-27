//go:build desktop && windows

package desktopapp

import (
	"os/exec"

	"github.com/nitecon/amux/internal/hidewindow"
)

func hideClientWindow(command *exec.Cmd) {
	hidewindow.Apply(command)
}
