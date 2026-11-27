# Agent-managed worktrees and revision handoff

Status: proposed operating guidance. Validate linked-worktree submission before
making it the default for amux agents.

## Local checkout isolation

Use one checkout per concurrent writer. When working in parallel, start the
agent in its provider's native worktree or create one with `git worktree` inside
the authorized workspace. Keep edits, staging, and builds in that checkout.
Follow the repository's existing branch and publication rules.

Linked worktrees share Git objects, but each checkout materializes its own files
and may generate its own build output. For repositories such as large Unreal
projects, use one writer at a time per existing checkout when another checkout
would be too expensive. Agents on separate machines can use their own checkouts
concurrently.

The agent or operator who created a worktree owns its cleanup. Before removing
it, check for active processes, local-only commits, and files worth preserving,
including untracked or ignored files. Use Git's worktree removal command after
those checks. Retain the checkout when ownership or preservation is uncertain.

Amux currently has no registry linking agent-created worktrees to tasks. Task
reassignment stops the old native worker, but does not remove untracked
worktrees; their creator remains responsible for cleanup. Automatic cleanup
requires an explicit ownership binding before any checkout can be removed.

## Validate checkout binding before rollout

The [Gateway CLI](gateway-cli.md#submitting-work) expects submission from the
registered agent's original checkout, and [checkout identity](../projectidentity/identity.go)
includes the canonical directory. Launch and register an agent in a linked
worktree and verify that it can claim, submit, and complete a task from that
checkout. Confirm that submission uses the correct repository and exact
changeset. If the workflow fails, fix checkout binding before recommending
linked worktrees as the default.

## Cross-machine handoff

1. Record the comparison base SHA and publish the intended changes with the
   existing `amux submit-work` workflow.
2. After successful publication, record the base and published commit SHA on the
   task. Include the branch or pull request when it helps the receiving agent
   fetch the commit, along with validation results and next steps.
3. Start with the existing completion-triggered review flow. The reviewer
   fetches and reviews the exact published SHA against the recorded base and
   identifies that SHA in the result. If a later submission advances the branch,
   the earlier review still applies only to the SHA it inspected.

## When to revisit amux automation

Defer managed allocation and cleanup, mandatory task branches, and background
fetching. Revisit the smallest relevant change when the current workflow shows
a recurring problem.

- If reviews routinely need to be queued before publication, add a readiness
  gate keyed to the required submission SHA.
- If agents repeatedly contend for a checkout on machines that can only
  support one, add exclusive checkout locking.
