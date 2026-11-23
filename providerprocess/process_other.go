//go:build !linux && !darwin && !windows

package providerprocess

import "os/exec"

func configureProcess(command *exec.Cmd) {}
func ownProcess(command *exec.Cmd) (func(), func() error, error) {
	return func() {}, command.Process.Kill, nil
}
