//go:build darwin

package update

import "github.com/nitecon/amux/update/mac"

var native updaterPlatform = mac.Updater{}
