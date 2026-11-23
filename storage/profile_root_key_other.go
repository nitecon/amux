//go:build !windows

package storage

func protectProfileRootKey(key []byte) ([]byte, error) { return key, nil }

func unprotectProfileRootKey(key []byte) ([]byte, error) { return key, nil }
