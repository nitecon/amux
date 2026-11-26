//go:build !windows

// Package hidewindow keeps console programs started by the windowed desktop
// from opening their own console windows.
package hidewindow

import "os/exec"

// Apply does nothing where child processes never open console windows.
func Apply(*exec.Cmd) {}
