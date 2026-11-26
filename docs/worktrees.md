# Task worktrees and distributed handoff

Status: design draft for iteration. This document records the worktree discussion;
it does not describe shipped functionality or authorize implementation. Proposed
names, fields, and transitions remain subject to implementation review.

## Problem and scope

Agents currently sharing a checkout can interfere with each other's files,
staging, branch selection, and build output. Tasks also move between machines,
so local filesystem state alone cannot provide a durable handoff.

The proposed system binds published Git work to a task, gives each execution
exclusive local workspace access, and delays dependent work until its required
revision is available. amux remains the coordination layer for delegation,
comments, task status, ownership, and execution. Upstream Git distributes commits.

Large repositories require a different allocation policy. An Unreal checkout
may exceed 1 TB; creating another checkout for every agent could exhaust a disk.
Those projects must be able to queue agents against existing local checkouts
while other machines continue working independently.

This draft covers task branches, revision dependencies, workspace allocation,
readiness, handoff, visibility, and cleanup. It does not define a new Git hosting
service, distributed filesystem, merge queue, or automatic conflict resolver.

## Direction established in the discussion

- Bind implementation branches to task IDs: `amux/task/<taskid>`.
- Publish work upstream so another machine can fetch and continue or review it.
- Identify a submitted revision by its exact commit SHA, together with its base.
- Run one shared fetch loop per local repository, approximately every 30 seconds
  while revision dependencies are waiting.
- Introduce an explicit `not_ready` queue excluded from ordinary assignment.
- Have the readiness plugin validate prerequisites and coordinate promotion and
  assignment through the state machine.
- Prefer dedicated task workspaces under `~/.amux/worktrees/<taskid>`, with a
  configurable root for machines using another disk.
- Enforce capacity limits and support exclusive queued use of existing checkouts.
- Use task comments and commit messages to preserve human-readable handoff context.

Detailed recovery rules and policy defaults below are proposals for review. Open
decisions are collected at the end of this document.

## Identity and durable records

A task branch and a local worktree serve different purposes. The branch and
commits are published; worktree directories and Git's local worktree metadata
are not transferred between machines.

Linked worktrees share Git object storage, but have separate checked-out files
and per-worktree state. Shared objects do not eliminate the disk cost of another
large checkout. See the [Git worktree documentation](https://git-scm.com/docs/git-worktree).

The model needs the following information; this is a conceptual inventory, not
a database schema or API contract:

| Record | Information |
| --- | --- |
| Task Git binding | Repository identity, task ID, task branch, recorded base SHA. |
| Submission | Source task, submission identity/version, base SHA, published commit SHA. |
| Dependent task | Source task and required submission; whether waiting for its first submission or bound to an existing one. |
| Local workspace | Machine identity, repository, task, canonical path, allocation mode, lifecycle state. |
| Preparation reservation | Task, submission version, target machine, workspace, execution/agent identity, expiry. |
| Review result | Reviewed submission and exact commit SHA, with findings and outcome. |

Existing confirmed `changeset` paths remain useful for attribution of changed
files to work. A path list cannot identify the file contents being reviewed and
does not replace a submitted commit SHA.

Task-to-task revision dependencies are also distinct from plugin subscription
`depends_on`, which orders plugins processing the same event in
[the state machine design](StateMachine.md). Reuse existing task and event
mechanisms where their semantics fit; do not overload plugin ordering to mean
that another task has published a revision.

## Branch and submission lifecycle

1. An implementation task records its base SHA and obtains a workspace on
   `amux/task/<taskid>`.
2. The author works and may publish intermediate checkpoints.
3. Submission pushes the intended commits successfully, then records the
   published SHA and base as a durable submission in amux.
4. Dependent tasks bind to that submission and can begin local preparation.
5. Review and validation results refer to the submitted SHA, even if the branch
   subsequently advances.

Branch existence or a changed branch tip is insufficient evidence of readiness:
it may represent unfinished work. Submission is the explicit handoff boundary.
Compare content against the recorded base when determining whether there is a
change to inspect; different commit IDs alone can represent identical content.

Do not make review readiness depend on continually matching current `main`.
Main can advance independently or already include the feature. Capture the
comparison base and submitted revision so another machine can reproduce the
intended review. Whether integration against newer main is required is a
separate policy decision.

Git publication and an amux transaction cannot be atomic together. If push
succeeds but recording the submission fails, retain the published commit and
retry recording the same submission idempotently. If push fails, do not mark
the revision as published or promote its dependents.

For the initial design, allow one active implementation writer per task across
machines. Independent implementation efforts should use separate child tasks
and branches. Review can run concurrently on another machine against a pinned
revision. A review assignment does not grant permission to push implementation
changes to the source branch.

## Revision dependencies and review freshness

A review can be created while implementation is still in progress. It starts
in `not_ready`, referencing the source task and waiting for a submission. Once
bound, the dependency names an exact submitted revision.

Before starting, the receiving machine must fetch that revision and prepare its
workspace at the required SHA. It must not silently substitute the latest
branch tip when the branch has advanced.

If a newer submission arrives during review, preserve the old result as evidence
about the old revision. That result cannot satisfy approval of the newer
submission. A delayed promotion or completion request must be checked against
the submission version it names. The choice between cancelling, finishing, or
replacing an old review remains open; none may silently approve the new SHA.

Documentation-only and other non-code tasks must not acquire a fabricated Git
dependency. An explicit no-file-change result follows existing task semantics;
it does not manufacture a code review merely to satisfy the new workflow.

## Readiness and assignment

Readiness has a shared component and a machine-local component:

- Shared: the source published the required submission and task dependencies
  permit execution.
- Local: the target machine has the commit, capacity, and an exclusively
  reserved workspace prepared for the task.

Machine A fetching a commit does not establish machine B's readiness. A local
readiness report must identify its machine, workspace, and submission version.

The proposed lifecycle is:

| Phase | Behavior |
| --- | --- |
| `not_ready` | Ordinary assignment and agent claims exclude the task. The plugin reconciles its prerequisites. |
| Preparing | The plugin holds a bounded preparation reservation while fetching/checking out and validating locally. This need not be a new task status. |
| Ready with assignment | The server atomically records the validated target/reservation and promotes the task to executable state. |
| Running | The worker revalidates its reservation and workspace immediately before starting the agent. |
| Completed/released | Existing task workflow applies, with revision evidence and workspace reconciliation added. |

`ready` here is a conceptual phase. Decide during implementation whether the
existing executable `todo` queue represents it or a new persisted status is
needed. Do not change current queue semantics merely through this document.

The plugin supplies local validation evidence; the server owns the authoritative
transition. Promotion and assignment must be one transactional operation, checked
against current task state, dependency/submission version, reservation, and
caller authority. A separate assignment followed by a status update permits
another controller to act on an intermediate state.

All assignment paths, including manual claims and delegated work, must respect
`not_ready`. Reordering a card must not bypass prerequisites. No agent starts
solely because a stale event says the task was ready.

If preparation fails, keep or return the task to `not_ready` and release its
reservation safely. If a selected machine disappears before execution, expiry
allows another machine to prepare it. Expiry does not prove that an already
running process has stopped.

## Plugin reconciliation and Git fetching

Use amux task/submission events for prompt reconciliation, plus the periodic
fetch loop to recover from missed events and discover upstream commits. Task
delegations and comments continue to carry coordination and handoff context.

There is one fetch owner per local repository, shared across agents, waiting
tasks, and linked worktrees. It is scoped to the shared local Git repository,
not just a selected task directory. Separate clones on separate machines each
have their own loop. Multiple local amux processes must coordinate ownership
so they do not each start an independent loop for the same repository.

- Fetch approximately every 30 seconds while relevant dependencies are waiting.
- Bound fetch duration and prevent overlapping fetches.
- Use backoff during network or authentication failures; leave work waiting.
- Check that required commits are available after fetch, then reconcile waiting
  tasks against their current submission versions.
- Fetching must not reset a checkout, switch an active agent's branch, or discard
  local work. Checkout preparation is a separate exclusive operation.
- Stop the loop with its owning runtime. Avoid an unbounded background process
  or a per-task polling loop.

Persist waiting reasons as structured state. Suggested reasons include source
submission missing, commit unavailable, workspace capacity unavailable, and
workspace cleanup required. Show those reasons in the UI. Add comments for
meaningful handoffs or reason changes, rather than every fetch attempt.

## Local workspace layout and file viewer

The proposed default is:

```text
~/.amux/worktrees/<full-task-id>/
```

The directory belongs to a local execution of the named task. It is not a
globally shared filesystem path. A review task can have its own task directory,
checked out at the source submission SHA, while the source task keeps its own
workspace. A detached checkout is suitable for inspection; actual mutation must
follow the task's authorized branch and submission workflow.

Make the root configurable per local repository/machine. A home directory on a
small system disk is unsuitable for a large checkout on another volume. Use
full task IDs and bind each allocation to its repository. If task IDs are not
globally unique across servers, the path scheme needs a namespace before use.
Only manage paths registered as amux-owned workspaces; existing arbitrary
directories must not be adopted or removed just because their names match.

The task file viewer should resolve the selected local workspace explicitly and
show the task, branch or pinned revision, and local preparation state. It should
support browsing workspace files, the submitted base-to-revision diff, and local
changes since submission. Show when files are unavailable locally rather than
silently displaying the default checkout as the task's files.

Do not replace or mutate the user's default checkout when allocating an isolated
workspace. Queued use of an existing checkout is an explicit alternate policy,
with exclusive access and preservation of pre-existing user changes.

This layout is a proposed runtime location. It does not grant development agents
permission to modify code outside their authorized repository boundaries.

## Capacity and large repositories

Provide two allocation modes with the same task branch/submission model:

| Mode | Allocation |
| --- | --- |
| Isolated | Allocate a dedicated task worktree within configured limits. |
| Queued | Reserve an existing checkout exclusively; additional local tasks wait. |

The initial discussion suggested disabling automatic worktree creation above
500 MB. Treat that as a proposed configurable threshold, not a settled universal
constant. Define what is measured before implementing it: Git history size alone
does not measure the cost of checked-out assets, LFS materialization, or builds.

Admission should consider estimated additional checkout cost, a maximum number
of allocated worktrees, minimum free space on the destination volume, and an
explicit disable-worktrees setting. Reserve capacity before allocation so two
agents cannot both consume the same apparent headroom. Retained workspaces
awaiting cleanup count toward capacity. Build output can grow after admission;
these checks are not a hard filesystem quota.

For a large Unreal repository, each machine may queue against one existing
checkout. Machine A and machine B can run separate tasks in their own checkouts;
agents on machine A cannot share its checkout concurrently. A separately
provisioned checkout can provide additional capacity if explicitly configured.

Lock the canonical checkout, not merely the agent or machine name. Multiple
amux instances using the same directory must observe the same exclusion. Keep
the reservation until agents and relevant child processes using it have stopped.
Before switching tasks, inspect cleanliness and unpublished work. Never resolve
a busy or dirty checkout through automatic destructive cleanup.

## Handoff context

The submission record is the machine-readable handoff. Task comments explain
decisions, validation, blockers, and next actions. Commit messages provide useful
context even when a receiving system initially has only Git access.

Example message, using the existing task trailer convention:

```text
Implement feature A configuration

Configuration parsing and validation are complete.
Validated with the configuration test suite.
Remaining: connect configuration to desktop controls.

AmuxID: <task-id>
```

Do not parse free-form prose to decide readiness. Preserve the existing
submission mechanism's ownership checks, exact changeset handling, and prohibition
on attribution trailers. Worktree support should extend that mechanism rather
than introduce a parallel commit/push path.

## Cleanup, closure, and offline machines

Every system with a registered task workspace must reconcile it when the task
closes. The recommended design separates task completion from local cleanup
acknowledgment so an offline machine cannot block closure indefinitely. This
separation is a proposal to confirm before implementation.

On closure, prevent new execution for that task and request cleanup of registered
workspaces. Online machines inspect immediately; offline machines reconcile on
reconnection and before reusing the workspace. Track pending, retained with a
reason, and removed outcomes separately from task status.

Before automatic removal, verify that:

- No agent or relevant child process still uses the workspace.
- There are no uncommitted changes or files requiring preservation, including
  untracked or ignored files; ignored build output needs an explicit policy.
- Local commits are durably published and no local-only work would be lost.
- Reviews and other dependent executions no longer require the workspace.
- The task/workspace version still matches the cleanup request.

A clean `git status` alone is insufficient: commits can exist only locally.
Retain uncertain workspaces with a visible reason and count them against limits.
Use Git's worktree removal mechanism after validation; do not force-delete dirty
directories. Remote branch retention/deletion is a separate policy and must not
remove commits still needed by dependents. This proposal makes no tag changes.

Cancellation or lease expiry does not establish process termination. A machine
that was disconnected may still contain edits. On reconnection, revalidate task
ownership before accepting managed submission and inspect its workspace before
reuse. Server fencing can reject stale managed actions; it cannot physically
stop an offline process or prevent arbitrary direct Git commands.

## Fit with existing amux behavior

Inspect and reuse these areas when implementation is authorized:

- [State machine](StateMachine.md): transactional task transitions, assignment,
  plugin replay/reconciliation, ownership, and completion/review boundaries.
- [Gateway CLI](gateway-cli.md#submitting-work): exact-file submission,
  publication recovery, and the existing `AmuxID` task reference.
- [Agent instructions](agent-instructions.md): changeset confirmation and
  independent review workflow.
- [Checkout identity](../projectidentity/identity.go): checkout identity includes
  the directory, and session identity derives from checkout identity. Task
  directories must not accidentally create new logical agent identities.
- [Worktree status](../projectidentity/worktree.go) and
  [desktop status view](../desktop/worktree_status.go): existing read-only local
  status and checkout-bound display behavior.

The current workflow generates automatic reviews after completion with confirmed
file changes. Supporting a manually created dependent review before completion
must coexist with that behavior without duplicate review obligations. Ordinary
completion must remain durable if later review preparation fails.

Implementation must update the relevant authoritative workflow documentation
together. Until then this file is a proposal, not an override of current task
statuses, checkout identities, or work-submission settings.

## Suggested implementation slices

1. Define submission/base identity and task revision dependencies using existing
   task ownership and submission paths.
2. Add `not_ready` eligibility enforcement and atomic preparation/promotion,
   including stale-submission rejection and observable waiting reasons.
3. Add the shared repository fetch owner and local workspace validation.
4. Add exclusive queued checkout use and bounded isolated worktree allocation.
5. Bind file browsing/diffs to task workspaces and add cleanup reconciliation.

Each slice should preserve existing execution modes and be reviewable on its own.
Do not introduce workspace pools, automatic merges, or new infrastructure without
an immediate requirement.

## Validation required before rollout

- Two local agents cannot modify the same allocated checkout concurrently,
  including when launched by separate local amux processes.
- A review created before submission remains unassigned until the exact required
  revision and a local workspace are ready.
- Concurrent readiness reports yield one assignment; stale reports cannot
  promote a changed, cancelled, closed, or reassigned task.
- Branch advancement cannot change an active review's revision or let an old
  result approve a newer submission.
- Several waiting tasks share one fetch loop; fetch errors/restarts preserve
  their dependencies and do not duplicate execution.
- Concurrent allocations respect destination-volume reservations and configured
  limits; large repositories queue without silently creating more checkouts.
- Process interruption and offline lease expiry preserve local changes and
  reject stale managed submission attempts.
- Closure while a machine is offline leaves visible cleanup obligations;
  reconnection reconciles safely and preserves unpublished or ignored work.
- Existing user changes survive preparation failures, release, and cleanup.
- Desktop and headless workers use the same readiness and workspace rules.

## Open decisions for the next iteration

- Does executable readiness use existing `todo`, with `not_ready` added, or is
  a separate `ready` status necessary?
- How does a pre-created review bind to a first submission, and how are later
  revisions scheduled without duplicating generated review tasks?
- When a new submission supersedes an active review, should it finish, stop,
  or be replaced? What happens to a review whose diff is empty?
- What does the size threshold measure, what units does 500 MB mean, and what
  defaults apply to checkout estimates, free-space reserves, and allocation count?
- How are tracked assets, LFS downloads, ignored Unreal output, and build caches
  accounted for and preserved? No shared mutable asset scheme is assumed here.
- What are the preparation lease duration, renewal, and running-task recovery
  rules under the existing execution ownership model?
- Is task closure independent of cleanup acknowledgment as proposed, and what
  retention/recovery UI is needed for dirty or offline workspaces?
- How are reviewed task branches integrated into main, and when may they be
  retired? Worktree allocation alone does not define integration policy.
- How is stable agent identity maintained across task directories, and is a
  server/repository namespace required in the default path?
- How does this optional worktree policy coexist with existing direct-push and
  pull-request submission settings, including repositories that prohibit branches?
