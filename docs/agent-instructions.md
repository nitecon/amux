# Agent instructions

amux can detect Claude Code, Codex and GitHub Copilot CLI through their executable
on PATH or their user configuration directory. Startup discovery only reads files.
If a detected agent needs instructions or task settings, a banner offers
**Agent Instructions** in Settings. Providers not detected can also be configured
there, for example before their first launch.

## Install and verify

Open **Settings → Agent Instructions**, then choose **Install / update
instructions** for each provider you use. Installation applies immediately;
**Save & Close** is for other preferences. **Check installation and updates**
rechecks the files against the instructions bundled with the running amux version.
Checks do not contact an update service or upgrade provider software. After an
amux update, install/update refreshes its managed block. Start a new provider
session to load changes; already running agents retain their loaded instructions.

The same operations are available without the desktop:

```sh
amux harness check
amux harness install claude
amux harness install codex
amux harness install copilot
```

Each check reports detection, target paths, missing/current/outdated instructions,
local-task settings and file errors. A current check confirms file contents,
not that an already running model has reloaded them or that every profile applies
the same configuration. Task commands still require a configured gateway and an
assigned amux identity. The managed block never contains credentials or a fixed
agent identity; agents resolve their own assigned identity.

## Files and preservation

| Provider | User instructions | Task settings |
| --- | --- | --- |
| Claude Code | `~/.claude/CLAUDE.md` | `~/.claude/settings.json` |
| Codex | `~/.codex/AGENTS.md`, or a nonempty `AGENTS.override.md` if present | `~/.codex/config.toml` |
| Copilot CLI | `~/.copilot/copilot-instructions.md` | Instructional guidance only |

`CLAUDE_CONFIG_DIR`, `CODEX_HOME` and `COPILOT_HOME` override the respective
directories. Settings shows the resolved paths. Project instructions are not
rewritten. Each installed block is bounded by `<!-- amux:tasks:start -->` and
`<!-- amux:tasks:end -->`; updates replace only that block. Other text, Claude
settings keys and Codex TOML comments/settings remain intact. Changed existing
files get a sibling `.amux.bak` backup. Repeated installation is a no-op when
current. Invalid JSON/TOML, malformed settings shapes, duplicate blocks and
unmatched markers are reported for repair rather than discarded. Installation
validates both files first; if a write fails, the UI reports an incomplete install
and rechecks actual files. It does not claim an all-or-nothing multi-file update.

## Local task systems

The block directs agents to use amux for task tracking and avoid parallel local
todo/plan boards. It documents task ownership, lifecycle, delegation, result
subscriptions and handling uncertain replies.

Claude installation adds `TaskCreate`, `TaskGet`, `TaskList`, `TaskUpdate` and
`TodoWrite` to `permissions.deny`, preserving existing permissions. It leaves
`Agent`, background-process tools and other capabilities available. Bare-name
deny rules remove matching tools from Claude's context. See
[Claude permissions](https://code.claude.com/docs/en/permissions).

Codex installation sets `tools.update_plan.enabled = false` in its user TOML
configuration. This key was verified against installed Codex CLI 0.164.0 using
strict startup validation in an isolated profile; an unknown tool key was rejected
as a control. Older provider versions may require an update. Profile or command
overrides can change the effective value. Global instruction discovery and the
override-file precedence follow [OpenAI's AGENTS.md documentation](https://learn.chatgpt.com/docs/agent-configuration/agents-md).

Copilot receives user instructions through its
[documented global instruction file](https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-custom-instructions).
No supported local task-list disable setting was verified. Its documented `task`
tool runs subagents, so amux does not disable it or claim enforced task-tool parity;
the UI explicitly reports **Instructions only**. See
[Copilot's tool reference](https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-command-reference#tool-availability-values).

Removal of old agent-tools task rules/hooks and migration stubs is delegated to
agent-tools. amux installs its own block without removing unrelated agent-tools,
memory or user-authored instructions.
