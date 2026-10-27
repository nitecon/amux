# Agent instructions

amux can detect Claude Code, Codex and GitHub Copilot CLI through their executable
on PATH or their user configuration directory. On startup, a detected agent with
missing or outdated rules gets a prompt naming its control file. Choose
**Update rules automatically** or **Show rules; don’t ask again**. Choices are
saved per provider on this machine. Providers not detected can also be configured
in Settings, for example before their first launch.

## Install and verify

Startup suggestions appear as dismissible cards in the bottom-right corner,
without resizing the terminal. Dismissing a card only hides it for this launch;
use **Show rules; don’t ask again** to remember a manual choice. Check and install
results use temporary corner notifications and remain available in Settings.

**Update rules automatically** installs the managed instructions and task settings,
then enables verification and repair on every startup, including after amux
updates. Startup maintenance finishes before saved provider sessions are restored,
so those sessions read the updated files. Only opted-in providers are edited.

**Show rules; don’t ask again** displays the rules and settings snippets with a
copy button, without editing provider files. amux remembers manual mode and never
prompts or auto-edits that provider again, even if the rules later become outdated.
To change that choice, use **Update rules** in **Settings → Agent Instructions**;
this installs the latest block and enables automatic startup maintenance.

These actions apply immediately; **Save & Close** is for other preferences.
**Check installation and updates** is a read-only check against the rules bundled
with the running amux version. Checks do not contact an update service or upgrade
provider software. Start a new provider session after editing rules; already
running agents retain their loaded instructions. Choices live in
`harness-preferences.json` in the amux state directory (`AMUX_STATE_DIR` when set).

The same operations are available without the desktop:

```sh
amux harness check
amux harness print claude
amux harness install claude
amux harness install codex
amux harness install copilot
```

`print PROVIDER` only prints manual instructions and task-settings snippets;
`install PROVIDER` performs a one-time install. These CLI commands do not change
the desktop startup choice. Automatic maintenance is an explicit desktop choice.

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

The concise block lists basic task lifecycle and delegation commands and points
to task comments for more details. Agents must read amux's normal shortened
text output directly, without `--json`, Python or scripts to parse the output,
and avoid parallel local todo/plan boards.

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

Agent-tools has retired its task rules/hooks in favor of amux migration stubs.
Existing agent-tools rules and skills still need regeneration/reloading on each
host. amux installs its own block without removing unrelated agent-tools, memory
or user-authored instructions.
