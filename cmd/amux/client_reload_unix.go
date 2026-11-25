//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func clientReloadSignals() (<-chan os.Signal, func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)
	return signals, func() { signal.Stop(signals) }
}
