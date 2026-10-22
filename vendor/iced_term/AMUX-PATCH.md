# iced_term observation patch

Source: crates.io `iced_term` 0.8.0, upstream
<https://github.com/Harzu/iced_term>, revision
`e0e90be2c160bd68f1bf544a51bf772d75b98ffc` from the crate's VCS metadata.
The original MIT license is in LICENSE.

The source is vendored so amux can use two small additions without modifying
the Cargo registry: the initial native PTY child PID and a bounded live screen
observation. Changes are in src/backend.rs, src/terminal.rs and src/lib.rs.
The normalized Cargo manifest omits the upstream README field because that
file is not bundled here. All other source files are unchanged.

Observations lock the live parser grid, exclude scrollback, and fail when the
screen exceeds the caller's budget or 32,768 cells. They contain base characters,
cursor coordinates, display offset and terminal modes. A screen containing
combining characters also returns no observation. A PID may be stale or identify a launcher, and screen content
does not prove readiness, recipient identity or agent acknowledgment. amux must
check process generation, exact recipient, human input and consent separately.

This remains a provisional terminal candidate. No reminder path is enabled by
these accessors alone.
