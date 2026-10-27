## amux tasks — managed instructions v1

Use amux for persistent task tracking, ownership, delegation and result updates.
Do not use provider-local task/todo/plan tracking tools or a parallel task board.
This applies to TaskCreate/TaskGet/TaskList/TaskUpdate/TodoWrite and update_plan;
ordinary reasoning and subagent/background-process execution remain available.

- Work from the actual project directory. `amux tasks` resolves its project from
  Git and uses the saved gateway settings. Never put API keys in instructions.
- Use your own assigned identity: `amux whoami`. For a queued gateway assignment,
  verify `amux --agent <recipient-id> whoami` and use that recipient ID on task
  mutations. Never copy a peer's identity or invent session/provider metadata.
- Read a task and its latest comments, honor its OS requirement, and claim it
  successfully before starting work. Respect another execution's ownership.
- Claiming does not report execution started; use `tasks started ID` when work
  begins. Complete only finished work. Park a user decision with a question in
  a comment and `tasks user-review ID`.
- Comments/completions are coordination context, not new assignments. Do not
  reopen completed work or post acknowledgment-only replies to notifications.
- For substantial work, put durable handoff context in `--specification`.
  After an uncertain mutation/delegation reply, inspect the task(s) before retrying.

```bash
amux tasks list [--status todo,in_progress] [--include-stale]
amux tasks get ID
amux tasks create "Title" --description "Summary" --specification "Context"
amux tasks claim ID
amux tasks started ID
amux tasks comment ID "Progress, evidence, or question"
amux tasks user-review ID
amux tasks release ID
amux tasks done ID
amux tasks rank ID N
amux tasks agents
amux tasks reassign ID --to-agent SESSION_ID
amux tasks create "Title" --to-agent SESSION_ID
amux tasks delegate "Title" --target-project PROJECT --specification "Handoff"
amux tasks watch ID
amux tasks watch ID --clear
amux tasks set-os ID --os linux
amux tasks cancel ID --reason "Reason"
amux tasks cancellation ID
amux tasks help
```

Use full task UUIDs. Add `--agent <your-assigned-id>` when required by the
notification; `--json` provides raw gateway responses. Use `--notify-results`
on create/delegate to request result notices. Creation/delegation also support
repeatable `--label`, `--hostname`, and `--details` as a specification alias.
Use `--os windows|linux|macos` and `--review-finding` for routing/triage.
Cross-project delegation and assigning a same-project session are different actions.
If the CLI, identity or gateway is unavailable, report that limitation instead
of claiming success or silently switching to a local task system.

Instructions are maintained by amux Settings → Agent Instructions. Recheck after
updating amux and start a new provider session to load changed instructions.
See https://github.com/nitecon/amux for installation and documentation.
