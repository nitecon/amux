package amuxruntime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nitecon/amux/config"
	"github.com/urfave/cli/v3"
)

const (
	serverConfigFilename             = "amux-server.yml"
	defaultArtifactAuthEnforced      = false
	defaultArtifactBodySchemaEnabled = true
)

func writeServerConfig(cmd *cli.Command, stdout io.Writer) error {
	path, err := filepath.Abs(serverConfigFilename)
	if err != nil {
		return err
	}
	const notes = `# Start with amux server --config /absolute/path/to/amux-server.yml.
# Set exactly one of database or profile. Use absolute paths for services.
# API and Discord credentials come from the named environment variables.
# Secure enrollment uses profile, secure-instance-id and profile-tls-name.
# Command-line flags override corresponding settings in this file.
`
	settings := config.ServerFromFlags(cmd)
	auth, schema := defaultArtifactAuthEnforced, defaultArtifactBodySchemaEnabled
	settings.ArtifactAuthEnforced, settings.ArtifactBodySchemaEnabled = &auth, &schema
	if err := config.GenerateServer(path, settings, notes); errors.Is(err, os.ErrExist) {
		return fmt.Errorf("server configuration already exists at %s", path)
	} else if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Generated server configuration at %s\n", path)
	return err
}
