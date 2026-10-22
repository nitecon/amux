//! Opt-in native input check. Never sends Enter, starts a model turn or claims a task.
#![cfg(all(feature = "desktop", target_os = "linux"))]
use amux::{
    identity::Provider, readiness::ReadinessGate, recipient::TerminalRecipient,
    terminal_lifetime::TerminalLifetime,
};
use iced::futures::{StreamExt, stream::BoxStream};
use iced_term::{Command, Event, Terminal, TerminalObservation, settings};
use std::{
    path::Path,
    time::{Duration, Instant},
};

fn observe_until(
    terminal: &mut Terminal,
    events: &mut BoxStream<'static, Event>,
    runtime: &tokio::runtime::Runtime,
    predicate: impl Fn(&TerminalObservation) -> bool,
) -> TerminalObservation {
    let deadline = Instant::now() + Duration::from_secs(20);
    loop {
        if let Some(screen) = terminal.observation(32_768)
            && predicate(&screen)
        {
            return screen;
        }
        assert!(
            Instant::now() < deadline,
            "Claude did not reach the expected observable input state; no Enter submitted"
        );
        if let Ok(Some(Event::BackendCall(id, command))) = runtime.block_on(async {
            tokio::time::timeout(Duration::from_millis(20), events.next()).await
        }) {
            assert_eq!(id, terminal.id);
            assert_ne!(
                terminal.handle(Command::ProxyToBackend(command)),
                iced_term::actions::Action::Shutdown,
                "provider exited before the input check"
            );
        }
    }
}

fn idle(screen: &TerminalObservation) -> bool {
    let Ok(row) = usize::try_from(screen.cursor_row) else {
        return false;
    };
    screen.cursor_column == 2
        && screen
            .lines
            .iter()
            .take(4)
            .any(|line| line.contains("Claude Code v2.1.308"))
        && screen
            .lines
            .get(row)
            .is_some_and(|line| line.starts_with("❯\u{a0}Try \""))
        && screen
            .lines
            .get(row + 2)
            .is_some_and(|line| line.contains("for agents"))
}

#[test]
#[ignore = "requires installed authenticated Claude2.1.308; types and clears input, never sends Enter or a model turn"]
fn installed_claude_preserves_partial_input_including_identical_placeholder_at_home() {
    let project = Path::new(env!("CARGO_MANIFEST_DIR"));
    let fixtures = project.join("target/test-fixtures");
    std::fs::create_dir_all(&fixtures).unwrap();
    let state = tempfile::tempdir_in(fixtures).unwrap();
    let mut terminal = Terminal::new(
        970_001,
        settings::Settings {
            backend: settings::BackendSettings {
                program: env!("CARGO_BIN_EXE_amux").into(),
                args: vec![
                    "--project-dir".into(),
                    project.display().to_string(),
                    "agent".into(),
                    "claude".into(),
                ],
                working_directory: Some(project.into()),
                env: [
                    ("AMUX_STATE_DIR".into(), state.path().display().to_string()),
                    ("TERM".into(), "xterm-256color".into()),
                    ("COLORTERM".into(), "truecolor".into()),
                    ("TERM_PROGRAM".into(), "amux".into()),
                    ("NO_COLOR".into(), "1".into()),
                ]
                .into(),
            },
            ..Default::default()
        },
    )
    .unwrap();
    // Retire this owned native session before the PTY drops, including on failure.
    let _lifetime = TerminalLifetime::capture(terminal.child_pid().unwrap()).unwrap();
    let mut recipient =
        TerminalRecipient::new(terminal.child_pid().unwrap(), Provider::Claude, project).unwrap();
    let mut events = iced::advanced::subscription::into_recipes(terminal.subscription())
        .pop()
        .unwrap()
        .stream(iced::futures::stream::empty().boxed());
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let empty = observe_until(&mut terminal, &mut events, &runtime, idle);
    recipient
        .resolve(state.path())
        .expect("native Claude must bind to its own registered execution");
    assert_eq!(
        recipient.address(terminal.id).unwrap().identity().provider,
        Provider::Claude
    );
    recipient.set_consent(true);
    let mut gate = ReadinessGate::default();
    let _ = gate.observe(&terminal, &recipient, Instant::now());
    std::thread::sleep(Duration::from_millis(550));
    let idle_profile_ready = gate
        .observe(&terminal, &recipient, Instant::now())
        .is_some();
    let row = usize::try_from(empty.cursor_row).unwrap();
    let placeholder = empty.lines[row].trim_end().strip_prefix("❯\u{a0}").unwrap();
    assert!(placeholder.is_ascii());
    let mut input = placeholder.as_bytes().to_vec();
    input.push(1); // Literal text, then Home, never Enter.
    recipient.note_human_input();
    terminal.try_enqueue_input(input).unwrap();
    let typed = observe_until(&mut terminal, &mut events, &runtime, |screen| {
        screen.cursor_row == empty.cursor_row
            && screen.cursor_column == empty.cursor_column
            && screen.lines[row] == empty.lines[row]
            && !screen.lines[row + 2].contains("for agents")
    });
    assert_eq!(
        typed.cursor_line_styles, empty.cursor_line_styles,
        "monochrome placeholder and typed text at Home have identical styles"
    );
    let _ = gate.observe(&terminal, &recipient, Instant::now());
    std::thread::sleep(Duration::from_millis(550));
    assert!(
        gate.observe(&terminal, &recipient, Instant::now())
            .is_none(),
        "typed input must never become ready after settling"
    );
    recipient.note_human_input();
    terminal.try_enqueue_input(vec![5, 21]).unwrap(); // End then Ctrl+U.
    let cleared = observe_until(&mut terminal, &mut events, &runtime, idle);
    recipient.note_human_input();
    terminal.try_enqueue_input(vec![b' ', 1]).unwrap();
    let _space = observe_until(&mut terminal, &mut events, &runtime, |screen| {
        screen.cursor_column == 2
            && !screen.lines[row + 2].contains("for agents")
            && screen.lines[row].trim_end() == "❯"
    });
    let _ = gate.observe(&terminal, &recipient, Instant::now());
    std::thread::sleep(Duration::from_millis(550));
    assert!(
        gate.observe(&terminal, &recipient, Instant::now())
            .is_none(),
        "invisible whitespace is still human input"
    );
    recipient.note_human_input();
    terminal.try_enqueue_input(vec![5, 21]).unwrap();
    observe_until(&mut terminal, &mut events, &runtime, idle);
    recipient.set_consent(false);
    assert!(
        gate.observe(&terminal, &recipient, Instant::now())
            .is_none()
    );
    std::fs::create_dir_all(project.join("artifacts")).unwrap();
    let evidence = serde_json::json!({"provider":"Claude2.1.308","no_color":true,"real_model_turns":0,"enter_bytes_sent":0,
        "native_recipient_bound":true,"typed_placeholder_same_text_cursor_and_style":true,"partial_input_deferred":true,
        "whitespace_deferred":true,"cleared":idle(&cleared),"idle_readiness_profile_ready":idle_profile_ready,
        "busy_continuation_acceptance":false});
    std::fs::write(
        project.join("artifacts/claude-native-input-evidence.json"),
        serde_json::to_vec_pretty(&evidence).unwrap(),
    )
    .unwrap();
    println!("{evidence}");
}
