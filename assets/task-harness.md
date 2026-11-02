## amux tasks — managed instructions v3

Use amux for all task tracking. Do not use local task/todo/plan tools
(TaskCreate/TaskGet/TaskList/TaskUpdate/TodoWrite or update_plan).
Subagent and background-process tools remain available.

Run commands from the project directory. Read the normal shortened text output
directly. Do not use `--json`, Python, or scripts to parse amux task output.

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
amux submit-work ID "Commit message" PATH...
```

Finish code work with `amux submit-work` (MCP tool `submit_work`), listing
every file you changed. It commits only those files, pushes them or opens a pull
request as set in amux Settings, and posts the result on the task. Never run
`git commit` or `git push` for task work yourself, and never add Co-Authored-By
or other attribution lines to commit messages.

For more details read the task and associated comments.

Rules are maintained in amux Settings → Agent Instructions.
