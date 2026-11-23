package providerprocess

// MCPConfig describes a native provider's stdio MCP server registration.
// EnvVars names environment variables the provider should forward by name.
type MCPConfig struct {
	Command string
	Args    []string
	EnvVars []string
	Env     map[string]string
}
