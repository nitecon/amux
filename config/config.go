package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

var (
	clientLock   sync.RWMutex
	activeClient Client
)

// LoadClient publishes client YAML settings after validation and any read-only reload checks succeed.
func LoadClient(path string, checks ...func(Client) error) error {
	selected, err := ReadClient(path)
	if errors.Is(err, os.ErrNotExist) && filepath.Base(path) == clientConfigName {
		if migrateErr := migrateClientFiles(path); migrateErr != nil && !errors.Is(migrateErr, os.ErrNotExist) {
			return migrateErr
		}
		selected, err = ReadClient(path)
	}
	if err != nil {
		return err
	}
	return publishClient(selected, checks)
}

// LoadClientFleet publishes device settings combined with the gateway's active fleet settings.
func LoadClientFleet(ctx context.Context, path string, gateway fleetSource, checks ...func(Client) error) error {
	selected, err := ReadClient(path)
	if err != nil {
		return err
	}
	selected.Providers, err = fleetProviders(ctx, gateway, selected.Providers)
	if err != nil {
		return err
	}
	return publishClient(selected, checks)
}

func publishClient(selected Client, checks []func(Client) error) error {
	for _, check := range checks {
		if err := check(selected); err != nil {
			return err
		}
	}
	clientLock.Lock()
	defer clientLock.Unlock()
	activeClient = selected
	return nil
}

// Get returns the active client settings. Referenced data is read-only.
func Get() Client {
	clientLock.RLock()
	defer clientLock.RUnlock()
	return activeClient
}

var (
	serverLock   sync.RWMutex
	activeServer Server
)

// GetServer returns the active server settings. Referenced data is read-only.
func GetServer() Server {
	serverLock.RLock()
	defer serverLock.RUnlock()
	return activeServer
}

// LoadServer loads and publishes the server YAML settings.
func LoadServer(path string) error {
	selected, err := ReadServer(path)
	if err != nil {
		return err
	}
	if err := selected.Validate(); err != nil {
		return err
	}
	serverLock.Lock()
	defer serverLock.Unlock()
	activeServer = selected
	return nil
}

// WriteFile atomically replaces a configuration or preference file.
func WriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
