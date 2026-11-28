package providers

import "strings"

// Antigravity qualifies tool names in ACP requests as mcp_<server>_<tool>.
func antigravityAmuxTool(name string) bool {
	tool, ok := strings.CutPrefix(name, "mcp_amux_")
	if !ok || tool == "" {
		return false
	}
	switch tool {
	case "submit_work", "project_message_thread", "task_agents", "task_reassign", "agent_identity",
		"task_get", "task_list", "task_create", "task_delegate", "task_update_delegation", "task_set_requester",
		"task_claim", "task_started", "task_comment", "task_ask", "task_release", "task_user_review", "task_done",
		"task_finish_review", "task_confirm_mutation", "task_set_changeset",
		"memory_search", "memory_store", "memory_get", "memory_update",
		"code_read", "code_symbols", "code_symbol", "code_search",
		"doc_outline", "doc_section", "knowledge_get", "knowledge_graph":
		return true
	default:
		return false
	}
}
