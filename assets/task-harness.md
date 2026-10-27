## amux tasks — managed instructions v2

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
```

For more details read the task and associated comments.

Rules are maintained in amux Settings → Agent Instructions.
