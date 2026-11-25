//go:build !unix

package main

import "os"

func clientReloadSignals() (<-chan os.Signal, func()) {
	return nil, func() {}
}
