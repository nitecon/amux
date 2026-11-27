//go:build !unix

package amuxruntime

import "os"

func clientReloadSignals() (<-chan os.Signal, func()) {
	return nil, func() {}
}
