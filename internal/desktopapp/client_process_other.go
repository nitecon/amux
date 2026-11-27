//go:build desktop && !unix && !windows

package desktopapp

import "os/exec"

func hideClientWindow(*exec.Cmd) {}
