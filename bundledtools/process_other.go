//go:build !windows

package bundledtools

import "os/exec"

func configureProcess(*exec.Cmd) {}
