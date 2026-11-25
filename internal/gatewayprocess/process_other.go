//go:build !windows

package gatewayprocess

import "os/exec"

func prepareProcess(*exec.Cmd) {}
