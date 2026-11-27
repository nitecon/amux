package main

import (
	"context"
	"os"

	"github.com/nitecon/amux/internal/desktopapp"
	"github.com/urfave/cli/v3"
)

func desktopFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Local: true, Name: "server", Value: "", Usage: "connect to a gateway instead of starting the local server"},
		&cli.StringFlag{Local: true, Name: "database", Value: "", Usage: "local gateway database (OS config directory by default)"},
		&cli.StringFlag{Local: true, Name: "project", Value: "", Usage: "initial browser project, or project for a direct conversation"},
		&cli.StringFlag{Local: true, Name: "project-dir", Value: "", Usage: "launch working directory"},
		&cli.StringFlag{Local: true, Name: "conversation", Value: "", Usage: "open a conversation directly instead of browsing projects and tasks"},
		&cli.StringFlag{Local: true, Name: "preferences", Value: "", Usage: "navigation preferences file (OS config directory by default; --preferences= uses memory only)"},
		&cli.StringFlag{Local: true, Name: "work-preferences", Value: "", Usage: "work submission preferences file (OS config directory by default)"},
		&cli.StringFlag{Local: true, Name: "gateway-preferences", Value: "", Usage: "gateway mode preferences file (OS config directory by default)"},
		&cli.StringFlag{Local: true, Name: "gateway-state-dir", Value: "", Usage: "server identity and enrolled peer directory (defaults beside --gateway-preferences when supplied)"},
		&cli.StringFlag{Local: true, Name: "api-key-env", Value: "GATEWAY_API_KEY", Usage: "environment variable containing the gateway API credential"},
		&cli.StringFlag{Local: true, Name: "enrolled-profile", Value: "", Usage: "enrolled gateway credential manifest"},
		&cli.StringFlag{Local: true, Name: "state", Value: "", Usage: "local worker journal (OS config directory by default in browser mode; --state= disables workers)"},
		&cli.StringFlag{Local: true, Name: "run-id", Value: "", Usage: "resume this saved worker run alongside the chat"},
		&cli.StringFlag{Local: true, Name: "task-tools-command", Value: "", Usage: "amux MCP executable (sibling or PATH amux by default for worker pools)"},
	}
}

func startDesktop(ctx context.Context, cmd *cli.Command) error {
	if err := noArguments(cmd); err != nil {
		return err
	}
	return desktopapp.Run(ctx, desktopapp.Options{
		ServerURL:              cmd.String("server"),
		Database:               cmd.String("database"),
		Project:                cmd.String("project"),
		ProjectDir:             cmd.String("project-dir"),
		ConversationID:         cmd.String("conversation"),
		Preferences:            cmd.String("preferences"),
		WorkPreferences:        cmd.String("work-preferences"),
		GatewayPreferencesPath: cmd.String("gateway-preferences"),
		GatewayStateDir:        cmd.String("gateway-state-dir"),
		KeyEnv:                 cmd.String("api-key-env"),
		EnrolledProfile:        cmd.String("enrolled-profile"),
		State:                  cmd.String("state"),
		RunID:                  cmd.String("run-id"),
		ToolsCommand:           cmd.String("task-tools-command"),
		ConfigPath:             cmd.String("config"),
		IsSet:                  cmd.IsSet,
	}, os.Args[1:])
}
