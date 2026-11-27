package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nitecon/amux/gatewayorigin"
	"github.com/nitecon/amux/projects"

	"gopkg.in/yaml.v3"
)

const LocalGatewayEndpoint = "https://127.0.0.1:7913"

type HostedGateway struct {
	InstanceID     string `yaml:"instance_id"`
	AdvertisedHost string `yaml:"advertised_host"`
	RemoteHosting  bool   `yaml:"remote_hosting,omitempty"`
}

type Client struct {
	HostedGateway     *HostedGateway    `yaml:"hosted_gateway,omitempty"`
	WorkerJournal     string            `yaml:"worker_journal,omitempty"`
	EnrolledProfile   string            `yaml:"enrolled_profile,omitempty"`
	GatewayEndpoint   string            `yaml:"gateway_endpoint,omitempty"`
	APIKeyEnv         string            `yaml:"api_key_env,omitempty"`
	TaskToolsCommand  string            `yaml:"task_tools_command,omitempty"`
	WorkPreferences   string            `yaml:"work_preferences,omitempty"`
	ProjectsDirectory string            `yaml:"projects_directory,omitempty"`
	Projects          map[string]string `yaml:"projects,omitempty"`
	ExcludedProjects  map[string]bool   `yaml:"excluded_projects,omitempty"`
	ProjectURLs       []string          `yaml:"project_urls"`
	Providers         []string          `yaml:"providers"`
}

func DefaultClientPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".amux", "config.yml"), nil
}

func ReadClient(path string) (Client, error) {
	file, err := os.Open(path)
	if err != nil {
		return Client{}, err
	}
	defer file.Close()
	return DecodeClient(file)
}

func DecodeClient(reader io.Reader) (Client, error) {
	var config Client
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Client{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return Client{}, err
		}
		return Client{}, errors.New("expected a single YAML document")
	}
	if err := config.Validate(); err != nil {
		return Client{}, err
	}
	return config, nil
}

func (config Client) Validate() error {
	if config.EnrolledProfile != "" {
		if !filepath.IsAbs(config.EnrolledProfile) || config.APIKeyEnv != "" {
			return errors.New("enrolled_profile must be an absolute path and cannot use api_key_env")
		}
	} else if config.GatewayEndpoint == "" || config.APIKeyEnv == "" || strings.ContainsAny(config.APIKeyEnv, "=\x00") {
		return errors.New("provide an enrolled_profile or a gateway_endpoint with an API key environment variable")
	}
	if config.GatewayEndpoint != "" {
		if _, err := gatewayorigin.Parse(config.GatewayEndpoint); err != nil {
			return err
		}
	}
	if config.WorkPreferences != "" && !filepath.IsAbs(config.WorkPreferences) {
		return errors.New("work_preferences must be an absolute path")
	}
	if config.HostedGateway != nil {
		if filepath.Base(config.EnrolledProfile) != "local-client.json" ||
			config.HostedGateway.InstanceID == "" || config.HostedGateway.AdvertisedHost == "" {
			return errors.New("hosted_gateway requires a local-client.json enrollment path, instance ID and advertised host")
		}
	}
	if config.WorkerJournal != "" && !filepath.IsAbs(config.WorkerJournal) {
		return errors.New("worker_journal must be an absolute path")
	}
	if config.ProjectsDirectory != "" && !filepath.IsAbs(config.ProjectsDirectory) {
		return errors.New("projects_directory must be an absolute path")
	}
	for id, directory := range config.Projects {
		if strings.TrimSpace(id) != id || id == "" {
			return fmt.Errorf("invalid project ID %q", id)
		}
		if !filepath.IsAbs(directory) {
			return fmt.Errorf("project %q must have an absolute checkout path", id)
		}
	}
	for id := range config.ExcludedProjects {
		if id == "" || strings.TrimSpace(id) != id {
			return fmt.Errorf("invalid excluded project ID %q", id)
		}
	}
	if len(config.ProjectURLs) != 0 && config.ProjectsDirectory == "" {
		return errors.New("project_urls requires projects_directory")
	}
	remotes := make(map[string]bool)
	for _, url := range config.ProjectURLs {
		remote, err := projects.CanonicalRemote(url)
		if err != nil {
			return fmt.Errorf("invalid project URL %q: %w", url, err)
		}
		if remotes[remote] {
			return fmt.Errorf("duplicate project URL %q", url)
		}
		remotes[remote] = true
	}
	seen := make(map[string]bool)
	for _, provider := range config.Providers {
		switch provider {
		case "antigravity", "claude", "codex":
		default:
			return fmt.Errorf("unknown provider %q", provider)
		}
		if seen[provider] {
			return fmt.Errorf("duplicate provider %q", provider)
		}
		seen[provider] = true
	}
	return nil
}

// SaveClient validates the new configuration before replacing the previous file.
func SaveClient(path string, config Client) error {
	if err := config.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	return WriteFile(path, data)
}

func SameHostedGateway(a, b *HostedGateway) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// GenerateClient creates a commented, editable YAML template without replacing an existing file.
func GenerateClient(path string, config Client) error {
	if err := config.Validate(); err != nil {
		return err
	}
	const notes = `# Enrolled clients use the gateway recorded in their enrollment manifest.
# Set enrolled_profile to the path printed by amux client enroll.
# For a bearer gateway, remove enrolled_profile and set gateway_endpoint and api_key_env.
# project_urls lists Git remotes to register if missing on the gateway.
# Existing gateway projects automatically match Git checkouts under projects_directory.
# Example: project_urls: [https://github.com/example/project.git]
# Use projects: {gateway-project-id: /absolute/path} for explicit checkout paths.
`
	return generateYAML(path, config, notes)
}

func generateYAML(path string, value any, notes string) error {
	data, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append([]byte(notes), data...))
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}
