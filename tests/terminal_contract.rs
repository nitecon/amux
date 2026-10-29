#![cfg(all(feature = "desktop", target_os = "linux"))]

use iced_term::{BackendCommand, Command, Terminal, TerminalCellFlags, settings};
use std::os::fd::AsRawFd;
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
                    "printf 'AMUX_PID=%s\\r\\n' \"$$\"; read line; printf 'REPLY=%s\\r\\n\\033[2mDIM\\033[0m normal' \"$line\"; read next; printf '\\033[2J\\033[He\\314\\201'; exec sleep 30".into(),
                ],
                ..Default::default()
            },
            ..Default::default()
        },
    )
    .unwrap();
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
    let mut foreground: libc::pid_t = 0;
    // SAFETY: borrowed live master descriptor and initialized output storage.
    assert_eq!(
        unsafe {
            libc::ioctl(
                terminal.native_terminal_fd().as_raw_fd(),
                libc::TIOCGPGRP,
                &mut foreground,
            )
        },
        0
    );
    // SAFETY: read-only group query for the known live native child.
    assert_eq!(foreground, unsafe { libc::getpgid(pid as libc::pid_t) });
    assert_eq!(observation.lines.len(), 50);
    assert_eq!(observation.lines[0].chars().count(), 80);
    assert_eq!(observation.display_offset, 0);
    assert!(observation.cursor_row >= 0);
    assert!(
        terminal.observation(10).is_none(),
        "never truncate a screen"
    );
    terminal.handle(Command::ProxyToBackend(BackendCommand::Write(
        b"typed-marker\r".to_vec(),
    )));
    let observation = wait_for(&terminal, "REPLY=typed-marker");
    assert_eq!(observation.lines.len(), 50, "font sync must preserve rows");
    assert_eq!(
        observation.lines[0].chars().count(),
        80,
        "font sync must preserve columns"
    );
    let styled = wait_for(&terminal, "DIM normal");
    assert_eq!(styled.cursor_line_styles.len(), 80);
    assert!(
        styled.cursor_line_styles[..3]
            .iter()
            .all(|style| style.flags == TerminalCellFlags::DIM)
    );
    assert!(
        styled.cursor_line_styles[4..10]
            .iter()
            .all(|style| style.flags.is_empty())
    );
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

/// Native text/style observation is independent of provider prompt classifiers.
#[test]
fn native_grid_observation_reports_faint_and_background_styles() {
    use iced_term::{NamedColor, TerminalCellStyle, TerminalColor};
    use std::time::{Duration, Instant};
    let script = r"printf '\033[2J\033[H\033[48;5;238m› \033[2mAsk Codex\033[0m\nplain'; sleep 10";
    let terminal = Terminal::new(
        970_101,
        settings::Settings {
            backend: settings::BackendSettings {
                program: "/bin/sh".into(),
                args: vec!["-c".into(), script.into()],
                ..Default::default()
            },
            ..Default::default()
        },
    )
    .unwrap();
    let _lifetime =
        amux::terminal_lifetime::TerminalLifetime::capture(terminal.child_pid().unwrap()).unwrap();
    let deadline = Instant::now() + Duration::from_secs(10);
    let grid = loop {
        if let Some(grid) = terminal.grid_observation(32_768)
            && grid
                .observation
                .lines
                .get(1)
                .is_some_and(|l| l.starts_with("plain"))
        {
            break grid;
        }
        assert!(Instant::now() < deadline, "fixture output never observed");
        std::thread::sleep(Duration::from_millis(20));
    };
    let screen = &grid.observation;
    assert_eq!(grid.styles.len(), screen.lines.len());
    assert!(screen.lines[0].starts_with("› Ask Codex"));
    assert_eq!(grid.styles[0][0].background, TerminalColor::Indexed(238));
    assert!(!grid.styles[0][0].flags.contains(TerminalCellFlags::DIM));
    assert!(grid.styles[0][2].flags.contains(TerminalCellFlags::DIM));
    assert_eq!(grid.styles[0][2].background, TerminalColor::Indexed(238));
    assert_eq!(
        grid.styles[1][0],
        TerminalCellStyle {
            foreground: TerminalColor::Named(NamedColor::Foreground),
            background: TerminalColor::Named(NamedColor::Background),
            flags: TerminalCellFlags::empty(),
        }
    );
    assert_eq!(
        grid.styles[screen.cursor_row as usize],
        screen.cursor_line_styles
    );
    assert!(
        terminal.observation(4).is_none(),
        "budget still bounds observations"
    );
}
