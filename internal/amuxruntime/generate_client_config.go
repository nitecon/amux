package amuxruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nitecon/amux/config"
	"github.com/urfave/cli/v3"
)

func generateClientConfig(args []string, stdout, stderr io.Writer) error {
	flags := commandFlags("amux --generate-client-config", stderr)
	profile := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "enrolled-profile", Value: "", Usage: "existing enrollment manifest path", Destination: profile})
	endpoint := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "server", Value: "", Usage: "gateway endpoint (optional with an enrollment manifest)", Destination: endpoint})
	keyEnv := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "api-key-env", Value: "", Usage: "environment variable for a bearer gateway", Destination: keyEnv})
	directory := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "projects-directory", Value: "", Usage: "directory containing local Git checkouts", Destination: directory})
	if err := parseCommand(context.Background(), flags, args); err != nil {
		if errors.Is(err, errCommandHelp) {
			return nil
		}
		return err
	}
	if flags.Args().Len() != 0 {
		return fmt.Errorf("unexpected client configuration arguments")
	}
	path, err := config.DefaultClientPath()
	if err != nil {
		return err
	}
	home := filepath.Dir(filepath.Dir(path))
	if *directory == "" {
		*directory = filepath.Join(home, "projects")
		if info, err := os.Stat(*directory); err != nil || !info.IsDir() {
			*directory = home
		}
	}
	if *profile == "" && *keyEnv == "" {
		*profile = filepath.Join(filepath.Dir(path), "client-profile.json")
	}
	if *profile != "" {
		*profile, err = filepath.Abs(*profile)
		if err != nil {
			return err
		}
	}
	if err := config.GenerateClient(path, config.Client{
		EnrolledProfile: *profile, GatewayEndpoint: *endpoint, APIKeyEnv: *keyEnv,
		ProjectsDirectory: *directory, Providers: []string{"antigravity", "claude", "codex"},
	}); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("client configuration already exists at %s", path)
		}
		return err
	}
	_, err = fmt.Fprintf(stdout, "Generated client configuration at %s\n", path)
	return err
}
