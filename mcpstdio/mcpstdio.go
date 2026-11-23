// Package mcpstdio implements a newline-delimited MCP stdio server with bounded inbound frames.
// It supports protocol revisions through 2025-11-25; newer modern revisions
// remain pending and are not advertised as supported.
//
// Serve owns neither stream during normal operation. If ctx is canceled, it
// closes in and out when they implement io.Closer, unblocking pending reads or
// writes. Plain readers and writers cannot be interrupted while blocked; callers
// using them must arrange for their operations to return.
package mcpstdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	maxFrameBytes = 4 << 20
	latestVersion = "2025-11-25"
)

var supportedVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18", latestVersion}

// Tool is an MCP tool definition.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// TextContent is a textual MCP tool result item.
type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Result is the result of executing an MCP tool.
type Result struct {
	Content []TextContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// Config supplies the tool catalog and execution callback.
type Config struct {
	Tools        []Tool
	ListTools    func() []Tool
	Instructions string
	Call         func(context.Context, string, json.RawMessage) (Result, error)
}

// Serve handles newline-delimited JSON-RPC messages until EOF or cancellation.
// Requests are handled serially, and tool calls are never automatically retried.
func Serve(ctx context.Context, in io.Reader, out io.Writer, config Config) error {
	if ctx == nil {
		return errors.New("mcpstdio: nil context")
	}
	if in == nil || out == nil {
		return errors.New("mcpstdio: input and output are required")
	}
	if config.ListTools == nil {
		if err := validateTools(config.Tools); err != nil {
			return err
		}
	} else if config.Call == nil {
		return errors.New("mcpstdio: dynamic tools require a callback")
	}
	inputCloser, canCloseInput := in.(io.Closer)
	outputCloser, canCloseOutput := out.(io.Closer)
	var stopWatcher, watcherDone chan struct{}
	if canCloseInput || canCloseOutput {
		stopWatcher = make(chan struct{})
		watcherDone = make(chan struct{})
		go func() {
			defer close(watcherDone)
			select {
			case <-ctx.Done():
				if canCloseInput {
					_ = inputCloser.Close()
				}
				if canCloseOutput {
					_ = outputCloser.Close()
				}
			case <-stopWatcher:
			}
		}()
		defer func() {
			close(stopWatcher)
			<-watcherDone
		}()
	}

	reader := bufio.NewReader(in)
	state := sessionState{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		frame, oversized, err := readFrame(reader)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if oversized {
			if err := writeMessage(out, errorResponse(nil, -32700, "frame exceeds 4 MiB")); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
			continue
		}
		response, hasResponse := state.handle(ctx, frame, config)
		if hasResponse {
			if err := writeMessage(out, response); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
		}
	}
}

type sessionState struct {
	initialized bool
	ready       bool
}

type request struct {
	ID        json.RawMessage
	HasID     bool
	Method    string
	Params    json.RawMessage
	HasParams bool
}

func (s *sessionState) handle(ctx context.Context, frame []byte, config Config) (any, bool) {
	message, parseErr := parseRequest(frame)
	if parseErr != nil {
		if errors.Is(parseErr, errInvalidRequest) {
			return errorResponse(nil, -32600, "invalid request"), true
		}
		return errorResponse(nil, -32700, "parse error"), true
	}
	if message == nil {
		return errorResponse(nil, -32600, "invalid request"), true
	}
	if message.HasID && !validID(message.ID) {
		return errorResponse(nil, -32600, "invalid request id"), true
	}
	if !message.HasID {
		if message.Method == "notifications/initialized" && s.initialized {
			if !message.HasParams {
				s.ready = true
			} else if _, ok := objectParams(message); ok {
				s.ready = true
			}
		}
		return nil, false
	}

	switch message.Method {
	case "initialize":
		if s.initialized {
			return errorResponse(message.ID, -32600, "initialize already completed"), true
		}
		params, ok := objectParams(message)
		if !ok {
			return errorResponse(message.ID, -32602, "initialize params must be an object"), true
		}
		versionRaw, ok := params["protocolVersion"]
		var version string
		if !ok || json.Unmarshal(versionRaw, &version) != nil || version == "" {
			return errorResponse(message.ID, -32602, "protocolVersion must be a string"), true
		}
		negotiated := version
		if !contains(supportedVersions, version) {
			negotiated = latestVersion
		}
		s.initialized = true
		result := map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "amux", "version": "0.0.0"},
		}
		if config.Instructions != "" {
			result["instructions"] = config.Instructions
		}
		return successResponse(message.ID, result), true
	case "ping":
		return successResponse(message.ID, map[string]any{}), true
	case "tools/list":
		if !s.ready {
			return errorResponse(message.ID, -32002, "server not initialized"), true
		}
		tools := configuredTools(config)
		if err := validateTools(tools); err != nil {
			return errorResponse(message.ID, -32603, "tool catalog is invalid"), true
		}
		return successResponse(message.ID, map[string]any{"tools": tools}), true
	case "tools/call":
		if !s.ready {
			return errorResponse(message.ID, -32002, "server not initialized"), true
		}
		return s.call(ctx, message, config), true
	default:
		return errorResponse(message.ID, -32601, "method not found: "+message.Method), true
	}
}

func (s *sessionState) call(ctx context.Context, message *request, config Config) any {
	params, ok := objectParams(message)
	if !ok {
		return errorResponse(message.ID, -32602, "tools/call params must be an object")
	}
	var name string
	if raw, exists := params["name"]; !exists || json.Unmarshal(raw, &name) != nil || name == "" {
		return errorResponse(message.ID, -32602, "tools/call requires a string name")
	}
	arguments, exists := params["arguments"]
	if !exists {
		arguments = json.RawMessage(`{}`)
	} else if !isJSONObject(arguments) {
		return errorResponse(message.ID, -32602, "tools/call arguments must be an object")
	}
	tools := configuredTools(config)
	if err := validateTools(tools); err != nil {
		return errorResponse(message.ID, -32603, "tool catalog is invalid")
	}
	found := false
	for _, tool := range tools {
		if tool.Name == name {
			found = true
			break
		}
	}
	if !found {
		return errorResponse(message.ID, -32602, "unknown tool: "+name)
	}
	if config.Call == nil {
		return successResponse(message.ID, Result{Content: []TextContent{{Type: "text", Text: "tool callback is not configured"}}, IsError: true})
	}
	result, err := config.Call(ctx, name, append(json.RawMessage(nil), arguments...))
	if ctx.Err() != nil {
		return errorResponse(message.ID, -32603, "tool call canceled")
	}
	if err != nil {
		result.IsError = true
		if len(result.Content) == 0 {
			result.Content = []TextContent{{Type: "text", Text: err.Error()}}
		}
	}
	return successResponse(message.ID, result)
}

func configuredTools(config Config) []Tool {
	if config.ListTools != nil {
		tools := config.ListTools()
		if tools == nil {
			return []Tool{}
		}
		return tools
	}
	tools := make([]Tool, len(config.Tools))
	copy(tools, config.Tools)
	return tools
}

func parseRequest(frame []byte) (*request, error) {
	if !utf8.Valid(frame) {
		return nil, errors.New("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(frame))
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON data")
	}
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errInvalidRequest
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil || fields == nil {
		return nil, errInvalidRequest
	}
	var version, method string
	if json.Unmarshal(fields["jsonrpc"], &version) != nil || version != "2.0" ||
		json.Unmarshal(fields["method"], &method) != nil || method == "" ||
		fields["result"] != nil || fields["error"] != nil {
		return nil, errInvalidRequest
	}
	if params, exists := fields["params"]; exists {
		trimmedParams := bytes.TrimSpace(params)
		if len(trimmedParams) == 0 || (trimmedParams[0] != '{' && trimmedParams[0] != '[') {
			return nil, errInvalidRequest
		}
	}
	id, hasID := fields["id"]
	params, hasParams := fields["params"]
	return &request{ID: id, HasID: hasID, Method: method, Params: params, HasParams: hasParams}, nil
}

func validID(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	if trimmed[0] == '"' {
		var value string
		return json.Unmarshal(trimmed, &value) == nil
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	return decoder.Decode(&number) == nil && number.String() != ""
}

func objectParams(message *request) (map[string]json.RawMessage, bool) {
	if !message.HasParams || !isJSONObject(message.Params) {
		return nil, false
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(message.Params, &params) != nil || params == nil {
		return nil, false
	}
	return params, true
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func validateTools(tools []Tool) error {
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) == "" {
			return errors.New("mcpstdio: tool name is required")
		}
		if _, exists := seen[tool.Name]; exists {
			return fmt.Errorf("mcpstdio: duplicate tool name %q", tool.Name)
		}
		seen[tool.Name] = struct{}{}
		if len(tool.InputSchema) == 0 || !json.Valid(tool.InputSchema) || !isJSONObject(tool.InputSchema) {
			return fmt.Errorf("mcpstdio: tool %q input schema must be a JSON object", tool.Name)
		}
	}
	return nil
}

func readFrame(reader *bufio.Reader) ([]byte, bool, error) {
	frame := make([]byte, 0, 4096)
	oversized := false
	for {
		part, err := reader.ReadSlice('\n')
		if !oversized {
			content := part
			if len(content) > 0 && content[len(content)-1] == '\n' {
				content = content[:len(content)-1]
			}
			if len(frame)+len(content) > maxFrameBytes {
				oversized = true
				frame = nil
			} else {
				frame = append(frame, content...)
			}
		}
		if err == nil {
			return frame, oversized, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(part) == 0 && len(frame) == 0 && !oversized {
				return nil, false, io.EOF
			}
			return frame, oversized, nil
		}
		return nil, false, err
	}
}

func writeMessage(out io.Writer, message any) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	for len(encoded) > 0 {
		n, err := out.Write(encoded)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		encoded = encoded[n:]
	}
	return nil
}

var errInvalidRequest = errors.New("invalid JSON-RPC request")

func successResponse(id json.RawMessage, result any) any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func errorResponse(id json.RawMessage, code int, message string) any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
