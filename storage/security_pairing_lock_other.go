//go:build !linux && !darwin && !windows

package storage

import "os"

func tryPairingFileLock(*os.File) error { return pairingLockUnsupported() }
