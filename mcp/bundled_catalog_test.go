package mcp

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/nitecon/amux/server"
)

func TestBundledCatalogHasExactUniqueSourceTools(t *testing.T) {
	if got := len(bundledToolCatalog); got != 12 {
		t.Fatalf("catalog has %d tools, want 12", got)
	}
	wantMemory := map[string]bool{"memory_search": true, "memory_store": true, "memory_get": true, "memory_update": true}
	wantAgentTools := map[string]bool{"code_read": true, "code_symbols": true, "code_symbol": true, "code_search": true, "doc_outline": true, "doc_section": true, "knowledge_get": true, "knowledge_graph": true}
	seen := make(map[string]bool)
	for _, spec := range bundledToolCatalog {
		if seen[spec.Name] {
			t.Fatalf("duplicate tool %q", spec.Name)
		}
		seen[spec.Name] = true
		if spec.Binary == "memory" {
			delete(wantMemory, spec.Name)
		} else if spec.Binary == "agent-tools" {
			delete(wantAgentTools, spec.Name)
		} else {
			t.Errorf("%s has unexpected binary %q", spec.Name, spec.Binary)
		}
		tool := bundledToolDefinition(spec)
		var schema struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s schema: %v", spec.Name, err)
		}
		for _, arg := range spec.Args {
			if schema.Properties[arg.Name].Type != "string" {
				t.Errorf("%s.%s schema type = %q, want string", spec.Name, arg.Name, schema.Properties[arg.Name].Type)
			}
			contains := false
			for _, required := range schema.Required {
				contains = contains || required == arg.Name
			}
			if contains != arg.Required {
				t.Errorf("%s.%s required = %v, want %v", spec.Name, arg.Name, contains, arg.Required)
			}
		}
	}
	if len(wantMemory) != 0 || len(wantAgentTools) != 0 {
		t.Fatalf("catalog omissions: memory=%v agent-tools=%v", wantMemory, wantAgentTools)
	}
}

func TestCombinedMCPToolCatalogNamesAreUnique(t *testing.T) {
	tools := server.AgentTaskTools()
	tools = append(tools, submitWorkMCPTool())
	for _, spec := range bundledToolCatalog {
		tools = append(tools, bundledToolDefinition(spec))
	}
	if len(tools) != 30 {
		t.Fatalf("combined MCP catalog has %d tools, want 30", len(tools))
	}
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if _, exists := seen[tool.Name]; exists {
			t.Errorf("duplicate MCP tool name %q", tool.Name)
		}
		seen[tool.Name] = struct{}{}
	}
}

func TestBundledToolArgvOptionsPrecedeSeparatedLiteralPositionals(t *testing.T) {
	spec := bundledToolSpec{
		Name: "fixture", Binary: "agent-tools", Command: []string{"example", "read"},
		Args: []bundledArg{
			{Name: "file", Required: true},
			{Name: "lines", Flag: "--lines", Kind: bundledOption},
		},
	}
	got, err := bundledToolArgv(spec, json.RawMessage(`{"file":"-odd file.txt","lines":"1:3"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"example", "read", "--lines=1:3", "--", "-odd file.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v, want %#v", got, want)
	}
}

func TestBundledToolArgvMatchesSourceValueAndWordsCoercion(t *testing.T) {
	spec := bundledToolSpec{Name: "fixture", Command: []string{"get"}, Args: []bundledArg{{Name: "ids", Required: true, Kind: bundledWords}}}
	got, err := bundledToolArgv(spec, json.RawMessage(`{"ids":"  alpha\t beta  ","ignored":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"get", "--", "alpha", "beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("words argv = %#v, want %#v", got, want)
	}
	spec = bundledToolSpec{Name: "fixture", Command: []string{"search"}, Args: []bundledArg{{Name: "query", Required: true}}}
	got, err = bundledToolArgv(spec, json.RawMessage(`{"query":{"b": 2, "a": 1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"search", "--", `{"a":1,"b":2}`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON value argv = %#v, want %#v", got, want)
	}
}

func TestBundledToolArgvOmitsNullOrBlankOptionalAndRejectsMissingRequired(t *testing.T) {
	spec := bundledToolSpec{Name: "fixture", Command: []string{"search"}, Args: []bundledArg{{Name: "query", Required: true}, {Name: "limit", Flag: "--limit", Kind: bundledOption}}}
	got, err := bundledToolArgv(spec, json.RawMessage(`{"query":"hello","limit":"  "}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"search", "--", "hello"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v, want %#v", got, want)
	}
	for _, raw := range []string{`{}`, `{"query":null}`, `{"query":"  "}`} {
		if _, err := bundledToolArgv(spec, json.RawMessage(raw)); err == nil {
			t.Errorf("bundledToolArgv(%s) succeeded, want required argument error", raw)
		}
	}
}
