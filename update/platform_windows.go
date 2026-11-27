//go:build windows

package update

import "github.com/nitecon/amux/update/windows"

var native updaterPlatform = windows.Updater{}
