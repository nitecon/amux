package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/nitecon/amux/bundledtools"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/mcpstdio"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/startupguidance"
	"github.com/nitecon/amux/storage"
)

func runMCP(ctx context.Context, args []string, stdin io.ReadCloser, stdout, stderr io.Writer, getenv func(string) string) error {
	flags := flag.NewFlagSet("amux mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("server", "", "gateway HTTP(S) origin")
	enrolledProfile := flags.String("enrolled-profile", "", "enrolled gateway credential manifest")
	project := flags.String("project", "", "bound project identifier")
	conversationID := flags.String("conversation", "", "bound task conversation UUID")
	localTask := flags.String("local-task", "", "bound task UUID for a gateway without server-side conversations")
	localConversation := flags.Bool("local-conversation", false, "local conversation without a server-side conversation record")
	session := flags.String("session", "", "bound agent session UUID")
	instance := flags.String("instance", "", "bound agent instance UUID")
	provider := flags.String("provider", "", "bound native provider")
	platform := flags.String("os", "", "bound agent OS")
	goalFlag := flags.String("goal", "", "selected task goal for this conversation")
	directory := flags.String("directory", "", "explicit saved checkout directory for local work submission")
	workPreferences := flags.String("work-preferences", "", "work preference JSON path (default: user config directory)")
	keyEnv := flags.String("api-key-env", "GATEWAY_API_KEY", "environment variable containing gateway credential")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected MCP arguments")
	}
	nativeRequested := false
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "server", "enrolled-profile", "project", "conversation", "local-task", "local-conversation", "session", "instance", "provider", "os", "goal":
			nativeRequested = true
		}
	})
	workingDirectory := ""
	directoryWasSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "directory" {
			directoryWasSet = true
		}
	})
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
		if *goalFlag != "" || flagWasSet(flags, "goal") {
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
	executable, err := os.Executable()
	if err != nil {
		executable = ""
	}
	runner := bundledToolRunner{Executable: executable, OverrideDirectory: getenv("AMUX_BUNDLED_DIR"), Directory: workingDirectory}
	configuredWorkPreferences := getenv("AMUX_WORK_SUBMISSION_PREFERENCES")
	instructions := mcpStartupInstructionsFor(*provider, getenv("AMUX_AGENT_PROVIDER"), runner)
	listTools := func() []mcpstdio.Tool {
		tools := make([]mcpstdio.Tool, 0, len(bundledToolCatalog)+len(server.AgentTaskTools())+len(taskRoutingMCPTools)+1)
		if nativeRequested {
			if *localConversation {
				tools = append(tools, server.LocalConversationTaskTools()...)
			} else {
				for _, tool := range server.AgentTaskTools() {
					if *localTask != "" && !localGatewayTaskTool(tool.Name) {
						continue
					}
					tools = append(tools, tool)
				}
				tools = append(tools, submitWorkMCPTool())
			}
			for _, tool := range taskRoutingMCPTools {
				if *localConversation && tool.Name == "task_reassign" {
					continue
				}
				tools = append(tools, tool)
			}
		}
		return append(tools, runner.AvailableTools()...)
	}
	err = mcpstdio.Serve(ctx, stdin, stdout, mcpstdio.Config{ListTools: listTools, Instructions: instructions, Call: func(callCtx context.Context, name string, args json.RawMessage) (mcpstdio.Result, error) {
		if name == "submit_work" && nativeRequested && !*localConversation {
			preferencesPath, err := submissionPreferencesPath(*workPreferences, configuredWorkPreferences)
			if err != nil {
				return mcpstdio.Result{Content: []mcpstdio.TextContent{{Type: "text", Text: err.Error()}}, IsError: true}, nil
			}
			conversation := id.String()
			if *localTask != "" {
				if err := verifyLocalGatewayTaskWrite(callCtx, client, *project, *localTask, origin, args, "task_id"); err != nil {
					return localGatewayToolResult(nil, err), nil
				}
				conversation = ""
			}
			return callSubmitWorkMCP(callCtx, client, *project, conversation, *directory, origin, selectedGoal, preferencesPath, args)
		}
		if nativeRequested {
			if name == "agent_identity" || name == "task_agents" || (!*localConversation && name == "task_reassign") {
				return callTaskRoutingMCP(callCtx, client, *project, id.String(), *localTask, workingDirectory, origin, name, args), nil
			}
			if *localTask != "" && localGatewayTaskTool(name) {
				return callLocalGatewayTaskTool(callCtx, client, *project, *localTask, origin, selectedGoal, name, args), nil
			}
			if *localConversation && localConversationTaskTool(name) {
				return callLocalConversationTaskTool(callCtx, client, *project, origin, selectedGoal, name, args), nil
			}
			for _, tool := range server.AgentTaskTools() {
				if *localTask != "" || *localConversation || name != tool.Name {
					continue
				}
				if name == "task_delegate" {
					if err := client.RequireTaskHierarchy(callCtx); err != nil {
						return mcpstdio.Result{Content: []mcpstdio.TextContent{{Type: "text", Text: err.Error()}}, IsError: true}, nil
					}
				}
				return client.CallTaskToolWithGoal(callCtx, *project, id.String(), origin, selectedGoal, name, args)
			}
		}
		return runner.Call(callCtx, name, args)
	}})
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}

func mcpStartupInstructions(provider string, runner bundledToolRunner) string {
	if provider == "codex" {
		return ""
	}
	if _, supplied := os.LookupEnv("AMUX_DESKTOP_GUIDANCE"); supplied {
		return ""
	}
	return nativeMCPStartupInstructions(runner)
}

func mcpStartupInstructionsFor(explicitProvider, environmentProvider string, runner bundledToolRunner) string {
	provider := explicitProvider
	if provider == "" {
		provider = environmentProvider
	}
	return mcpStartupInstructions(provider, runner)
}

func nativeMCPStartupInstructions(runner bundledToolRunner) string {
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
	return startupguidance.Context(codeTools, memoryTools, memories)
}
