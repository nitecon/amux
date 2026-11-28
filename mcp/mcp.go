package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nitecon/amux/bundledtools"
	"github.com/nitecon/amux/config"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/startupguidance"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worksubmission"
)

// Binding identifies the worker session served by this MCP process.
type Binding struct {
	Native            bool
	Project           string
	ConversationID    string
	LocalTask         string
	LocalConversation bool
	Origin            agent.Origin
	Goal              *string
	Directory         *string
}

// Start serves the bound session over the official MCP transport.
func Start(ctx context.Context, transport sdk.Transport, binding Binding) error {
	workingDirectory := ""
	if binding.Directory != nil {
		if *binding.Directory == "" {
			return fmt.Errorf("--directory must name an existing directory")
		}
		absolute, err := filepath.Abs(*binding.Directory)
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
		origin       agent.Origin
		selectedGoal *string
	)
	if binding.Native {
		if binding.Project == "" || binding.ConversationID == "" || binding.Origin.SessionID == "" || binding.Origin.InstanceID == "" || binding.Origin.Provider == "" || binding.Origin.OS == "" {
			return fmt.Errorf("native MCP requires gateway configuration and a complete project, conversation, and agent binding")
		}
		var err error
		id, err = uuid.Parse(binding.ConversationID)
		if err != nil || id == uuid.Nil {
			return fmt.Errorf("MCP requires a nonzero conversation UUID")
		}
		if binding.Goal != nil {
			normalized, err := storage.NormalizeTaskGoal(binding.Goal)
			if err != nil {
				return err
			}
			value := ""
			if normalized != nil {
				value = *normalized
			}
			selectedGoal = &value
		}
		normalized, err := agent.NormalizeOrigin(&binding.Origin)
		if err != nil {
			return err
		}
		origin = *normalized
	}
	path, err := config.DefaultClientPath()
	if err != nil {
		return err
	}
	if err := config.LoadClient(path); err != nil {
		return fmt.Errorf("load MCP client connectivity: %w", err)
	}
	settings := config.Get()
	if binding.Native {
		client, _, err = gatewayclient.NewForProfile(settings.GatewayEndpoint, settings.EnrolledProfile, os.Getenv(settings.APIKeyEnv))
		if err != nil {
			return err
		}
	}
	if binding.LocalTask != "" {
		if binding.LocalConversation {
			return fmt.Errorf("--local-conversation cannot be combined with --local-task")
		}
		if _, err := storage.CanonicalTaskID(binding.LocalTask); err != nil {
			return err
		}
	}
	directory := ""
	if binding.Directory != nil {
		directory = *binding.Directory
	}
	catalog := mcpTools(client, binding.Project, id.String(), binding.LocalTask, directory, workingDirectory, settings.WorkPreferences, origin, selectedGoal, binding.LocalConversation)
	server := sdk.NewServer(&sdk.Implementation{Name: "amux", Version: "1.0.0"}, &sdk.ServerOptions{Instructions: catalog.instructions})
	for _, tool := range catalog.tools {
		server.AddTool(&sdk.Tool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			args := req.Params.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			result, err := catalog.call(ctx, tool.Name, args)
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
	err = server.Run(ctx, transport)
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}

type toolCatalog struct {
	tools        []storage.TaskTool
	instructions string
	call         func(context.Context, string, json.RawMessage) (storage.ToolResult, error)
}

func mcpTools(client *gatewayclient.Client, project, conversationID, localTask, directory, workingDirectory, workPreferences string, origin agent.Origin, selectedGoal *string, localConversation bool) toolCatalog {
	nativeRequested := client != nil
	executable, err := os.Executable()
	if err != nil {
		executable = ""
	}
	runner := bundledToolRunner{Executable: executable, OverrideDirectory: os.Getenv("AMUX_BUNDLED_DIR"), Directory: workingDirectory}
	configuredWorkPreferences := os.Getenv("AMUX_WORK_SUBMISSION_PREFERENCES")
	instructions := mcpStartupInstructionsFor(origin.Provider, os.Getenv("AMUX_AGENT_PROVIDER"), runner, localConversation)
	tools := make([]storage.TaskTool, 0, len(bundledToolCatalog)+len(server.AgentTaskTools())+len(taskRoutingMCPTools)+1)
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
	tools = append(tools, runner.AvailableTools()...)
	return toolCatalog{tools: tools, instructions: instructions, call: func(callCtx context.Context, name string, args json.RawMessage) (storage.ToolResult, error) {
		if name == "submit_work" && nativeRequested && !localConversation {
			preferencesPath, err := worksubmission.OptionalPreferencesPath(workPreferences, configuredWorkPreferences)
			if err != nil {
				return storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: err.Error()}}, IsError: true}, nil
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
						return storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: err.Error()}}, IsError: true}, nil
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
