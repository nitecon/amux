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

const serverConfigFilename = "amux-server.yml"

func serverFromFlags(cmd *cli.Command) config.Server {
	return config.Server{
		Database: cmd.String("database"), Profile: cmd.String("profile"), SecureInstance: cmd.String("secure-instance-id"),
		DiscoveryBind: cmd.String("discovery-bind"), ProfileTLSNames: cmd.StringSlice("profile-tls-name"),
		Listen: cmd.String("listen"), KeyEnv: cmd.String("api-key-env"), CertPath: cmd.String("tls-cert"), KeyPath: cmd.String("tls-key"),
		RetentionDays: cmd.Uint64("message-retention-days"), BotRetentionDays: cmd.Uint64("bot-message-retention-days"),
		DiscordTokenEnv: cmd.String("discord-token-env"), DiscordGuildID: cmd.String("discord-guild-id"), DiscordCategoryID: cmd.String("discord-category-id"),
	}
}

func writeServerConfig(stdout io.Writer) error {
	path, err := filepath.Abs(serverConfigFilename)
	if err != nil {
		return err
	}
	const notes = `# Start with amux server --config /absolute/path/to/amux-server.yml.
# Set exactly one of database or profile. Use absolute paths for services.
# API and Discord credentials come from the named environment variables.
# Secure enrollment uses profile, secure-instance-id and profile-tls-name.
`
	settings := config.DefaultServer()
	auth, schema := false, true
	settings.ArtifactAuthEnforced, settings.ArtifactBodySchemaEnabled = &auth, &schema
	if err := config.GenerateServer(path, settings, notes); errors.Is(err, os.ErrExist) {
		return fmt.Errorf("server configuration already exists at %s", path)
	} else if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Generated server configuration at %s\n", path)
	return err
}
