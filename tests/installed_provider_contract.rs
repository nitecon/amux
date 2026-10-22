//! Optional local check: an installed, authenticated provider is required.
#![cfg(all(feature = "desktop", target_os = "linux"))]
use iced::futures::StreamExt;
use iced_term::{BackendCommand, Command, Event, Terminal, settings};
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
                    predicate: &dyn Fn(&str) -> bool| {
        let start = Instant::now();
        let deadline = start + Duration::from_secs(20);
        loop {
            let screen = terminal
                .observation(32_768)
                .expect("bounded complete screen")
                .lines
                .join("\n");
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
        s.contains("Ask Codex to do anything") && s.contains("for agents")
    });
    let records = amux::identity::live_agents(state.path()).unwrap();
    assert_eq!(records.len(), 1);
    assert_eq!(
        records[0].identity.provider,
        amux::identity::Provider::Codex
    );
    // Literal input only: no Enter, task claim or model request is sent.
    terminal.handle(Command::ProxyToBackend(BackendCommand::Write(
        b"amux-native-input-probe".to_vec(),
    )));
    let echo = wait_for(&mut terminal, &mut events, &|s| {
        s.contains("amux-native-input-probe")
    });
    terminal.handle(Command::ProxyToBackend(BackendCommand::Write(vec![0x15])));
    let clear = wait_for(&mut terminal, &mut events, &|s| {
        !s.contains("amux-native-input-probe") && s.contains("Ask Codex to do anything")
    });
    println!(
        "Codex native screen timings: startup={startup:?}, input_echo={echo:?}, clear={clear:?}"
    );
}
