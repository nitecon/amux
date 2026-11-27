package amuxruntime

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"
)

const (
	serverConfigFilename             = "amux-server.yml"
	defaultArtifactAuthEnforced      = false
	defaultArtifactBodySchemaEnabled = true
)

var serverConfigBooleans = []struct {
	option, env string
	value       bool
}{
	{"artifact-auth-enforced", "GATEWAY_ARTIFACT_AUTH_ENFORCED", defaultArtifactAuthEnforced},
	{"artifact-body-schema-enabled", "GATEWAY_ARTIFACT_BODY_SCHEMA_ENABLED", defaultArtifactBodySchemaEnabled},
}

func writeServerConfig(flags *flag.FlagSet, stdout io.Writer) error {
	defaults := make(map[string]any)
	flags.VisitAll(func(f *flag.Flag) {
		if f.Name == "profile-tls-name" {
			defaults[f.Name] = []string{}
			return
		}
		defaults[f.Name] = f.Value.(flag.Getter).Get()
	})
	for _, option := range serverConfigBooleans {
		defaults[option.option] = option.value
	}
	data, err := yaml.Marshal(defaults)
	if err != nil {
		return err
	}
	path, err := filepath.Abs(serverConfigFilename)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("server configuration already exists at %s", path)
	}
	if err != nil {
		return err
	}
	const notes = `# Start with amux server --config /absolute/path/to/amux-server.yml.
# Set exactly one of database or profile. Use absolute paths for services.
# API and Discord credentials come from the named environment variables.
# Secure enrollment uses profile, secure-instance-id and profile-tls-name.
# Command-line flags override corresponding settings in this file.
`
	_, writeErr := file.Write(append([]byte(notes), data...))
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = os.Remove(path)
		return err
	}
	_, err = fmt.Fprintf(stdout, "Generated server configuration at %s\n", path)
	return err
}

func applyServerConfig(flags *flag.FlagSet, path string) (map[string]string, error) {
	explicit := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	return readServerConfig(path, func(name string) bool { return flags.Lookup(name) != nil }, func(name string) bool { return explicit[name] }, flags.Set)
}

func LoadServerConfig(cmd *cli.Command, path string, getenv func(string) string) (func(string) string, error) {
	if path == "" {
		return getenv, nil
	}
	env, err := readServerConfig(path, func(name string) bool {
		for _, flag := range cmd.Flags {
			if flag.Names()[0] == name && name != "help" {
				return true
			}
		}
		return false
	}, cmd.IsSet, func(name, value string) error {
		return cmd.Set(name, value)
	})
	if err != nil {
		return nil, err
	}
	return func(name string) string {
		if value, found := env[name]; found {
			return value
		}
		return getenv(name)
	}, nil
}

func readServerConfig(path string, exists, explicit func(string) bool, set func(string, string) error) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var values map[string]yaml.Node
	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("read server config %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("server config %s must contain one YAML document", path)
	}
	env := make(map[string]string)
	for _, option := range serverConfigBooleans {
		if node, exists := values[option.option]; exists {
			if node.Tag != "!!bool" {
				return nil, fmt.Errorf("server setting %s must be a boolean", option.option)
			}
			var enabled bool
			if err := node.Decode(&enabled); err != nil {
				return nil, err
			}
			env[option.env] = strconv.FormatBool(enabled)
			delete(values, option.option)
		}
	}
	for name, node := range values {
		if !exists(name) || name == "config" {
			return nil, fmt.Errorf("unknown server setting %q", name)
		}
		var entries []string
		switch {
		case name == "profile-tls-name" && node.Kind == yaml.SequenceNode:
			for _, entry := range node.Content {
				if entry.Tag != "!!str" {
					return nil, fmt.Errorf("server setting %s requires strings", name)
				}
				entries = append(entries, entry.Value)
			}
		case node.Kind == yaml.ScalarNode && (node.Tag == "!!str" || node.Tag == "!!int"):
			entries = []string{node.Value}
		default:
			return nil, fmt.Errorf("invalid value for server setting %q", name)
		}
		if explicit(name) {
			continue
		}
		for _, value := range entries {
			if err := set(name, value); err != nil {
				return nil, fmt.Errorf("server setting %s: %w", name, err)
			}
		}
	}
	return env, nil
}
