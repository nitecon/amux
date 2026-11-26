//go:build desktop && windows

package main

import (
	"os/exec"

	"github.com/nitecon/amux/internal/hidewindow"
)

func hideClientWindow(command *exec.Cmd) {
	hidewindow.Apply(command)
}
