# Default approval for Amux tools in Antigravity

The live Antigravity task paused on an ACP `session/request_permission` for `mcp_amux_task_get`. Amux registers the MCP server as `amux` for native Antigravity workers. Its ACP adapter currently turns every valid permission request into a Task Thread prompt. [Worker registration](../worker/native_tools.go) · [ACP adapter](../providers/acp/acp.go)

## Prefer Antigravity’s scoped rule

Google documents `mcp(amux/*)` as the rule syntax for all tools on the named MCP server. Under `permissions.allow`, it auto-approves those calls. Antigravity gives explicit deny and ask rules higher precedence. The installed `agy acp --help` lists a global permission bypass and does not list the standalone CLI’s `--settings` flag. Verify whether the installed ACP version honors the documented rule through an app-owned settings scope before deciding how to ship the default. [Google Antigravity permissions](https://antigravity.google/docs/permissions/?tab=cli) · [Google Antigravity ACP integration](https://antigravity.google/docs/ide/extensions/acp/)

## ACP client fallback

If a launch-scoped provider rule is unavailable, Amux can select an offered `allow_once` option for requests that it can attribute to tools on its own registered MCP server. ACP explicitly supports client-side automated decisions. Use structured provider tool identity where available and exact catalog membership if the ACP payload only supplies the provider’s qualified tool name. Carry the policy only on launches that register the Amux server. Requests with ambiguous identity, terminal operations, and other MCP servers keep their normal permission flow. Preserve cancellation behavior and existing gateway authorization. [ACP v1 permission protocol](https://agentclientprotocol.com/protocol/v1/tool-calls#requesting-permission)

## Acceptance

Use a disposable task and a real Antigravity ACP session. Confirm `task_get` and `task_done` run without approval, and verify completion on the gateway. Confirm an unrelated MCP call and a terminal operation still prompt under the default policy. Exercise explicit ask and deny rules with any provider-native solution. For the ACP fallback, also cover missing tool identity, a similar tool name on another server, cancellation, and requests without an offered allow-once option.
