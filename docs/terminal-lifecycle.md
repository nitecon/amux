# Terminal lifecycle candidate

## Missing conversation history

Restored agent tabs first use the provider's standard continuation command:
`codex resume --last` or `claude --continue`. If that attempt exits with code 1,
amux launches the same provider once with no arguments, retaining the terminal
and assigned amux identity. The fresh process is registered with its own native
PID/generation, and its exit status is returned. Successful resumes, other exit
codes, signals and custom arguments do not trigger a fallback. Provider input and
output remain attached to the terminal throughout.

## Process ownership and cleanup

On Linux, a newly created desktop terminal retains a native pidfd for its
initial PTY child and its exact process generation. Alacritty starts that child
as a new terminal session leader. Closing its tab, pane or desktop requests
termination of processes still in that owned session, including background jobs
in separate process groups. The guard runs before the PTY is dropped.

After a process exits, its tab retains up to 32,768 characters from the final
visible viewport as plain text, including failure diagnostics. The PTY and input
binding are released. This temporary text excludes scrollback and is not saved
in the layout; restoring a layout never restarts the process.

Cleanup checks the original leader before and after enumeration, captures each
member's native handle with generation checks, then rechecks session membership
before signaling. It uses SIGKILL to retire captured jobs and the leader. The
scan has a 100 ms budget and a 256-member bound. A bound or signal failure leaves
complete retirement unconfirmed. Retained handles prevent a recycled PID from
receiving an old termination request. The PTY backend still owns reaping its
direct child; this guard does not wait for arbitrary descendants.

Work deliberately detached into another session before close is outside this
ownership boundary. A peer terminal in the same project is also independent.
Closing a terminal does not cancel a gateway task, report its outcome, or restart
work. Task outcome remains unknown without an explicit executor report.

The Linux native test first reproduced a separate-group, HUP-ignoring background
job surviving the earlier PTY-only close. With the guard it verifies retirement
of that job and the registered agent, while a peer terminal and deliberately
detached job survive. Fixture pidfds provide cleanup even if assertions fail.

An optimized Linux GUI probe with a native executor fixture also verified tab
close while preserving a peer shell, then normal window close with an agent in a
hidden workspace. The checked processes retired within about 102 ms and 81 ms
respectively on that run. Local evidence is
`artifacts/teardown-gui-v1/evidence.json`; no model turns or gateway task mutations
were performed. This fixture is not provider or Windows acceptance.

```sh
cargo test --locked --test terminal_lifecycle_contract
```

This is not complete lifecycle acceptance. A leader that has already exited
cannot authorize a new scan, so that path fails closed. Abrupt desktop death,
late-forking descendants, exhaustive output-flood teardown, all descendant
reaping, and native Windows teardown still need evidence. Linux pidfd support is
required for this guard; a capture failure leaves the terminal available and
reports unavailable cleanup ownership. The existing Linux launcher parent-death
protection covers its direct agent separately.

The native event loop also drains a bounded final PTY read on child exit. Without
that option, exit can arrive before the read event and lose final output. The
checked-input/closed-channel protocol test verifies the final reply before
checking transport closure. This does not establish durable history retention.
The terminal subscription also finishes when its event channel closes after
Exit, preserving queued events without polling the closed channel indefinitely.

The Windows candidate uses a private, anonymous Job Object. It opens the initial
ConPTY child through a retained native handle, checks its exact creation FILETIME,
live state and direct parent, then enrolls it in a job with
`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`. The job handle is not inherited. The guard
runs before the PTY drops; explicit retirement requests kernel termination, and
closing the last job handle also requests termination. A peer terminal is not
enrolled. Incompatible enclosing jobs or unavailable permissions report cleanup
ownership as unavailable and leave the terminal usable.

Children created after enrollment inherit the job, including background work
with a new process group or no console. The job retains ownership after the
initial child exits. Enrollment currently happens **after** process creation:
descendants created before enrollment may escape. This gap remains open; neither
project identity nor a later PID scan is treated as authority to adopt them.
The [Windows job documentation](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects)
describes the underlying inheritance and handle-close behavior.

```powershell
cargo test --locked --test windows_terminal_lifecycle_contract
```

This native ConPTY regression deliberately gates background-child creation until
after enrollment, then checks both a live and an already-exited initial child,
idempotent retirement, and a surviving peer terminal. Retained native fixture
handles provide cleanup after assertion failure. Native Windows CI and desktop
review evidence are required; a Linux cross-check cannot establish Windows
runtime or complete launch/teardown acceptance.
