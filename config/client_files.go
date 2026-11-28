package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const clientConfigName = "config.yaml"
const deviceConfigName = "device.yaml"

type clientConnection struct {
	GatewayEndpoint string `yaml:"gateway_endpoint,omitempty"`
	EnrolledProfile string `yaml:"enrolled_profile,omitempty"`
	APIKeyEnv       string `yaml:"api_key_env,omitempty"`
}

type clientDevice struct {
	HostedGateway     *HostedGateway    `yaml:"hosted_gateway,omitempty"`
	WorkerJournal     string            `yaml:"worker_journal,omitempty"`
	TaskToolsCommand  string            `yaml:"task_tools_command,omitempty"`
	WorkPreferences   string            `yaml:"work_preferences,omitempty"`
	ProjectsDirectory string            `yaml:"projects_directory,omitempty"`
	Projects          map[string]string `yaml:"projects,omitempty"`
	ExcludedProjects  map[string]bool   `yaml:"excluded_projects,omitempty"`
	ProjectURLs       []string          `yaml:"project_urls,omitempty"`
	Providers         *[]string         `yaml:"providers,omitempty"`
}

func readClientFile(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return decodeClientFile(file, target)
}

func decodeClientFile(reader io.Reader, target any) error {
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("expected a single YAML document")
	}
	return nil
}

func readClientFiles(path string) (Client, error) {
	var connection clientConnection
	if err := readClientFile(path, &connection); err != nil {
		return Client{}, err
	}
	devicePath := filepath.Join(filepath.Dir(path), deviceConfigName)
	var device clientDevice
	if err := readClientFile(devicePath, &device); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Client{}, fmt.Errorf("read device settings: %w", err)
	}
	settings := Client{
		GatewayEndpoint: connection.GatewayEndpoint, EnrolledProfile: connection.EnrolledProfile, APIKeyEnv: connection.APIKeyEnv,
		HostedGateway: device.HostedGateway, WorkerJournal: device.WorkerJournal, TaskToolsCommand: device.TaskToolsCommand,
		WorkPreferences: device.WorkPreferences, ProjectsDirectory: device.ProjectsDirectory,
		Projects: device.Projects, ExcludedProjects: device.ExcludedProjects, ProjectURLs: device.ProjectURLs,
	}
	if device.Providers != nil {
		settings.Providers = *device.Providers
	}
	return settings, settings.Validate()
}

func splitClient(settings Client) (clientConnection, clientDevice) {
	connection := clientConnection{GatewayEndpoint: settings.GatewayEndpoint, EnrolledProfile: settings.EnrolledProfile, APIKeyEnv: settings.APIKeyEnv}
	deviceSettings := clientDevice{
		HostedGateway: settings.HostedGateway, WorkerJournal: settings.WorkerJournal, TaskToolsCommand: settings.TaskToolsCommand,
		WorkPreferences: settings.WorkPreferences, ProjectsDirectory: settings.ProjectsDirectory,
		Projects: settings.Projects, ExcludedProjects: settings.ExcludedProjects, ProjectURLs: settings.ProjectURLs,
	}
	if settings.Providers != nil {
		deviceSettings.Providers = &settings.Providers
	}
	return connection, deviceSettings
}

func saveClientFiles(path string, settings Client) error {
	connection, deviceSettings := splitClient(settings)
	gateway, err := yaml.Marshal(connection)
	if err != nil {
		return err
	}
	device, err := yaml.Marshal(deviceSettings)
	if err != nil {
		return err
	}
	devicePath := filepath.Join(filepath.Dir(path), deviceConfigName)
	previous, readErr := os.ReadFile(devicePath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if err := WriteFile(devicePath, device); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, gateway) {
		return nil
	}
	if err := WriteFile(path, gateway); err != nil {
		if readErr == nil {
			return errors.Join(err, WriteFile(devicePath, previous))
		}
		return errors.Join(err, os.Remove(devicePath))
	}
	return nil
}

func generateClientFiles(path string, settings Client) error {
	connection, device := splitClient(settings)
	if err := generateYAML(path, connection, "# Gateway connectivity for Amux.\n"); err != nil {
		return err
	}
	devicePath := filepath.Join(filepath.Dir(path), deviceConfigName)
	if err := generateYAML(devicePath, device, "# Local checkout, worker, and provider settings.\n"); err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return nil
}

func migrateClientFiles(path string) error {
	previous := filepath.Join(filepath.Dir(path), "config.yml")
	settings, err := ReadClient(previous)
	if err != nil {
		return err
	}
	return SaveClient(path, settings)
}
