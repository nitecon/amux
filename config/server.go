package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"
)

type Server struct {
	GCP                       GoogleConfig `yaml:"gcp,omitempty"`
	Aws                       AwsConfig    `yaml:"aws,omitempty"`
	Database                  string       `yaml:"database"`
	Profile                   string       `yaml:"profile"`
	SecureInstance            string       `yaml:"secure-instance-id"`
	DiscoveryBind             string       `yaml:"discovery-bind"`
	ProfileTLSNames           []string     `yaml:"profile-tls-name"`
	Listen                    string       `yaml:"listen"`
	KeyEnv                    string       `yaml:"api-key-env"`
	CertPath                  string       `yaml:"tls-cert"`
	KeyPath                   string       `yaml:"tls-key"`
	RetentionDays             uint64       `yaml:"message-retention-days"`
	BotRetentionDays          uint64       `yaml:"bot-message-retention-days"`
	DiscordTokenEnv           string       `yaml:"discord-token-env"`
	DiscordGuildID            string       `yaml:"discord-guild-id"`
	DiscordCategoryID         string       `yaml:"discord-category-id"`
	ArtifactAuthEnforced      *bool        `yaml:"artifact-auth-enforced,omitempty"`
	ArtifactBodySchemaEnabled *bool        `yaml:"artifact-body-schema-enabled,omitempty"`
}

type GoogleConfig struct {
	ProjectID        string `yaml:"project-id"`
	QueueEndpoint    string `yaml:"queue-endpoint"`
	QueueSubEndpoint string `yaml:"queue-sub-endpoint"`
}

type AwsConfig struct {
	SQSEndpoint string `yaml:"sqs-endpoint"`
}

// DefaultServer supplies defaults for omitted YAML settings.
func DefaultServer() Server {
	return Server{
		Listen: "127.0.0.1:7913", KeyEnv: "GATEWAY_API_KEY",
		RetentionDays: 30, BotRetentionDays: 7,
	}
}

// Validate checks the server settings that do not require opening resources.
func (settings Server) Validate() error {
	if (settings.Database == "") == (settings.Profile == "") {
		return errors.New("supply exactly one of database or profile")
	}
	if settings.DiscoveryBind != "" && settings.SecureInstance == "" {
		return errors.New("discovery-bind requires secure-instance-id")
	}
	if settings.SecureInstance != "" {
		instance, err := uuid.Parse(settings.SecureInstance)
		if err != nil || instance == uuid.Nil || instance.String() != settings.SecureInstance {
			return errors.New("secure-instance-id requires a canonical nonzero UUID")
		}
		if settings.Profile == "" || settings.CertPath != "" || settings.KeyPath != "" {
			return errors.New("secure-instance-id requires profile and profile-managed TLS")
		}
	}
	if len(settings.ProfileTLSNames) > 0 && settings.Profile == "" {
		return errors.New("profile-tls-name requires profile")
	}
	if len(settings.ProfileTLSNames) > 0 && (settings.CertPath != "" || settings.KeyPath != "") {
		return errors.New("profile-tls-name cannot be combined with tls-cert or tls-key")
	}
	if (settings.CertPath == "") != (settings.KeyPath == "") {
		return errors.New("tls-cert and tls-key must be supplied together")
	}
	if (settings.DiscordTokenEnv != "" || settings.DiscordGuildID != "" || settings.DiscordCategoryID != "") &&
		(settings.DiscordTokenEnv == "" || settings.DiscordGuildID == "") {
		return errors.New("discord-token-env and discord-guild-id must be supplied together")
	}
	if _, _, err := net.SplitHostPort(settings.Listen); err != nil {
		return errors.New("invalid listen address")
	}
	return nil
}

// ReadServer reads server settings and fills in omitted defaults.
func ReadServer(path string) (Server, error) {
	if path == "" {
		return Server{}, errors.New("server configuration is required; use --config PATH")
	}
	file, err := os.Open(path)
	if err != nil {
		return Server{}, err
	}
	defer file.Close()
	settings, err := DecodeServer(file)
	if err != nil {
		return Server{}, fmt.Errorf("read server config %s: %w", path, err)
	}
	return settings, nil
}

// DecodeServer overlays YAML settings on defaults.
func DecodeServer(reader io.Reader) (Server, error) {
	selected := DefaultServer()
	fields := map[string]any{
		"database": &selected.Database, "profile": &selected.Profile,
		"secure-instance-id": &selected.SecureInstance, "discovery-bind": &selected.DiscoveryBind,
		"profile-tls-name": &selected.ProfileTLSNames, "listen": &selected.Listen,
		"api-key-env": &selected.KeyEnv, "tls-cert": &selected.CertPath, "tls-key": &selected.KeyPath,
		"message-retention-days": &selected.RetentionDays, "bot-message-retention-days": &selected.BotRetentionDays,
		"discord-token-env": &selected.DiscordTokenEnv, "discord-guild-id": &selected.DiscordGuildID,
		"discord-category-id":          &selected.DiscordCategoryID,
		"artifact-auth-enforced":       &selected.ArtifactAuthEnforced,
		"artifact-body-schema-enabled": &selected.ArtifactBodySchemaEnabled,
	}
	values, err := decodeServerFields(reader)
	if err != nil {
		return Server{}, err
	}
	for name, node := range values {
		field, ok := fields[name]
		if !ok {
			return Server{}, fmt.Errorf("unknown server setting %q", name)
		}
		if err := setServerField(field, node); err != nil {
			return Server{}, fmt.Errorf("server setting %s: %w", name, err)
		}
	}
	return selected, nil
}

func setServerField(field any, node yaml.Node) error {
	if setting, ok := field.(**bool); ok {
		if node.Tag != "!!bool" {
			return errors.New("must be a boolean")
		}
		var value bool
		if err := node.Decode(&value); err != nil {
			return err
		}
		*setting = &value
		return nil
	}
	if names, ok := field.(*[]string); ok && node.Kind == yaml.SequenceNode {
		values := make([]string, 0, len(node.Content))
		for _, entry := range node.Content {
			if entry.Tag != "!!str" {
				return errors.New("requires strings")
			}
			values = append(values, entry.Value)
		}
		*names = values
		return nil
	}
	if node.Kind != yaml.ScalarNode || (node.Tag != "!!str" && node.Tag != "!!int") {
		return errors.New("requires a string or integer")
	}
	switch target := field.(type) {
	case *string:
		*target = node.Value
	case *uint64:
		value, err := strconv.ParseUint(node.Value, 0, 64)
		if err != nil {
			return err
		}
		*target = value
	case *[]string:
		*target = []string{node.Value}
	}
	return nil
}

// SaveServer validates and writes the server configuration file.
func SaveServer(path string, selected Server) error {
	if err := selected.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(selected)
	if err != nil {
		return err
	}
	return WriteFile(path, data)
}

func GenerateServer(path string, selected Server, notes string) error {
	return generateYAML(path, selected, notes)
}

func decodeServerFields(reader io.Reader) (map[string]yaml.Node, error) {
	var values map[string]yaml.Node
	decoder := yaml.NewDecoder(reader)
	if err := decoder.Decode(&values); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected a single YAML document")
	}
	return values, nil
}
