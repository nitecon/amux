package main

import (
	"context"
	"os"

	"github.com/nitecon/amux/internal/amuxruntime"
	"github.com/urfave/cli/v3"
)

func serverFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "database", Value: "", Usage: "durable SQLite database path (required)"},
		&cli.StringFlag{Name: "profile", Value: "", Usage: "explicit Rust-compatible server profile directory"},
		&cli.StringFlag{Name: "secure-instance-id", Value: "", Usage: "enable enrolled-certificate TLS using this stable local administrator instance UUID"},
		&cli.StringFlag{Name: "discovery-bind", Value: "", Usage: "public enrollment discovery address (defaults to secure listen host on port 24181)"},
		&cli.StringSliceFlag{Name: "profile-tls-name", Value: nil, Usage: "DNS or IP name for the profile-managed TLS certificate (repeatable)"},
		&cli.StringFlag{Name: "listen", Value: "127.0.0.1:7913", Usage: "TCP address to listen on"},
		&cli.StringFlag{Name: "api-key-env", Value: "GATEWAY_API_KEY", Usage: "environment variable containing the server API credential"},
		&cli.StringFlag{Name: "tls-cert", Value: "", Usage: "TLS server certificate PEM file"},
		&cli.StringFlag{Name: "tls-key", Value: "", Usage: "TLS server private key PEM file"},
		&cli.Uint64Flag{Name: "message-retention-days", Value: 30, Usage: "message history retention in days"},
		&cli.Uint64Flag{Name: "bot-message-retention-days", Value: 7, Usage: "bot message history retention in days"},
		&cli.StringFlag{Name: "discord-token-env", Value: "", Usage: "enable Discord messaging and inbound room ingestion using the token in this environment variable"},
		&cli.StringFlag{Name: "discord-guild-id", Value: "", Usage: "Discord guild ID (required with --discord-token-env)"},
		&cli.StringFlag{Name: "discord-category-id", Value: "", Usage: "optional Discord category ID for newly created rooms"},
	}
}

func startServer(ctx context.Context, cmd *cli.Command) error {
	if err := noArguments(cmd); err != nil {
		return err
	}
	getenv, err := amuxruntime.LoadServerConfig(cmd, cmd.String("config"), os.Getenv)
	if err != nil {
		return err
	}
	return amuxruntime.Serve(ctx, amuxruntime.ServerOptions{
		Database:          cmd.String("database"),
		Profile:           cmd.String("profile"),
		SecureInstance:    cmd.String("secure-instance-id"),
		DiscoveryBind:     cmd.String("discovery-bind"),
		ProfileTLSNames:   cmd.StringSlice("profile-tls-name"),
		Listen:            cmd.String("listen"),
		KeyEnv:            cmd.String("api-key-env"),
		CertPath:          cmd.String("tls-cert"),
		KeyPath:           cmd.String("tls-key"),
		RetentionDays:     cmd.Uint64("message-retention-days"),
		BotRetentionDays:  cmd.Uint64("bot-message-retention-days"),
		DiscordTokenEnv:   cmd.String("discord-token-env"),
		DiscordGuildID:    cmd.String("discord-guild-id"),
		DiscordCategoryID: cmd.String("discord-category-id"),
	}, cmd.Writer, cmd.ErrWriter, getenv)
}
