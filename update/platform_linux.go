//go:build linux

package update

import "github.com/nitecon/amux/update/linux"

var native updaterPlatform = linux.Updater{}
