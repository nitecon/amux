package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nitecon/amux/internal/clientconfig"
)

func generateClientConfig(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("amux --generate-client-config", flag.ContinueOnError)
	flags.SetOutput(stderr)
	profile := flags.String("enrolled-profile", "", "existing enrollment manifest path")
	endpoint := flags.String("server", "", "gateway endpoint (optional with an enrollment manifest)")
	keyEnv := flags.String("api-key-env", "", "environment variable for a bearer gateway")
	directory := flags.String("projects-directory", "", "directory containing local Git checkouts")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected client configuration arguments")
	}
	path, err := clientconfig.DefaultPath()
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
	if err := clientconfig.Generate(path, clientconfig.Config{
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
