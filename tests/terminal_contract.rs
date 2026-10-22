#![cfg(all(feature = "desktop", target_os = "linux"))]

use iced_term::{BackendCommand, Command, Terminal, settings};
use std::time::{Duration, Instant};

#[test]
fn native_pty_observation_tracks_the_child_and_live_input_without_a_redraw() {
    let mut terminal = Terminal::new(
        910_001,
        settings::Settings {
            backend: settings::BackendSettings {
                program: "/bin/sh".into(),
                args: vec![
                    "-c".into(),
                    "printf 'AMUX_PID=%s\\r\\n' \"$$\"; read line; printf 'REPLY=%s\\r\\n' \"$line\"; read next; printf '\\033[2J\\033[He\\314\\201'; exec sleep 30".into(),
                ],
                ..Default::default()
            },
            ..Default::default()
        },
    )
    .unwrap();
    terminal.handle(Command::ProxyToBackend(BackendCommand::Resize(
        Some(iced::Size::new(800.0, 600.0)),
        None,
    )));
    let pid = terminal.child_pid().expect("native PTY child PID");
    let wait_for = |terminal: &Terminal, marker: &str| {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            let observation = terminal.observation(32_768).unwrap();
            if observation.lines.iter().any(|line| line.contains(marker)) {
                return observation;
            }
            assert!(
                Instant::now() < deadline,
                "missing native output: {marker}; screen={:?}",
                observation.lines
            );
            std::thread::sleep(Duration::from_millis(20));
        }
    };
    let observation = wait_for(&terminal, &format!("AMUX_PID={pid}"));
    assert_eq!(observation.display_offset, 0);
    assert!(observation.cursor_row >= 0);
    assert!(
        terminal.observation(10).is_none(),
        "never truncate a screen"
    );
    terminal.handle(Command::ProxyToBackend(BackendCommand::Write(
        b"typed-marker\r".to_vec(),
    )));
    wait_for(&terminal, "REPLY=typed-marker");
    terminal.handle(Command::ProxyToBackend(BackendCommand::Write(
        b"next\r".to_vec(),
    )));
    let deadline = Instant::now() + Duration::from_secs(5);
    while terminal.observation(32_768).is_some() {
        assert!(
            Instant::now() < deadline,
            "incomplete combining text must defer observation"
        );
        std::thread::sleep(Duration::from_millis(20));
    }
    drop(terminal);
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        let status = std::fs::read_to_string(format!("/proc/{pid}/stat"));
        let alive = status.ok().is_some_and(|stat| {
            let state = stat.rsplit_once(')').unwrap().1.trim_start().as_bytes()[0];
            state != b'Z' && state != b'X'
        });
        if !alive {
            break;
        }
        assert!(
            Instant::now() < deadline,
            "closed native PTY child stayed live"
        );
        std::thread::sleep(Duration::from_millis(20));
    }
}
