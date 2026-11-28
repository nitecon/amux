package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nitecon/amux/config"
	"github.com/nitecon/amux/internal/amuxruntime"
	"github.com/nitecon/amux/providers/process"
	"github.com/nitecon/amux/server"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"
)

var (
	version = "source"
)

func setLogger() {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
}
func main() {
	setLogger()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if os.Getenv(process.ManagedMCPEnv) == "1" {
		if err := newMCPCommand(&sdk.StdioTransport{}).Run(ctx, os.Args); err != nil {
			log.Error().Err(err).Msg("amux MCP")
			os.Exit(1)
		}
		return
	}
	if err := newCommand().Run(ctx, os.Args); err != nil {
		log.Error().Err(err).Msg("amux")
		os.Exit(1)
	}
}

func newCommand() *cli.Command {
	cmd := &cli.Command{
		Name:    "amux",
		Usage:   "Run the Amux desktop, server, or headless client",
		Version: version,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "debug",
				Aliases: []string{"d"},
				Usage:   "Enable debug logging",
			},
		},
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			zerolog.SetGlobalLevel(zerolog.InfoLevel)
			if cmd.Bool("debug") {
				zerolog.SetGlobalLevel(zerolog.DebugLevel)
			}
			return ctx, nil
		},
		Action: startDesktop,
		Commands: []*cli.Command{updateCommand(), fleetCommand(),
			{
				Name:    "server",
				Aliases: []string{"s"},
				Usage:   "Run the server",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "config", Aliases: []string{"c"}, Usage: "Load server YAML from FILE"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if err := noArguments(cmd); err != nil {
						return err
					}
					if err := config.LoadServer(cmd.String("config")); err != nil {
						return err
					}
					return server.Start(ctx, cmd.Writer, cmd.ErrWriter)
				},
			},
			{
				Name:    "headless",
				Aliases: []string{"c"},
				Usage:   "Run the client without the desktop",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "state", Usage: "Durable worker journal"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					// The configured client handles reloads and stops on cancellation.
					if err := noArguments(cmd); err != nil {
						return err
					}
					return amuxruntime.StartClient(ctx, cmd.String("state"), cmd.Writer, cmd.ErrWriter)
				},
			},
		},
	}
	return cmd
}

func noArguments(cmd *cli.Command) error {
	if cmd.Args().Len() != 0 {
		return fmt.Errorf("unexpected argument %q", cmd.Args().First())
	}
	return nil
}
