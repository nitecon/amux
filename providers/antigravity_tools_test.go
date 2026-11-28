package providers

import "testing"

func TestAntigravityDefaultsApproveOnlyKnownAmuxTools(t *testing.T) {
	for name, allowed := range map[string]bool{
		"mcp_amux_task_get": true, "mcp_amux_task_done": true, "mcp_amux_memory_get": true,
		"mcp_other_task_get": false, "mcp_amux_unknown": false, "mcp_amux_task_get_more": false,
		"terminal_exec": false, "mcp_amux_": false, "": false,
	} {
		if antigravityAmuxTool(name) != allowed {
			t.Errorf("incorrect permission decision for %q", name)
		}
	}
}
