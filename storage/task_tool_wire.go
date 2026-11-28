package storage

import "encoding/json"

// TaskTool is an MCP tool definition.
type TaskTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolText is a textual MCP tool result item.
type ToolText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolResult is the result of executing an MCP tool.
type ToolResult struct {
	Content []ToolText `json:"content"`
	IsError bool       `json:"isError,omitempty"`
}
