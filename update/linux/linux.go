//go:build linux

package linux

import (
	"path/filepath"

	"github.com/nitecon/amux/update/platform"
)

type Updater struct {
	platform.Flat
	platform.UnixProcess
}

func (Updater) LocalNames(executable, cli, gui string) (string, string) {
	if filepath.Base(executable) == cli+".bin" {
		return cli + ".bin", gui + ".bin"
	}
	return cli, gui
}
