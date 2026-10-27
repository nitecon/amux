## amux tasks — managed instructions v2

Use amux for all task tracking. Do not use local task/todo/plan tools
(TaskCreate/TaskGet/TaskList/TaskUpdate/TodoWrite or update_plan).
Subagent and background-process tools remain available.

Run commands from the project directory. Read the normal shortened text output
directly. Do not use `--json`, Python, or scripts to parse amux task output.

Use your own assigned identity (`amux whoami`). If a gateway notification supplies
your recipient ID, verify it with `amux --agent <recipient-id> whoami` and use
that ID on task mutations. Never copy a peer's identity or invent actor metadata.

```sh
amux tasks list
amux tasks get ID
amux tasks create "Title" --specification "Context and acceptance criteria"
amux tasks claim ID
amux tasks started ID
amux tasks comment ID "Progress, evidence, or question"
amux tasks user-review ID
amux tasks release ID
amux tasks done ID
amux tasks delegate "Title" --target-project PROJECT --specification "Handoff"
amux tasks help
```

Read the task and latest comments, honor its OS requirement, and claim it before
working. Respect another execution's ownership. Report started when work begins;
complete only finished work. For a user decision, comment with the question and
use user-review. Keep durable handoff context in the specification. After an
uncertain mutation reply, inspect the task before retrying. Comments/completions
are coordination context, not new assignments. If amux, identity or the gateway
is unavailable, report that instead of silently using a local task system.

Rules are maintained in amux Settings → Agent Instructions.
See https://github.com/nitecon/amux for setup and documentation.
