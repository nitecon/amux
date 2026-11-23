package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/nitecon/amux/mcpstdio"
)

type bundledArgKind uint8

const (
	bundledPositional bundledArgKind = iota
	bundledWords
	bundledOption
)

type bundledArg struct {
	Name, Description, Flag string
	Required                bool
	Kind                    bundledArgKind
}

type bundledToolSpec struct {
	Name, Description, Binary string
	Command                   []string
	Args                      []bundledArg
}

var bundledToolCatalog = []bundledToolSpec{
	{
		Name: "memory_search", Description: "Search stored memories (project and global) by meaning and keywords.", Binary: "memory", Command: []string{"search"},
		Args: []bundledArg{{Name: "query", Description: "Search query", Required: true}, {Name: "limit", Description: "Number of results (default 10)", Flag: "--limit", Kind: bundledOption}},
	},
	{
		Name: "memory_store", Description: "Store a durable memory for this project, or globally with scope global.", Binary: "memory", Command: []string{"store"},
		Args: []bundledArg{{Name: "content", Description: "Memory content", Required: true}, {Name: "memory_type", Description: "user, feedback, project or reference (default user)", Flag: "--memory-type", Kind: bundledOption}, {Name: "tags", Description: "Comma-separated tags", Flag: "--tags", Kind: bundledOption}, {Name: "scope", Description: "project (default) or global", Flag: "--scope", Kind: bundledOption}},
	},
	{
		Name: "memory_get", Description: "Fetch the full content of memories by ID.", Binary: "memory", Command: []string{"get"},
		Args: []bundledArg{{Name: "ids", Description: "Space-separated memory IDs or ID prefixes", Required: true, Kind: bundledWords}},
	},
	{
		Name: "memory_update", Description: "Update an existing durable memory by ID after inspecting it; preserve unrelated guidance.", Binary: "memory", Command: []string{"update"},
		Args: []bundledArg{{Name: "id", Description: "Existing memory ID or ID prefix", Required: true}, {Name: "content", Description: "Replacement memory content", Flag: "--content", Required: true, Kind: bundledOption}, {Name: "tags", Description: "Optional replacement comma-separated tags", Flag: "--tags", Kind: bundledOption}},
	},
	{
		Name: "code_read", Description: "Read a file, optionally limited to a line range.", Binary: "agent-tools", Command: []string{"read"},
		Args: []bundledArg{{Name: "file", Description: "File path", Required: true}, {Name: "lines", Description: "Line or range: N, START:END, START:, :END", Flag: "--lines", Kind: bundledOption}},
	},
	{
		Name: "code_symbols", Description: "List the symbols in a file.", Binary: "agent-tools", Command: []string{"symbols"},
		Args: []bundledArg{{Name: "file", Description: "File path", Required: true}},
	},
	{
		Name: "code_symbol", Description: "Extract one symbol's source by name.", Binary: "agent-tools", Command: []string{"symbol"},
		Args: []bundledArg{{Name: "name", Description: "Symbol name", Required: true}, {Name: "file", Description: "File to search (default: the project index)", Flag: "--file", Kind: bundledOption}},
	},
	{
		Name: "code_search", Description: "Search the project index for symbols, files or knowledge concepts.", Binary: "agent-tools", Command: []string{"search"},
		Args: []bundledArg{{Name: "query", Description: "Search query", Required: true}, {Name: "type", Description: "symbol (default), file, knowledge or all", Flag: "--type", Kind: bundledOption}, {Name: "limit", Description: "Maximum results (default 20)", Flag: "--limit", Kind: bundledOption}},
	},
	{
		Name: "doc_outline", Description: "Show a Markdown file's heading outline.", Binary: "agent-tools", Command: []string{"doc", "outline"},
		Args: []bundledArg{{Name: "file", Description: "Markdown file", Required: true}},
	},
	{
		Name: "doc_section", Description: "Return one section of a Markdown file by heading text.", Binary: "agent-tools", Command: []string{"doc", "section"},
		Args: []bundledArg{{Name: "file", Description: "Markdown file", Required: true}, {Name: "section", Description: "Heading text", Required: true}},
	},
	{
		Name: "knowledge_get", Description: "Show a knowledge-graph concept as a compact card.", Binary: "agent-tools", Command: []string{"get"},
		Args: []bundledArg{{Name: "resource", Description: "Resource URI, title or identifier", Required: true}},
	},
	{
		Name: "knowledge_graph", Description: "Traverse the project knowledge graph from a resource.", Binary: "agent-tools", Command: []string{"graph"},
		Args: []bundledArg{{Name: "resource", Description: "Resource URI, title or symbol", Required: true}, {Name: "relation", Description: "Relationship to follow: calls, imports, inherits, implements, ...", Flag: "--relation", Kind: bundledOption}, {Name: "depth", Description: "Maximum depth (default 1)", Flag: "--depth", Kind: bundledOption}},
	},
}

func bundledToolDefinition(spec bundledToolSpec) mcpstdio.Tool {
	properties := make(map[string]any, len(spec.Args))
	required := make([]string, 0, len(spec.Args))
	for _, arg := range spec.Args {
		properties[arg.Name] = map[string]string{"type": "string", "description": arg.Description}
		if arg.Required {
			required = append(required, arg.Name)
		}
	}
	schema := struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}{Type: "object", Properties: properties, Required: required}
	encoded, _ := json.Marshal(schema)
	return mcpstdio.Tool{Name: spec.Name, Description: spec.Description, InputSchema: encoded}
}

func bundledToolArgv(spec bundledToolSpec, raw json.RawMessage) ([]string, error) {
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil || args == nil {
		return nil, fmt.Errorf("arguments must be an object")
	}
	var options, positionals []string
	for _, arg := range spec.Args {
		value, ok := args[arg.Name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			if arg.Required {
				return nil, fmt.Errorf("missing required argument: %s", arg.Name)
			}
			continue
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			text = bundledJSONValue(value)
		}
		if strings.TrimSpace(text) == "" {
			if arg.Required {
				return nil, fmt.Errorf("missing required argument: %s", arg.Name)
			}
			continue
		}
		switch arg.Kind {
		case bundledPositional:
			positionals = append(positionals, text)
		case bundledWords:
			positionals = append(positionals, strings.Fields(text)...)
		case bundledOption:
			options = append(options, arg.Flag+"="+text)
		}
	}
	argv := append([]string(nil), spec.Command...)
	argv = append(argv, options...)
	argv = append(argv, "--")
	argv = append(argv, positionals...)
	return argv, nil
}

func bundledJSONValue(raw json.RawMessage) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return string(raw)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return string(raw)
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return string(raw)
	}
	return strings.TrimSuffix(encoded.String(), "\n")
}
