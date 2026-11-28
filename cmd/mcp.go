package main

import (
	"context"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/mcp"
	"github.com/urfave/cli/v3"
)

// newMCPCommand defines the managed MCP subprocess arguments.
func newMCPCommand(transport sdk.Transport) *cli.Command {
	return &cli.Command{
		Name:  "mcp",
		Usage: "Run the managed Amux MCP server",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "project", Usage: "bound project identifier"},
			&cli.StringFlag{Name: "conversation", Usage: "bound conversation UUID"},
			&cli.StringFlag{Name: "local-task", Usage: "bound task UUID for a gateway without server-side conversations"},
			&cli.BoolFlag{Name: "local-conversation", Usage: "local conversation without a server-side conversation record"},
			&cli.StringFlag{Name: "session", Usage: "bound agent session UUID"},
			&cli.StringFlag{Name: "instance", Usage: "bound agent instance UUID"},
			&cli.StringFlag{Name: "provider", Usage: "bound native provider"},
			&cli.StringFlag{Name: "os", Usage: "bound agent OS"},
			&cli.StringFlag{Name: "goal", Usage: "selected task goal for this conversation"},
			&cli.StringFlag{Name: "directory", Usage: "saved checkout directory"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() != 0 {
				return fmt.Errorf("unexpected MCP arguments")
			}
			binding := mcp.Binding{
				Project: cmd.String("project"), ConversationID: cmd.String("conversation"),
				LocalTask: cmd.String("local-task"), LocalConversation: cmd.Bool("local-conversation"),
				Origin: agent.Origin{SessionID: cmd.String("session"), InstanceID: cmd.String("instance"),
					Provider: cmd.String("provider"), OS: cmd.String("os")},
			}
			for _, name := range []string{"project", "conversation", "local-task", "local-conversation", "session", "instance", "provider", "os", "goal"} {
				binding.Native = binding.Native || cmd.IsSet(name)
			}
			if cmd.IsSet("directory") {
				directory := cmd.String("directory")
				binding.Directory = &directory
			}
			if cmd.IsSet("goal") {
				goal := cmd.String("goal")
				binding.Goal = &goal
			}
			return mcp.Start(ctx, transport, binding)
		},
	}
}
