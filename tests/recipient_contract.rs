#![cfg(all(feature = "desktop", target_os = "linux"))]
use amux::{identity::Provider, recipient::TerminalRecipient};
use iced_term::{BackendCommand, Command, Terminal, settings};
use std::{
    collections::HashSet,
    fs,
    os::unix::fs::PermissionsExt,
    path::Path,
    time::{Duration, Instant},
};

#[test]
fn three_native_agent_terminals_bind_exactly_and_invalidate_stale_checkpoints() {
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&target).unwrap();
    let fixture = tempfile::tempdir_in(target).unwrap();
    let root = fixture.path();
    fs::write(
        root.join("codex"),
        "#!/bin/sh\nprintf 'AGENT_READY\\r\\n'\nread stop\n",
    )
    .unwrap();
    fs::set_permissions(root.join("codex"), fs::Permissions::from_mode(0o700)).unwrap();
    let path = std::env::join_paths(std::iter::once(root.to_path_buf()).chain(
        std::env::split_paths(&std::env::var_os("PATH").unwrap_or_default()),
    ))
    .unwrap();
    let registry = root.join("state");
    let mut terminals = Vec::new();
    let mut recipients = Vec::new();
    for id in 1..=3 {
        let terminal = Terminal::new(
            920_000 + id,
            settings::Settings {
                backend: settings::BackendSettings {
                    program: env!("CARGO_BIN_EXE_amux").into(),
                    args: vec![
                        "--project-dir".into(),
                        root.display().to_string(),
                        "agent".into(),
                        "codex".into(),
                    ],
                    working_directory: Some(root.into()),
                    env: [
                        ("PATH".into(), path.to_string_lossy().into()),
                        ("AMUX_STATE_DIR".into(), registry.display().to_string()),
                    ]
                    .into(),
                },
                ..Default::default()
            },
        )
        .unwrap();
        recipients.push(
            TerminalRecipient::new(terminal.child_pid().unwrap(), Provider::Codex, root).unwrap(),
        );
        terminals.push(terminal);
    }
    let deadline = Instant::now() + Duration::from_secs(10);
    let identities = loop {
        let identities: Vec<_> = recipients
            .iter_mut()
            .filter_map(|r| r.resolve(&registry).ok().cloned())
            .collect();
        if identities.len() == 3 {
            break identities;
        }
        assert!(Instant::now() < deadline, "agent terminals failed to bind");
        std::thread::sleep(Duration::from_millis(25));
    };
    assert_eq!(
        identities
            .iter()
            .map(|a| a.session_id)
            .collect::<HashSet<_>>()
            .len(),
        3
    );
    assert_eq!(
        identities
            .iter()
            .map(|a| a.instance_id)
            .collect::<HashSet<_>>()
            .len(),
        3
    );
    assert!(recipients[0].checkpoint().is_none(), "consent defaults off");
    for recipient in &mut recipients {
        recipient.set_consent(true);
    }
    let checkpoints: Vec<_> = recipients.iter().map(|r| r.checkpoint().unwrap()).collect();
    for (index, recipient) in recipients.iter().enumerate() {
        assert!(recipient.is_current(&checkpoints[index]));
        assert!(
            !recipient.is_current(&checkpoints[(index + 1) % 3]),
            "another same-project terminal is not this recipient"
        );
    }
    recipients[0].note_human_input();
    assert!(!recipients[0].is_current(&checkpoints[0]));
    assert!(recipients[1].is_current(&checkpoints[1]));
    recipients[1].set_consent(false);
    recipients[1].set_consent(true);
    assert!(
        !recipients[1].is_current(&checkpoints[1]),
        "restoring consent cannot revive an old checkpoint"
    );
    terminals[2].handle(Command::ProxyToBackend(BackendCommand::Write(
        b"stop\r".to_vec(),
    )));
    let deadline = Instant::now() + Duration::from_secs(10);
    while recipients[2].is_current(&checkpoints[2]) {
        assert!(Instant::now() < deadline, "exited agent remained current");
        std::thread::sleep(Duration::from_millis(25));
    }
    assert!(
        recipients[2].resolve(&registry).is_err(),
        "never replace an exited recipient with a peer"
    );
}
