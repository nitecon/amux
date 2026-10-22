# iced_term observation patch

Source: crates.io `iced_term` 0.8.0, upstream
<https://github.com/Harzu/iced_term>, revision
`e0e90be2c160bd68f1bf544a51bf772d75b98ffc` from the crate's VCS metadata.
The original MIT license is in LICENSE. The upstream rustfmt.toml is retained
so `cargo fmt --all` checks the vendored source with its original formatting.

The source is vendored so amux can use two small additions without modifying
the Cargo registry: the initial native PTY child PID and a bounded live screen
observation. Changes are in src/backend.rs, src/terminal.rs and src/lib.rs.
The initial native PTY geometry also uses the font's cell size, preserving the
default 80 by 50 grid during font synchronization before a widget resize arrives.
src/bindings.rs corrects Ctrl+U and Ctrl+Shift+U to send the control byte 0x15
instead of the printable character Q (0x51).
Terminal color queries now receive the configured palette color, and text-area
size queries receive the current native pixel dimensions. These replies use the
existing PTY response path without scrolling or accessing the clipboard.
The normalized Cargo manifest omits the upstream README field because that
file is not bundled here. All other source files are unchanged.

Observations lock the live parser grid, exclude scrollback, and fail when the
screen exceeds the caller's budget or 32,768 cells. They contain base characters,
cursor coordinates, display offset and terminal modes. A screen containing
combining characters also returns no observation. A PID may be stale or identify a launcher, and screen content
does not prove readiness, recipient identity or agent acknowledgment. amux must
check process generation, exact recipient, human input and consent separately.

Observations also include parser styling for the cursor's row, with one entry
per terminal column. This lets callers distinguish a dim placeholder from typed
text with the same characters and cursor position. The existing cell budget
bounds this row too. On Linux the adapter retains a duplicate of the owned PTY
master and exposes a borrowed descriptor for foreground-group verification.
It closes with the terminal; callers do not discover a PTY by searching PIDs.

This remains a provisional terminal candidate. No reminder path is enabled by
these accessors alone.

Linux native PTY tests in `terminal.rs` verify synchronized output reaches the
cached render snapshot and query responses match the configured background and
the PTY's actual `TIOCGWINSZ` dimensions. Run them with
`cargo test --locked -p iced_term amux_tests`. They do not measure GUI latency or
establish provider readiness. The optional ignored root test
`installed_provider_contract` requires installed, authenticated Codex and checks
unsubmitted input and clearing through the native event stream; it never sends
Enter or submits a model turn.
It also verifies that typing the placeholder and moving to Home cannot pass the
provisional native prompt profile. The profile is not connected to automatic
desktop reminder submission. The separate opt-in `provider_turn_contract` test
submits one real no-tools/no-files reply request to check busy/continuation and
the durable checked-input coordinator; it is ignored in normal tests and CI.

`Terminal::try_enqueue_input` reports the existing native input channel's send
result, unlike the widget's fire-and-forget input path. An error can follow
channel acceptance, and success does not prove a PTY write or agent
acknowledgment. Callers retain uncertainty across failures and crashes.
