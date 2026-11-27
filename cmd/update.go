package main

import (
	"context"
	"fmt"
	"os"

	"github.com/nitecon/amux/update"
	"github.com/urfave/cli/v3"
)

func updateCommand() *cli.Command {
	return &cli.Command{
		Name:  "update",
		Usage: "Download and install a verified Amux update",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "staged", Hidden: true},
			&cli.StringFlag{Name: "executable", Hidden: true},
			&cli.StringFlag{Name: "parent-pid", Hidden: true},
			&cli.StringFlag{Name: "wait-pids", Hidden: true},
			&cli.StringFlag{Name: "restart-args", Hidden: true},
			&cli.StringFlag{Name: "cache-root", Hidden: true},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := noArguments(cmd); err != nil {
				return err
			}
			if cmd.IsSet("staged") {
				return update.FinishHelper([]string{"update", cmd.String("staged"), cmd.String("executable"),
					cmd.String("parent-pid"), cmd.String("wait-pids"), cmd.String("restart-args"), cmd.String("cache-root")})
			}
			executable, err := os.Executable()
			if err != nil {
				return err
			}
			archive, info, err := update.Stage(ctx, executable)
			if err != nil {
				return err
			}
			if !info.Available {
				_, err = fmt.Fprintf(cmd.Writer, "amux %s is up to date.\n", info.Version)
				return err
			}
			return update.StartApply(executable, archive, nil, nil)
		},
	}
}
