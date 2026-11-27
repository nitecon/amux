package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nitecon/amux/internal/amuxruntime"
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
	if err := newCommand().Run(ctx, commandArgs(os.Args)); err != nil {
		log.Error().Err(err).Msg("amux")
		os.Exit(1)
	}
}

func newCommand() *cli.Command {
	cmd := &cli.Command{
		Name:    "amux",
		Usage:   "Run the Amux desktop, server, or headless client",
		Version: version,
		Flags: append([]cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Usage:   "Load configuration from FILE",
			},
			&cli.BoolFlag{
				Name:    "debug",
				Aliases: []string{"d"},
				Usage:   "Enable debug logging",
			},
		}, desktopFlags()...),
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			zerolog.SetGlobalLevel(zerolog.InfoLevel)
			if cmd.Bool("debug") {
				zerolog.SetGlobalLevel(zerolog.DebugLevel)
			}
			return ctx, nil
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// Start the client and desktop with the same shutdown context.
			return startDesktop(ctx, cmd)
		},
		Commands: []*cli.Command{
			{
				Name:     "server",
				Aliases:  []string{"s"},
				Usage:    "Run the server",
				Flags:    serverFlags(),
				Commands: []*cli.Command{legacyCommand("peering-key", "server", "peering-key")},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					// Run one server until shutdown.
					return startServer(ctx, cmd)
				},
			},
			{
				Name:    "headless",
				Aliases: []string{"c"},
				Usage:   "Run the client without the desktop",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "state", Usage: "Durable worker journal"},
					&cli.StringFlag{Name: "enrolled-profile", Usage: "Enrolled gateway manifest"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					// The configured client handles reloads and stops on cancellation.
					if err := noArguments(cmd); err != nil {
						return err
					}
					return amuxruntime.StartClient(ctx, cmd.String("config"), cmd.String("state"), cmd.String("enrolled-profile"), cmd.Writer, cmd.ErrWriter)
				},
			},
		},
	}
	return addCompatibilityCommands(cmd)
}

func noArguments(cmd *cli.Command) error {
	if cmd.Args().Len() != 0 {
		return fmt.Errorf("unexpected argument %q", cmd.Args().First())
	}
	return nil
}
