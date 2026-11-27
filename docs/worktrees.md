# Task worktrees

Status: design proposal. Nothing here is implemented. The items under
[Validate before building](#validate-before-building) and
[Open questions](#open-questions) must be settled first.

## Goal

Agents work on a task in a folder that belongs to that task. The same task can
continue on another machine, or in the same folder on one machine, without
depending on what a previous agent left on disk.

- Task `abc-123` is worked on branch `amux/abc-123` in worktree `abc-123`.
- The author and the reviewer use the same task folder, one after the other.
- If the folder is missing (another machine, or it was cleaned up), amux checks
  it out again from the pushed branch.

The branch travels between machines. The worktree is a local checkout of it
that any machine can recreate.

## Task branch

Each task with a worktree has one branch, `<prefix><full task UUID>`, using the
existing work-submission branch prefix (`amux/` by default). This is the name
pull-request mode already pushes to; see
[Submitting work](gateway-cli.md#submitting-work).

## Worktree location

amux uses the first available root:

1. `<projects_directory>/.amux/worktrees`, when the client has a projects
   directory configured.
2. `~/.amux/worktrees`.
3. The system temporary directory.

The worktree is `<root>/<project>/<task UUID>`. Checkout discovery reads only
the direct children of the projects directory, so these folders are not listed
as project checkouts.

A temporary directory can be cleared by the system at any time. Only unpushed
work is lost when that happens; see [Pushed work only](#pushed-work-only).

## Size limit

Worktrees are used only when the repository is at most 500 MB. The limit is
configurable.

A larger repository keeps today's behaviour: agents share the existing
checkout, one writer at a time, with the current submission modes. No task
branch or worktree is created.

## Ensuring a worktree

amux runs this before it starts any agent on a task, whether author or
reviewer:

1. **The folder exists.** Fetch the task branch and fast-forward. If the folder
   has uncommitted changes or has diverged from the remote branch, stop and
   report on the task. Never reset.
2. **The folder is missing and the branch is on the remote.** Fetch the branch
   and add the worktree from it.
3. **Neither exists.** Create the branch from the default branch, add the
   worktree, and push the branch with its upstream set.

amux then starts the agent with the worktree as its checkout.

## Pushed work only

Only pushed commits reach another machine. Handoff therefore means pushed:

- `amux submit-work` in a task worktree commits the listed paths and pushes to
  the task branch.
- A reviewer reviews the pushed tip of the task branch. On one machine the
  reviewer also sees any uncommitted files in the shared folder; those are not
  part of the submission.
- Ignored and untracked files, including build output, do not travel. A fresh
  worktree needs its own build.

Two agents writing in the same task folder at the same time have the same
problems as a shared checkout today. Author then reviewer is the supported
sequence.

## Merge on completion

amux merges the task branch into the default branch when the task becomes
done, so the merge is recorded on the task that produced it.

With submission review enabled, that point is the reviewer's approval: today an
`approved` verdict moves the source task to done, and `changes_requested`
returns it to the queue for its author. Without review, it is the ordinary Done
transition.

The merge runs in the task worktree and does not touch the main checkout:

1. Fetch the default branch and merge it into the task branch.
2. Push the result to the default branch as a fast-forward, without force. If
   the default branch moved in the meantime, repeat from step 1.
3. Record the merged commit SHA on the task and mark it done.

If the merge conflicts, amux aborts it, comments on the task, and returns the
task to its author. The task is not done until its work is on the default
branch.

Git and the task store cannot commit together. If the push succeeds and
recording fails, amux retries the recording; a task branch that is already an
ancestor of the default branch counts as merged.

## Cleanup

Today amux has no registry linking worktrees to tasks. Reassignment stops the
old native worker but leaves any worktree an agent created, and its creator
stays responsible for it. Naming the worktree and branch by task UUID provides
that link for worktrees amux creates; worktrees an agent creates on its own
remain the agent's to clean up.

amux removes a task worktree and deletes its branch after a successful merge.
A worktree with everything pushed can also be removed earlier, because the next
agent's ensure step recreates it. A worktree with uncommitted or unpushed work
is kept and reported.

## Validate before building

- **Checkout identity.** The checkout ID includes the canonical directory
  ([identity.go](../projectidentity/identity.go)), so a worktree is a separate
  checkout from the main one. Confirm that an agent launched in a worktree can
  register, claim, submit, and complete a task.
- **Branch upstream.** Confirm that commit-and-push mode pushes to the task
  branch from a worktree without changes.

## Open questions

- **Protected default branches.** A direct push to the default branch fails
  where the host requires pull requests. Decide whether those projects stay on
  pull-request mode without amux merging.
- **Merged content differs from reviewed content** when the default branch
  moved after review. This proposal accepts a merge without conflicts. Decide
  whether such a merge needs another check first.
- **What the size limit measures.** The cost of a worktree is its checked-out
  files, not the shared object store. Proposed: the size of the tracked files
  on the default branch.
- **Which machine merges.** The gateway has no checkout. Proposed: the client
  of the agent whose action completed the task.
- **Windows path length.** A full task UUID under a deep root can exceed the
  default path limit for repositories with long paths.
