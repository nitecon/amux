package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/mcpstdio"
	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

var taskRoutingMCPTools = []mcpstdio.Tool{
	{Name: "agent_identity", Description: "Read this worker's current Amux name, hostname, OS, architecture, provider, session, and execution instance.", InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
	{Name: "task_agents", Description: "List live agent names, providers, and session IDs for this project. Optionally filter by OS.", InputSchema: json.RawMessage(`{"type":"object","properties":{"os":{"type":"string","enum":["linux","macos","windows"]}},"additionalProperties":false}`)},
	{Name: "task_reassign", Description: "Transfer this assigned task to another live agent in the project. Use task_agents to get the recipient session ID.", InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Assigned task UUID"},"recipient_session_id":{"type":"string","description":"Recipient session UUID from task_agents"}},"required":["id","recipient_session_id"],"additionalProperties":false}`)},
}

func callTaskRoutingMCP(ctx context.Context, client *gatewayclient.Client, project, conversationID, localTask, directory string, actor storage.AgentOrigin, name string, args json.RawMessage) mcpstdio.Result {
	if name == "agent_identity" {
		return localGatewayToolResult(json.Marshal(worker.CurrentAgentIdentity(actor)))
	}
	if name == "task_agents" {
		var request struct {
			OS string `json:"os"`
		}
		if err := json.Unmarshal(args, &request); err != nil {
			return localGatewayToolResult(nil, err)
		}
		checkout, err := projectidentity.ResolveCheckout(ctx, directory)
		if err != nil {
			return localGatewayToolResult(nil, err)
		}
		targets, err := client.TaskAgents(ctx, project, checkout.Upstream, request.OS)
		if err != nil {
			return localGatewayToolResult(nil, err)
		}
		return localGatewayToolResult(json.Marshal(targets))
	}
	var request struct {
		ID        string `json:"id"`
		Recipient string `json:"recipient_session_id"`
	}
	if err := json.Unmarshal(args, &request); err != nil {
		return localGatewayToolResult(nil, err)
	}
	if localTask == "" {
		chat, err := client.Conversation(ctx, project, conversationID)
		if err != nil {
			return localGatewayToolResult(nil, err)
		}
		if chat.Owner != actor {
			return localGatewayToolResult(nil, storage.ErrTaskOwnership)
		}
		localTask = chat.TaskID
	}
	if localTask == "" || request.ID != localTask {
		return localGatewayToolResult(nil, fmt.Errorf("reassignment must target the task bound to this conversation"))
	}
	if request.Recipient == actor.SessionID {
		return localGatewayToolResult(nil, fmt.Errorf("recipient must be a different agent session"))
	}
	task, err := client.ReassignTask(ctx, project, request.ID, request.Recipient, actor)
	if err != nil {
		return localGatewayToolResult(nil, err)
	}
	return localGatewayToolResult(json.Marshal(task))
}
