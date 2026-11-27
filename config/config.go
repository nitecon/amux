package config

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

var (
	configLock sync.RWMutex
	current    AppConfig
	serverPath string
)

type AppConfig struct {
	Client Client
	Server Server
}

// InitConfig loads the client configuration selected at startup.
func InitConfig(loc string) error {
	configLock.Lock()
	defer configLock.Unlock()
	selected, err := ReadClient(loc)
	if err != nil {
		return err
	}
	current.Client = cloneClient(selected)
	return nil
}

// UseClient publishes a validated client configuration from startup or reload.
func UseClient(selected Client) error {
	if err := selected.Validate(); err != nil {
		return err
	}
	configLock.Lock()
	defer configLock.Unlock()
	current.Client = cloneClient(selected)
	return nil
}

// Get returns an isolated snapshot of the configuration currently in use.
func Get() *AppConfig {
	configLock.RLock()
	defer configLock.RUnlock()
	return &AppConfig{Client: cloneClient(current.Client), Server: cloneServer(current.Server)}
}

func cloneClient(value Client) Client {
	value.Projects = maps.Clone(value.Projects)
	value.ExcludedProjects = maps.Clone(value.ExcludedProjects)
	value.ProjectURLs = slices.Clone(value.ProjectURLs)
	value.Providers = slices.Clone(value.Providers)
	if value.HostedGateway != nil {
		gateway := *value.HostedGateway
		value.HostedGateway = &gateway
	}
	return value
}

func absolutePath(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
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
