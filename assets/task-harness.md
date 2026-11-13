## amux tasks — managed instructions v8

Use amux for all task tracking. Do not use local task/todo/plan tools
(TaskCreate/TaskGet/TaskList/TaskUpdate/TodoWrite or update_plan).
Subagent and background-process tools remain available.

Run commands from the project directory. Read the normal shortened text output
directly. Do not use `--json`, Python, or scripts to parse amux task output.

Agents normally create subtasks. Before creating work, read the
proposed parent and check that its scope includes the subtask. Choose a better
existing parent if needed; do not force unrelated work under an arbitrary parent.
Agents may create a root only for an explicit user request (`--root-reason
user-directed`) or a concrete DRY/refactor or similar core-principle finding
(`--root-reason principle`). Use `--specification` to record the user's request
or the principle, concrete evidence and bounded acceptance criteria. Root reasons
cannot accompany `--parent`. Otherwise, ask the user to create the root and
propose its title and description. Review findings belong under the work reviewed,
including completed work. Cross-project handoffs specify a parent in the
source project, preserving the link to the feature or requirement.

```sh
amux tasks list
amux tasks get ID
amux tasks create "Title" --parent PARENT_ID --specification "Context and acceptance criteria"
amux tasks claim ID
amux tasks started ID
amux tasks comment ID "Progress, evidence, or question"
amux tasks user-review ID "Specific user action required"
amux tasks release ID
amux tasks done ID
amux tasks delegate "Title" --parent PARENT_ID --target-project PROJECT --specification "Handoff"
amux tasks help
amux submit-work ID "Commit message" PATH...
```

Tasks have one creation workflow. When completing, releasing, parking or cancelling
work, confirm every mutated file with `--changeset '["path/to/file"]'`, or
`--changeset null` if no files were changed. MCP transition tools accept the same
array/null `changeset` value. Omission returns the gateway's confirmation question
without moving the task. Review affected documentation and search/update relevant
durable memories where useful. Completed main work with file changes queues code
and documentation reviews when enabled. Generated review/documentation tasks never
create another review workflow, including when reporting file changes.

Before moving a task to User review, comment with the question or decision needed
and provide the specific user action as the required command argument.

Finish code work with `amux submit-work` (MCP tool `submit_work`), listing
every file you changed. It commits only those files, pushes them or opens a pull
request as set in amux Settings, and posts the result on the task. Never run
`git commit` or `git push` for task work yourself, and never add Co-Authored-By
or other attribution lines to commit messages.

For more details read the task and associated comments.

Rules are maintained in amux Settings → Agent Instructions.
