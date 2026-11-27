package main

import (
	"context"
	"os"
	"slices"

	"github.com/nitecon/amux/internal/amuxruntime"
	"github.com/urfave/cli/v3"
)

// Preserve invocations used by existing worker tools, installers, and services.
var legacyCommands = []string{"client", "messages", "tasks", "submit-work", "work-submission", "mcp", "worker"}
var legacyOptions = []string{"--generate-client-config", "--reload-config", "--update", "--apply-update", "--finish-update", "--add-project", "-add-project", "--generate-server-config"}

func addCompatibilityCommands(cmd *cli.Command) *cli.Command {
	for _, name := range legacyCommands {
		cmd.Commands = append(cmd.Commands, legacyCommand(name, name))
	}
	for _, option := range legacyOptions {
		cmd.Commands = append(cmd.Commands, legacyCommand("compat-"+option, option))
	}
	return cmd
}

func legacyCommand(name string, prefix ...string) *cli.Command {
	return &cli.Command{
		Name:            name,
		Hidden:          true,
		SkipFlagParsing: true,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return amuxruntime.Run(ctx, append(slices.Clone(prefix), cmd.Args().Slice()...), cmd.Writer, cmd.ErrWriter, os.Getenv)
		},
	}
}

func commandArgs(args []string) []string {
	if len(args) > 1 && slices.Contains(legacyOptions, args[1]) {
		args = slices.Clone(args)
		args[1] = "compat-" + args[1]
	}
	if len(args) > 2 && (args[1] == "server" || args[1] == "s") && args[2] == "run" {
		args = slices.Delete(slices.Clone(args), 2, 3)
	}
	if len(args) > 2 && args[1] == "client" && args[2] == "run" {
		args = slices.Delete(slices.Clone(args), 2, 3)
		args[1] = "headless"
	}
	return args
}
