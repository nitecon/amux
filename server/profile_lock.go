package server

import (
	"fmt"
	"os"
	"path/filepath"
)

// LockProfile shares the Rust server's lock path. Hold the file open for the
// full server lifetime; process death releases the OS lock without stale leases.
func LockProfile(path string) (*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if os.IsNotExist(err) {
		parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
		if err != nil {
			return nil, err
		}
		canonical = filepath.Join(parent, filepath.Base(absolute))
	} else if err != nil {
		return nil, err
	}
	return lockProfileFile(canonical+".amux-server.lock", "gateway database profile is already owned or cannot be locked")
}

// LockSecurityProfile holds the Rust-compatible owner lock for an explicitly
// selected server profile. Callers must retain the returned file until shutdown.
func LockSecurityProfile(profile string) (*os.File, error) {
	if profile == "" {
		return nil, fmt.Errorf("security profile path is required")
	}
	if err := os.MkdirAll(profile, 0700); err != nil {
		return nil, err
	}
	if err := makePrivateProfileDirectory(profile); err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(profile)
	if err != nil {
		return nil, err
	}
	return lockProfileFile(filepath.Join(canonical, ".security-owner.lock"), "security profile is already owned or cannot be locked")
}

func lockProfileFile(path, conflict string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := tryProfileLock(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("%s: %w", conflict, err)
	}
	return file, nil
}
