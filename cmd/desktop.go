package main

import (
	"context"
	"os"

	"github.com/nitecon/amux/internal/desktopapp"
	"github.com/urfave/cli/v3"
)

func startDesktop(ctx context.Context, cmd *cli.Command) error {
	if err := noArguments(cmd); err != nil {
		return err
	}
	return desktopapp.Run(ctx, os.Args[1:])
}
