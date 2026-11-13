# Agent instructions

amux delivers operational guidance inside each agent session. It does not
install operational command manuals in user-level root harnesses or introduce
provider hooks. Repository instruction files remain untouched.

## Root harness cleanup

Every desktop startup removes recognized legacy managed blocks before restoring
saved terminals, including providers previously set to manual maintenance.
Unrelated user-authored text, including strict rules outside those blocks,
is preserved byte for byte. Recognized boundaries are:

- `<!-- amux:tasks:start -->` / `<!-- amux:tasks:end -->`
- `<agent-tools-rules>` / `</agent-tools-rules>`
- `<memory-rules>` / `</memory-rules>`

Changed existing files receive the existing sibling `.amux.bak` backup. Repeated
cleanup is a no-op. Duplicate, overlapping, inline or unmatched boundaries are
reported without changing that provider's instruction files.

| Provider | Root files | Home override |
| --- | --- | --- |
| Claude | `~/.claude/CLAUDE.md` | `CLAUDE_CONFIG_DIR` |
| Codex | `~/.codex/AGENTS.md` and `AGENTS.override.md` | `CODEX_HOME` |
| Copilot | `~/.copilot/copilot-instructions.md` | `COPILOT_HOME` |

Both Codex files are cleaned, including a base file shadowed by an override.
The existing **Settings → Agent Instructions** checks show resolved paths and
file errors. Neither automatic maintenance nor manual setup instructions can
reinstall the retired blocks.

## Contextual guidance

Session start/resume and explicit `/clear` receive current task-policy and tool
hints plus relevant durable memories. Desktop guidance waits for an idle agent
with an empty composer and uses the same native process verification and fenced
terminal queue as task notifications. A changed draft cancels background guidance.
Standalone MCP launches retain their existing startup instruction mechanism;
Codex's configured developer instructions are preserved.

Task assignments receive task identity, concise workflow hints and durable recall
based on the task. Updates and results retain their existing ownership and
informational boundaries. Hints cover reading and claiming work, fitting parent
scope, cross-project delegation and submission through `amux submit-work`.
Substantive untracked work should be associated with a fitting task. Task output
is read as normal text, without `--json` or parsing scripts.

Agents normally create subtasks under a fitting parent. Root-task exceptions use
`amux tasks create TITLE --root-reason REASON --specification TEXT`:
`user-directed` records an explicit user request, and `principle` records a
concrete DRY/refactor or similar core-principle finding. The specification must
record the request or finding, evidence and bounded acceptance criteria. Reasons
cannot accompany a parent. Otherwise, ask the user to create the root. Review
findings and cross-project handoffs still require fitting parents.

Available MCP tools determine tool hints. Memory supports `memory_search`,
`memory_get`, `memory_store` and `memory_update`; code exploration uses
`code_search`, `code_symbols`, `code_symbol`, `code_read`, `doc_outline` and
`doc_section`. Sessions without amux MCP receive CLI hints only for present bundled
executables. Templates are visible in Settings.

Task creation no longer asks agents to select General or Software. On completion,
release, User review or cancellation, the tool workflow asks whether files were
mutated. Reply with a `changeset` array of every changed path, or explicit null
when no files were changed; omission leaves the task unchanged. For completion
and User review, the question offers `amux set-task ID software` (MCP
`task_confirm_mutation`), followed by `amux set-changeset ID '["path"]'` (MCP
`task_set_changeset`). With no mutations, use `amux set-changeset ID null`
directly. The final call finishes the recorded transition, preserving the User
review action. Follow-ups require the same owning execution and unchanged task;
repeat the original transition if either changed. Internal classification is
updated by the response workflow, never chosen when creating work. This question
comes from the tool response, without an additional terminal injection.

With automatic reviews enabled in Settings > Gateway > Task automation, completing
main work with confirmed file changes queues separate code and documentation
reviews under the original task. No file changes means no automatic reviews.
Generated review/documentation tasks never create another review workflow,
including when they report changed files. Reviewers inspect the original change,
report concrete findings and create correction tasks under the original source;
they do not edit implementation or documentation during review. The server switch
applies immediately across projects; disabling it preserves already queued reviews.

Recall calls durable `memory search`, never `memory context` or WorkingContext.
It has a five-second timeout and a bounded output. Unchanged recall is suppressed
within the same context. Explicit context restoration permits recall again.

## Idle memory review

Each session work cycle allows one idle memory-review injection. A fresh session,
a new task assignment or explicit context clear begins a cycle. Resume, restart,
comments, ordinary messages and busy/idle changes do not re-arm it. Repeated
assignments for the same task do not re-arm it or clear context again. The legacy
classification no longer gates this optional review.

The allowance is persisted before dispatch in the amux state directory under
`guidance/<session-id>.json`. It belongs to the session rather than the project
directory. An uncertain or failed dispatch consumes the allowance; it is not
retried automatically. New task delivery preempts background guidance. Storage
failures suppress memory review and are reported in `queue.log`; task delivery
can continue without guidance bookkeeping.

The review asks the agent to consider durable decisions, corrections, conventions
and discoveries; search and inspect existing memories; update a matching memory
where appropriate; and store a new memory only when useful. No write is required.
Routine progress, transient logs and source-recoverable facts do not qualify.
No shared project handoff buffer is used for recall or session state.

## Optional local task settings

The existing settings choice still controls provider task-tool settings. Claude
setup adds local task tools to `permissions.deny`; Codex setup disables
`tools.update_plan`. Subagent and background tools remain available. Copilot has
instructional guidance only. Settings changes preserve other JSON/TOML values.

```sh
amux harness check
amux harness print claude
amux harness install claude
amux harness install codex
amux harness install copilot
```

`check` is read-only. `print` describes removal and optional task settings without
editing files. `install` cleans recognized blocks and configures task settings;
it never adds operational blocks. CLI installation does not change the desktop's
saved automatic/manual task-settings choice.
