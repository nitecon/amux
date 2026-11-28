package providers

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/providers/acp"
	"github.com/nitecon/amux/providers/claude"
	"github.com/nitecon/amux/providers/codex"
)

func antigravityCommand(prefs Preferences) ([]string, error) {
	if prefs != (Preferences{}) {
		return nil, fmt.Errorf("this ACP provider does not expose model or persona preferences")
	}
	return nil, nil
}

func claudeCommand(prefs Preferences) ([]string, error) {
	command := []string{"claude"}
	if prefs.Model != "" {
		command = append(command, "--model", prefs.Model)
	}
	if prefs.Persona != "" {
		command = append(command, "--agent", prefs.Persona)
	}
	if prefs.Effort != "" {
		command = append(command, "--effort", prefs.Effort)
	}
	return command, nil
}

func codexCommand(prefs Preferences) ([]string, error) {
	if prefs.Persona != "" {
		return nil, fmt.Errorf("Codex app-server does not expose an agent persona option")
	}
	command := []string{"codex", "app-server", "--listen", "stdio://"}
	if prefs.Model != "" {
		command = append(command, "--config", "model="+strconv.Quote(prefs.Model))
	}
	if prefs.Effort != "" {
		command = append(command, "--config", "model_reasoning_effort="+strconv.Quote(prefs.Effort))
	}
	return command, nil
}

func startAntigravity(ctx context.Context, options StartOptions, sink conversation.Sink) (Session, error) {
	config := acp.Config{
		Command: options.Command, Directory: options.Directory, ResumeID: options.ResumeID, Env: options.Env,
	}
	if mcp := options.MCP; mcp != nil {
		env := make([]acp.EnvVar, 0, len(mcp.Env))
		for _, name := range slices.Sorted(maps.Keys(mcp.Env)) {
			env = append(env, acp.EnvVar{Name: name, Value: mcp.Env[name]})
		}
		config.MCPServers = []acp.MCPServer{{Name: "amux", Command: mcp.Command, Args: mcp.Args, Env: env}}
		config.AutoApproveTool = antigravityAmuxTool
	}
	session, err := acp.Start(ctx, config, sink)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func startClaude(ctx context.Context, options StartOptions, sink conversation.Sink) (Session, error) {
	session, err := claude.Start(ctx, claude.Config{
		Command: options.Command, Directory: options.Directory, ResumeID: options.ResumeID, Env: options.Env, MCP: options.MCP,
	}, sink)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func startCodex(ctx context.Context, options StartOptions, sink conversation.Sink) (Session, error) {
	config := codex.Config{
		Command: options.Command, Directory: options.Directory, ResumeID: options.ResumeID, Env: options.Env, MCP: options.MCP,
	}
	if options.MCP != nil {
		config.DeveloperInstructions = codex.DeveloperInstructions(options.Directory)
	}
	session, err := codex.Start(ctx, config, sink)
	if err != nil {
		return nil, err
	}
	return session, nil
}
