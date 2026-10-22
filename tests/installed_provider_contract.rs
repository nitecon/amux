//! Optional local check: an installed, authenticated provider is required.
#![cfg(all(feature = "desktop", target_os = "linux"))]
use iced::futures::StreamExt;
use iced_term::{BackendCommand, Command, Event, Terminal, TerminalObservation, settings};
use std::{
    path::Path,
    time::{Duration, Instant},
};

#[test]
#[ignore = "requires an installed, authenticated Codex; never submits a model turn"]
fn installed_codex_echoes_and_clears_unsubmitted_input() {
    let project = Path::new(env!("CARGO_MANIFEST_DIR"));
    let fixtures = project.join("target/test-fixtures");
    std::fs::create_dir_all(&fixtures).unwrap();
    let state = tempfile::tempdir_in(fixtures).unwrap();
    let mut terminal = Terminal::new(
        940_001,
        settings::Settings {
            backend: settings::BackendSettings {
                program: env!("CARGO_BIN_EXE_amux").into(),
                args: vec![
                    "--project-dir".into(),
                    project.display().to_string(),
                    "agent".into(),
                    "codex".into(),
                ],
                working_directory: Some(project.into()),
                env: [
                    ("AMUX_STATE_DIR".into(), state.path().display().to_string()),
                    ("TERM".into(), "xterm-256color".into()),
                    ("COLORTERM".into(), "truecolor".into()),
                    ("TERM_PROGRAM".into(), "amux".into()),
                ]
                .into(),
            },
            ..Default::default()
        },
    )
    .unwrap();
    let mut events = iced::advanced::subscription::into_recipes(terminal.subscription())
        .pop()
        .unwrap()
        .stream(iced::futures::stream::empty().boxed());
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let wait_for = |terminal: &mut Terminal,
                    events: &mut iced::futures::stream::BoxStream<'static, Event>,
                    predicate: &dyn Fn(&TerminalObservation) -> bool| {
        let start = Instant::now();
        let deadline = start + Duration::from_secs(20);
        loop {
            let screen = terminal
                .observation(32_768)
                .expect("bounded complete screen");
            if predicate(&screen) {
                return start.elapsed();
            }
            let timeout = deadline.saturating_duration_since(Instant::now());
            let event = runtime
                .block_on(async { tokio::time::timeout(timeout, events.next()).await })
                .expect(
                    "installed Codex did not reach expected UI state; no model turn was submitted",
                )
                .expect("provider exited before the UI check");
            let Event::BackendCall(id, command) = event;
            assert_eq!(id, terminal.id);
            terminal.handle(Command::ProxyToBackend(command));
        }
    };
    let startup = wait_for(&mut terminal, &mut events, &|s| {
        let s = s.lines.join("\n");
        s.contains("Ask Codex to do anything") && s.contains("for agents")
    });
    let empty = terminal.observation(32_768).unwrap();
    let records = amux::identity::live_agents(state.path()).unwrap();
    assert_eq!(records.len(), 1);
    assert_eq!(
        records[0].identity.provider,
        amux::identity::Provider::Codex
    );
    let mut recipient = amux::recipient::TerminalRecipient::new(
        terminal.child_pid().unwrap(),
        amux::identity::Provider::Codex,
        project,
    )
    .unwrap();
    recipient.resolve(state.path()).unwrap();
    let mut gate = amux::readiness::ReadinessGate::default();
    assert!(
        gate.observe(&terminal, &recipient, Instant::now())
            .is_none(),
        "consent defaults off"
    );
    recipient.set_consent(true);
    assert!(
        gate.observe(&terminal, &recipient, Instant::now())
            .is_none(),
        "one screen observation is insufficient"
    );
    std::thread::sleep(Duration::from_millis(550));
    let proof = gate
        .observe(&terminal, &recipient, Instant::now())
        .expect("installed native Codex should match its supported empty prompt profile");
    assert!(proof.is_current(&terminal, &recipient, Instant::now()));
    // The old screen can still look empty while native input is buffered.
    // Resume before asserting results so failures do not leave a stopped provider.
    assert_eq!(
        unsafe { libc::kill(records[0].pid as libc::pid_t, libc::SIGSTOP) },
        0
    );
    recipient.note_human_input();
    let queued = terminal.try_enqueue_input(b"buffered-human-input".to_vec());
    std::thread::sleep(Duration::from_millis(550));
    let unchanged =
        terminal.observation(32_768).unwrap().cursor_line_styles == empty.cursor_line_styles;
    let blocked = gate
        .observe(&terminal, &recipient, Instant::now())
        .is_none();
    assert_eq!(
        unsafe { libc::kill(records[0].pid as libc::pid_t, libc::SIGCONT) },
        0
    );
    queued.unwrap();
    assert!(
        unchanged,
        "stopped provider retained its old empty composer"
    );
    assert!(
        blocked,
        "buffered human input must not be mistaken for an empty prompt"
    );
    wait_for(&mut terminal, &mut events, &|s| {
        s.lines
            .iter()
            .any(|line| line.contains("buffered-human-input"))
    });
    recipient.note_human_input();
    terminal.handle(Command::ProxyToBackend(BackendCommand::Write(vec![0x15])));
    wait_for(&mut terminal, &mut events, &|s| {
        s.cursor_line_styles == empty.cursor_line_styles
    });
    // Literal input only: no Enter, task claim or model request is sent.
    recipient.note_human_input();
    terminal.handle(Command::ProxyToBackend(BackendCommand::Write(
        b"amux-native-input-probe".to_vec(),
    )));
    let echo = wait_for(&mut terminal, &mut events, &|s| {
        s.lines
            .iter()
            .any(|line| line.contains("amux-native-input-probe"))
    });
    assert!(!proof.is_current(&terminal, &recipient, Instant::now()));
    assert!(
        gate.observe(&terminal, &recipient, Instant::now())
            .is_none(),
        "partial input defers"
    );
    recipient.note_human_input();
    terminal.handle(Command::ProxyToBackend(BackendCommand::Write(vec![0x15])));
    let clear = wait_for(&mut terminal, &mut events, &|s| {
        let s = s.lines.join("\n");
        !s.contains("amux-native-input-probe") && s.contains("Ask Codex to do anything")
    });
    // Text and cursor location alone can be identical for a placeholder and human input.
    let placeholder = "Ask Codex to do anything";
    recipient.note_human_input();
    terminal.handle(Command::ProxyToBackend(BackendCommand::Write(
        placeholder.as_bytes().to_vec(),
    )));
    wait_for(&mut terminal, &mut events, &|s| {
        s.cursor_column == 2 + placeholder.len()
    });
    recipient.note_human_input();
    terminal.handle(Command::ProxyToBackend(BackendCommand::Write(vec![0x01])));
    wait_for(&mut terminal, &mut events, &|s| s.cursor_column == 2);
    let typed_at_home = terminal.observation(32_768).unwrap();
    assert_eq!(
        empty.lines[empty.cursor_row as usize],
        typed_at_home.lines[typed_at_home.cursor_row as usize]
    );
    assert_ne!(
        empty.cursor_line_styles, typed_at_home.cursor_line_styles,
        "placeholder styling must distinguish identical text at the same cursor location"
    );
    assert!(
        gate.observe(&terminal, &recipient, Instant::now())
            .is_none(),
        "identical typed text at Home must never match an empty placeholder"
    );
    println!(
        "Empty prompt: mode={:?}, cursor=({},{}), first text style={:?}; typed-at-home style={:?}",
        empty.mode,
        empty.cursor_row,
        empty.cursor_column,
        empty.cursor_line_styles[2],
        typed_at_home.cursor_line_styles[2]
    );
    // Ctrl+U clears only before the cursor; return to End after the Home check.
    recipient.note_human_input();
    terminal.handle(Command::ProxyToBackend(BackendCommand::Write(vec![
        0x05, 0x15,
    ])));
    wait_for(&mut terminal, &mut events, &|s| {
        s.cursor_line_styles == empty.cursor_line_styles
    });
    assert!(
        gate.observe(&terminal, &recipient, Instant::now())
            .is_none()
    );
    std::thread::sleep(Duration::from_millis(550));
    let proof = gate
        .observe(&terminal, &recipient, Instant::now())
        .expect("cleared prompt settles again");
    assert!(proof.is_current(&terminal, &recipient, Instant::now()));
    recipient.set_consent(false);
    assert!(!proof.is_current(&terminal, &recipient, Instant::now()));
    println!(
        "Codex native screen timings: startup={startup:?}, input_echo={echo:?}, clear={clear:?}"
    );
}
