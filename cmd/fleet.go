package main

import (
	"context"
	"fmt"

	"github.com/nitecon/amux/config"
	"github.com/nitecon/amux/internal/amuxruntime"
	"github.com/urfave/cli/v3"
)

func fleetCommand() *cli.Command {
	return &cli.Command{Name: "fleet", Usage: "Manage fleet conversations", Commands: []*cli.Command{{
		Name: "close", Usage: "Retire a fleet conversation and retain its transcript",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "project", Required: true},
			&cli.StringFlag{Name: "conversation", Required: true},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := noArguments(cmd); err != nil {
				return err
			}
			path, err := config.DefaultClientPath()
			if err != nil {
				return err
			}
			if err := config.LoadClient(path); err != nil {
				return err
			}
			if err := amuxruntime.CloseFleetConversation(ctx, cmd.String("project"), cmd.String("conversation")); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.Writer, "Fleet conversation closed; transcript retained.")
			return err
		},
	}}}
}
