package storage

import (
	"fmt"
	"os"
)

func lockPairingFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	if err := tryPairingFileLock(file); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func pairingLockUnsupported() error {
	return fmt.Errorf("peer profile locking is unsupported on this OS")
}
