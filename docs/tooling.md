# amux tooling

This document covers the open language, terminal engine, and UI choices, the
agent-facing interface, and how candidates are evaluated. Requirements are in
[features.md](features.md); system boundaries are in
[architecture.md](architecture.md). None of the choices below are decided.

## Language and UI candidates

Choices are evaluated against the architecture boundary and the measured
cmux-gtk baseline.

| Candidate | Potential fit | What must be proved |
| --- | --- | --- |
| Go local core with Go/Gio UI | Keeps most of the application in Go; GPU interfaces exist | Terminal rendering, native input, IME, accessibility, and packaging on all three OSs |
| Go local core with Rust/GPUI UI | Separates local application logic from a GPU-oriented frontend | Benefit relative to two languages, IPC/FFI, and GPUI API churn |
| Rust local core with a replacement for GTK | Reuses more existing code, the agent-tools gateway client, and platform work | Frontend quality, maintenance cost, and gateway client boundaries |
| C++/Qt Quick frontend with a suitable local core | Established cross-platform UI and custom GPU integration | Dependency footprint, integration cost, terminal behavior, and browser workflow compatibility |

Notes:

- Go does not prevent Metal, Vulkan, or Direct3D rendering. Rust graphics
  bindings alone do not justify Rust for all orchestration.
- Use existing graphics abstractions. Do not add owned GPU backends before a
  measured need exists.
- GPUI is pre-1.0 and warns of frequent breaking changes.
- C may serve narrow native bridges. Assembly or a bespoke graphics engine
  needs a demonstrated bottleneck that existing libraries cannot solve.

## Terminal engine

At the pinned CMUX baseline `dc9bc256`, [surface creation][cmux-surface] embeds
Ghostty's full runtime, renderer and PTY over OpenGL, using the `nitecon/ghostty`
fork pinned at `8481f95cd523e32f6d6416f2739066fb2226fe8c`.
[Gateway readiness checks][cmux-readiness] use bounded render-grid observations.

Ghostty's terminal semantics stay under consideration independently of its app
runtime and renderer. `libghostty-vt` and its Go bindings are a possible
cross-platform foundation, but `vt.h` states the API "is not yet stable and is
definitely going to change". The VT library supplies terminal state only, not a
renderer, PTY, or widget. Rendering, font shaping and fallback, selection, IME,
accessibility, clipboard, terminal graphics, and resize behavior still need
integration and parity evidence.

Any terminal option must account for these behaviors. Current ABI names are
implementation evidence, not requirements to preserve those names or select
Ghostty:

| Terminal capability | Pinned implementation evidence / limit |
| --- | --- |
| Bounded prompt-grid observation for readiness | [text.rs:175-202][cmux-grid] calls `ghostty_surface_render_grid_json_v2` |
| Bounded screen-tail export without altering selection/viewport | [text.rs:148-174][cmux-tail] calls `ghostty_surface_read_screen_tail_vt` |
| Restore styled terminal content before fresh child output | [surface.rs:191-205][cmux-replay] calls `ghostty_surface_new_with_replay`; this is not process checkpointing |
| Native foreground/child process identity and TTY metadata | [tty.rs:8-40][cmux-tty] uses `ghostty_surface_tty_name`, POSIX `ghostty_surface_foreground_pid` or Windows `ghostty_surface_child_pid`; platform process inspection supplies generation identity |
| Working-directory inheritance for splits/tabs | [inherited.rs:18-31][cmux-inherited] calls `ghostty_surface_inherited_config`; [split_engine.rs:907-956][cmux-splits] consumes native inherited context |
| Terminal-protocol key input bypassing host keybindings | `ghostty_surface_key_to_process` is a [fork capability][ghostty-key], not a Rust call-site dependency in the pinned CMUX source. Its [implementation][ghostty-key-impl] bypasses bindings; it does not authenticate an agent recipient. Equivalent behavior must be evaluated where needed. |

Recipient protection is separate from key encoding. Immediately before literal
paste and native Enter, [gateway/mod.rs:138-208][cmux-delivery] rechecks native
process identity, workspace/directory/foreground PID, input generation, empty
prompt readiness, freshness and explicit consent. Preserve those behaviors when
changing engines; a key adapter or successful input write proves neither recipient
authentication nor agent execution. See [architecture.md](architecture.md) for
the delivery evidence contract.

Where terminal state lives and how the frontend consumes it is open. A separate
process or terminal display protocol must earn its complexity through specific
lifecycle or performance requirements. A mandatory background terminal host is
not an approved requirement, and gateway-held tasks remove task tracking as a
reason for one.

## Agent-facing interface

An amux CLI/control API is the proposed built-in agent interface. Exact
commands and schemas are not yet selected. It covers the agent capabilities in
[features.md](features.md) and uses the same operations as the desktop.

agent-tools stays usable against the same gateway. Task semantics must not be
maintained separately in the UI, the amux CLI, and agent-tools. agent-tools is
Rust, with its task client in `crates/agent-comms/src/tasks.rs`. If amux uses
Rust, a shared gateway client library is a reuse candidate, subject to extraction,
API stability and build/integration cost. With another language, the gateway HTTP
contract remains the shared boundary. Compare those costs without selecting a
language or library packaging in advance.

## Evaluation gates

This is a proposed feasibility plan, separate from owner decisions D1-D6. It
does not authorize prototypes, dependency installation, gateway implementation,
or a language/toolkit/process-model selection under this documentation task.

1. Delegate verified gateway gaps to agent-gateway. The recommended initial
   connection is to an existing deployment, with bundling deferred; packaging
   remains open. Prove the [handoff cases](features.md#acceptance-scenarios)
   before accepting production cross-machine execution. Independent terminal/UI
   feasibility comparisons may proceed separately; there is no blanket ban on
   UI evaluation before gateway work.
2. Start with one representative terminal/UI candidate and a measured CMUX
   baseline, using the same workload and documented machine/OS configuration.
   An early comparison on one OS can reject a poor fit cheaply. Do not prototype
   every candidate or build custom GPU backends; use existing abstractions.
3. Before committing to a stack, obtain evidence for its difficult integration
   paths on Linux, Windows and macOS. Existing Linux capability and Windows
   preview availability are the baseline in [features.md](features.md), not proof
   that a new stack meets future acceptance. macOS is a new target whose final
   acceptance scope remains open; an unresolved platform risk must be reported,
   not treated as a pass.

| Area | Evidence to collect in a later authorized evaluation |
| --- | --- |
| Rendering and output | Representative sustained/burst output throughput, resize behavior and rendering correctness; record workload and capture method |
| Typing and resources | Input latency, idle CPU and memory under matched conditions; report raw observations and measurement method |
| Native text/input | IME composition, shortcuts, clipboard/selection, font shaping/fallback and accessibility paths on each OS |
| Safe reminders | Bounded prompt observations and native process identity; direct literal input/Enter with consent, readiness, partial-input protection, uncertain submission and deduplication as in the delivery contract |
| Integration and packaging | PTY/rendering boundary, browser service compatibility, dependency footprint, native bridges, reproducible build/package effort and platform-specific gaps |

The report should identify exact CMUX/candidate/library revisions, platform and
hardware, workload, commands/tools, results and artifacts, failures, unresolved
risks, and integration/maintenance costs. Compare against the baseline and list
what remains unproved. Choose acceptance thresholds through a later decision;
none are invented here. No benchmark results, proven parity, mandatory separate
terminal process, or stack choice are implied by this plan.

## Research references

Verify versions and behavior again when selecting dependencies.

- [Ghostty and its embeddable library](https://github.com/ghostty-org/ghostty)
- [Ghostling: scope of the VT library and a minimal consumer](https://github.com/ghostty-org/ghostling)
- [Ghostty VT C API and stability warning](https://github.com/ghostty-org/ghostty/blob/main/include/ghostty/vt.h)
- [Go libghostty bindings, cgo, and cross-compilation](https://tangled.org/mitchellh.com/go-libghostty/tree/main)
- [Gio platform support](https://gioui.org/) and [GPU interfaces](https://pkg.go.dev/gioui.org/gpu)
- [GPUI development status](https://github.com/zed-industries/zed/tree/main/crates/gpui)
- [wgpu graphics backends](https://wgpu.rs/doc/wgpu/documentation/features/index.html)
- [Qt Quick custom graphics integration](https://doc.qt.io/qt-6/qtquick-visualcanvas-scenegraph.html)
- [Qt WebEngine extension manager](https://doc.qt.io/qt-6.10/qwebengineextensionmanager.html)
- [WezTerm multiplexing as an optional session-lifecycle reference](https://wezterm.org/multiplexing.html)

[cmux-surface]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/src/ghostty/surface.rs
[cmux-readiness]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/src/gateway/readiness.rs
[cmux-grid]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/src/ghostty/text.rs#L175-L202
[cmux-tail]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/src/ghostty/text.rs#L148-L174
[cmux-replay]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/src/ghostty/surface.rs#L191-L205
[cmux-tty]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/src/ghostty/tty.rs#L8-L40
[cmux-inherited]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/src/ghostty/inherited.rs#L18-L31
[cmux-splits]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/src/split_engine.rs#L907-L956
[cmux-delivery]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/src/gateway/mod.rs#L138-L208
[ghostty-key]: https://github.com/nitecon/ghostty/blob/8481f95cd523e32f6d6416f2739066fb2226fe8c/include/ghostty.h#L1475-L1478
[ghostty-key-impl]: https://github.com/nitecon/ghostty/blob/8481f95cd523e32f6d6416f2739066fb2226fe8c/src/Surface.zig#L4395-L4410
