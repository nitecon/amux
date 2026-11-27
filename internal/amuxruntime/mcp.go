package amuxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nitecon/amux/bundledtools"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/mcpstdio"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/startupguidance"
	"github.com/nitecon/amux/storage"
	"github.com/urfave/cli/v3"
)

const ManagedMCPEnv = "AMUX_INTERNAL_MANAGED_MCP"

func ServeManagedMCP(ctx context.Context, args []string) error {
	return runMCPWithTransport(ctx, args, os.Stderr, os.Getenv, func(ctx context.Context, config mcpstdio.Config) error {
		server := sdk.NewServer(&sdk.Implementation{Name: "amux", Version: "1.0.0"}, &sdk.ServerOptions{Instructions: config.Instructions})
		for _, tool := range config.ListTools() {
			server.AddTool(&sdk.Tool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				args := req.Params.Arguments
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				result, err := config.Call(ctx, tool.Name, args)
				output := &sdk.CallToolResult{IsError: result.IsError, Content: make([]sdk.Content, 0, len(result.Content))}
				for _, item := range result.Content {
					output.Content = append(output.Content, &sdk.TextContent{Text: item.Text})
				}
				if err != nil {
					output.SetError(err)
				}
				return output, nil
			})
		}
		return server.Run(ctx, &sdk.StdioTransport{})
	})
}

func runMCP(ctx context.Context, args []string, stdin io.ReadCloser, stdout, stderr io.Writer, getenv func(string) string) error {
	return runMCPWithTransport(ctx, args, stderr, getenv, func(ctx context.Context, config mcpstdio.Config) error {
		return mcpstdio.Serve(ctx, stdin, stdout, config)
	})
}

func runMCPWithTransport(ctx context.Context, args []string, stderr io.Writer, getenv func(string) string, serve func(context.Context, mcpstdio.Config) error) error {
	flags := commandFlags("amux mcp", stderr)
	endpoint := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "server", Value: "", Usage: "gateway HTTP(S) origin", Destination: endpoint})
	enrolledProfile := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "enrolled-profile", Value: "", Usage: "enrolled gateway credential manifest", Destination: enrolledProfile})
	project := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "project", Value: "", Usage: "bound project identifier", Destination: project})
	conversationID := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "conversation", Value: "", Usage: "bound task conversation UUID", Destination: conversationID})
	localTask := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "local-task", Value: "", Usage: "bound task UUID for a gateway without server-side conversations", Destination: localTask})
	localConversation := new(bool)
	flags.Flags = append(flags.Flags, &cli.BoolFlag{Name: "local-conversation", Value: false, Usage: "local conversation without a server-side conversation record", Destination: localConversation})
	session := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "session", Value: "", Usage: "bound agent session UUID", Destination: session})
	instance := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "instance", Value: "", Usage: "bound agent instance UUID", Destination: instance})
	provider := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "provider", Value: "", Usage: "bound native provider", Destination: provider})
	platform := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "os", Value: "", Usage: "bound agent OS", Destination: platform})
	goalFlag := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "goal", Value: "", Usage: "selected task goal for this conversation", Destination: goalFlag})
	directory := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "directory", Value: "", Usage: "explicit saved checkout directory for local work submission", Destination: directory})
	workPreferences := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "work-preferences", Value: "", Usage: "work preference JSON path (default: user config directory)", Destination: workPreferences})
	keyEnv := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "api-key-env", Value: "GATEWAY_API_KEY", Usage: "environment variable containing gateway credential", Destination: keyEnv})
	if err := parseCommand(ctx, flags, args); err != nil {
		if errors.Is(err, errCommandHelp) {
			return nil
		}
		return err
	}
	if flags.Args().Len() != 0 {
		return fmt.Errorf("unexpected MCP arguments")
	}
	nativeRequested := false
	for _, name := range []string{"server", "enrolled-profile", "project", "conversation", "local-task", "local-conversation", "session", "instance", "provider", "os", "goal"} {
		nativeRequested = nativeRequested || flags.IsSet(name)
	}
	workingDirectory := ""
	directoryWasSet := flags.IsSet("directory")
	if directoryWasSet {
		if *directory == "" {
			return fmt.Errorf("--directory must name an existing directory")
		}
		absolute, err := filepath.Abs(*directory)
		if err != nil {
			return fmt.Errorf("resolve bundled-tool directory: %w", err)
		}
		info, err := os.Stat(absolute)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("--directory must name an existing directory")
		}
		workingDirectory = absolute
	}
	var (
		client       *gatewayclient.Client
		id           uuid.UUID
		origin       storage.AgentOrigin
		selectedGoal *string
	)
	if nativeRequested {
		if (*endpoint == "" && *enrolledProfile == "") || *project == "" || *conversationID == "" || *session == "" || *instance == "" || *provider == "" || *platform == "" {
			return fmt.Errorf("native MCP requires --server or --enrolled-profile, --project, --conversation, --session, --instance, --provider, and --os")
		}
		var err error
		id, err = uuid.Parse(*conversationID)
		if err != nil || id == uuid.Nil {
			return fmt.Errorf("MCP requires a nonzero conversation UUID")
		}
		if *goalFlag != "" || flags.IsSet("goal") {
			normalized, err := storage.NormalizeTaskGoal(goalFlag)
			if err != nil {
				return err
			}
			value := ""
			if normalized != nil {
				value = *normalized
			}
			selectedGoal = &value
		}
		normalized, err := storage.NormalizeOrigin(&storage.AgentOrigin{SessionID: *session, InstanceID: *instance, Provider: *provider, OS: *platform})
		if err != nil {
			return err
		}
		origin = *normalized
		client, *endpoint, err = gatewayClientForProfile(*endpoint, *enrolledProfile, getenv(*keyEnv))
		if err != nil {
			return err
		}
	}
	if *localTask != "" {
		if *localConversation {
			return fmt.Errorf("--local-conversation cannot be combined with --local-task")
		}
		if _, err := canonicalTaskID(*localTask); err != nil {
			return err
		}
	}
	config := mcpTools(client, *project, id.String(), *localTask, *directory, workingDirectory, *workPreferences, origin, selectedGoal, *localConversation, getenv)
	err := serve(ctx, config)
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}

func mcpTools(client *gatewayclient.Client, project, conversationID, localTask, directory, workingDirectory, workPreferences string, origin storage.AgentOrigin, selectedGoal *string, localConversation bool, getenv func(string) string) mcpstdio.Config {
	nativeRequested := client != nil
	executable, err := os.Executable()
	if err != nil {
		executable = ""
	}
	runner := bundledToolRunner{Executable: executable, OverrideDirectory: getenv("AMUX_BUNDLED_DIR"), Directory: workingDirectory}
	configuredWorkPreferences := getenv("AMUX_WORK_SUBMISSION_PREFERENCES")
	instructions := mcpStartupInstructionsFor(origin.Provider, getenv("AMUX_AGENT_PROVIDER"), runner, localConversation)
	listTools := func() []mcpstdio.Tool {
		tools := make([]mcpstdio.Tool, 0, len(bundledToolCatalog)+len(server.AgentTaskTools())+len(taskRoutingMCPTools)+1)
		if nativeRequested {
			if localConversation {
				tools = append(tools, server.LocalConversationTaskTools()...)
			} else {
				for _, tool := range server.AgentTaskTools() {
					if localTask != "" && !localGatewayTaskTool(tool.Name) {
						continue
					}
					tools = append(tools, tool)
				}
				tools = append(tools, submitWorkMCPTool())
			}
			for _, tool := range taskRoutingMCPTools {
				if localConversation && tool.Name == "task_reassign" {
					continue
				}
				tools = append(tools, tool)
			}
		}
		return append(tools, runner.AvailableTools()...)
	}
	return mcpstdio.Config{ListTools: listTools, Instructions: instructions, Call: func(callCtx context.Context, name string, args json.RawMessage) (mcpstdio.Result, error) {
		if name == "submit_work" && nativeRequested && !localConversation {
			preferencesPath, err := submissionPreferencesPath(workPreferences, configuredWorkPreferences)
			if err != nil {
				return mcpstdio.Result{Content: []mcpstdio.TextContent{{Type: "text", Text: err.Error()}}, IsError: true}, nil
			}
			conversation := conversationID
			if localTask != "" {
				if err := verifyLocalGatewayTaskWrite(callCtx, client, project, localTask, origin, args, "task_id"); err != nil {
					return localGatewayToolResult(nil, err), nil
				}
				conversation = ""
			}
			return callSubmitWorkMCP(callCtx, client, project, conversation, directory, origin, selectedGoal, preferencesPath, args)
		}
		if nativeRequested {
			if name == "agent_identity" || name == "task_agents" || (!localConversation && name == "task_reassign") {
				return callTaskRoutingMCP(callCtx, client, project, conversationID, localTask, workingDirectory, origin, name, args), nil
			}
			if localTask != "" && localGatewayTaskTool(name) {
				return callLocalGatewayTaskTool(callCtx, client, project, localTask, origin, selectedGoal, name, args), nil
			}
			if localConversation && localConversationTaskTool(name) {
				return callLocalConversationTaskTool(callCtx, client, project, origin, selectedGoal, name, args), nil
			}
			for _, tool := range server.AgentTaskTools() {
				if localTask != "" || localConversation || name != tool.Name {
					continue
				}
				if name == "task_delegate" {
					if err := client.RequireTaskHierarchy(callCtx); err != nil {
						return mcpstdio.Result{Content: []mcpstdio.TextContent{{Type: "text", Text: err.Error()}}, IsError: true}, nil
					}
				}
				return client.CallTaskToolWithGoal(callCtx, project, conversationID, origin, selectedGoal, name, args)
			}
		}
		return runner.Call(callCtx, name, args)
	}}
}

func mcpStartupInstructionsFor(explicitProvider, environmentProvider string, runner bundledToolRunner, localConversation bool) string {
	provider := explicitProvider
	if provider == "" {
		provider = environmentProvider
	}
	if provider == "codex" {
		return ""
	}
	if _, supplied := os.LookupEnv("AMUX_DESKTOP_GUIDANCE"); supplied {
		return ""
	}
	return nativeMCPStartupInstructions(runner, localConversation)
}

func nativeMCPStartupInstructions(runner bundledToolRunner, localConversation bool) string {
	query, ok := startupguidance.MemoryQuery(runner.workingDirectory())
	if !ok {
		return ""
	}
	codeTools, memoryTools := false, false
	for _, tool := range runner.AvailableTools() {
		codeTools = codeTools || tool.Name == "code_symbols"
		memoryTools = memoryTools || tool.Name == "memory_search"
	}
	memories := bundledtools.DurableMemories(runner.workingDirectory(), query)
	if localConversation {
		return startupguidance.LocalConversationContext(codeTools, memoryTools, memories)
	}
	return startupguidance.Context(codeTools, memoryTools, memories)
}
