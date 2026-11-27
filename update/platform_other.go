//go:build !linux && !windows && !darwin

package update

import "github.com/nitecon/amux/update/platform"

var native updaterPlatform = struct {
	platform.Flat
	platform.UnixProcess
}{}
