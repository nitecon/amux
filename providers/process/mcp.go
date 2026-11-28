package process

// MCPConfig describes a native provider's stdio MCP server registration.
// EnvVars names environment variables the provider should forward by name.
type MCPConfig struct {
	Command string
	Args    []string
	EnvVars []string
	Env     map[string]string
}

// ManagedMCPEnv identifies the Amux subprocess that serves provider tools.
const ManagedMCPEnv = "AMUX_INTERNAL_MANAGED_MCP"
