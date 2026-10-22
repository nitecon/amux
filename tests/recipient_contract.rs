#![cfg(all(feature = "desktop", target_os = "linux"))]
use amux::{
    delivery::{DeliveryCause, DeliveryLedger, DeliveryState},
    identity::Provider,
    recipient::TerminalRecipient,
};
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
    let addresses: Vec<_> = recipients
        .iter()
        .zip(&terminals)
        .map(|(recipient, terminal)| recipient.address(terminal.id).unwrap())
        .collect();
    let ledger_path = root.join("deliveries.json");
    let mut ledger = DeliveryLedger::open(&ledger_path).unwrap();
    assert!(
        DeliveryLedger::open(&ledger_path).is_err(),
        "only one ledger writer"
    );
    let task = uuid::Uuid::new_v4();
    let cause = DeliveryCause::Notification {
        event_id: uuid::Uuid::new_v4(),
    };
    let delivery = ledger
        .queue(task, addresses[0].clone(), cause.clone(), 10_000, 10_000)
        .unwrap();
    assert_eq!(
        ledger
            .queue(task, addresses[0].clone(), cause.clone(), 10_000, 10_001)
            .unwrap(),
        delivery
    );
    assert!(
        ledger
            .queue(
                uuid::Uuid::new_v4(),
                addresses[0].clone(),
                cause.clone(),
                10_000,
                10_001
            )
            .is_err(),
        "event ID cannot conceal different work"
    );
    assert!(
        ledger
            .queue(task, addresses[0].clone(), cause.clone(), 10_000, 310_001)
            .is_err(),
        "old event cannot be made fresh on reconnect"
    );
    assert!(
        ledger
            .queue(
                task,
                addresses[0].clone(),
                DeliveryCause::RecoveryQuery,
                20_000,
                10_000
            )
            .is_err(),
        "future timestamp defers"
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
    assert!(
        ledger
            .begin_attempt(
                delivery,
                terminals[1].id,
                &recipients[1],
                &checkpoints[1],
                10_001
            )
            .is_err(),
        "same-project peer is not a delivery target"
    );
    assert!(
        ledger
            .begin_attempt(
                delivery,
                terminals[0].id,
                &recipients[1],
                &checkpoints[1],
                10_001
            )
            .is_err(),
        "copying terminal ID cannot retarget a recipient"
    );
    let refresh = ledger
        .queue(
            task,
            addresses[1].clone(),
            DeliveryCause::RecoveryQuery,
            10_000,
            10_000,
        )
        .unwrap();
    assert_eq!(
        ledger
            .queue(
                task,
                addresses[1].clone(),
                DeliveryCause::RecoveryQuery,
                400_000,
                400_000
            )
            .unwrap(),
        refresh,
        "fresh authoritative recovery preserves reminder identity"
    );
    let attempt = ledger
        .begin_attempt(
            refresh,
            terminals[1].id,
            &recipients[1],
            &checkpoints[1],
            400_001,
        )
        .unwrap();
    ledger.mark_enqueued(attempt).unwrap();
    assert!(
        ledger
            .begin_attempt(
                refresh,
                terminals[1].id,
                &recipients[1],
                &checkpoints[1],
                400_002
            )
            .is_err(),
        "enqueued reminder is not retried"
    );
    for (index, recipient) in recipients.iter().enumerate() {
        assert!(recipient.is_current(&checkpoints[index]));
        assert!(
            !recipient.is_current(&checkpoints[(index + 1) % 3]),
            "another same-project terminal is not this recipient"
        );
    }
    recipients[0].note_human_input();
    assert!(!recipients[0].is_current(&checkpoints[0]));
    assert!(
        ledger
            .begin_attempt(
                delivery,
                terminals[0].id,
                &recipients[0],
                &checkpoints[0],
                10_002
            )
            .is_err(),
        "partial human input invalidates attempt checkpoint"
    );
    let fresh_checkpoint = recipients[0].checkpoint().unwrap();
    let _uncertain = ledger
        .begin_attempt(
            delivery,
            terminals[0].id,
            &recipients[0],
            &fresh_checkpoint,
            10_002,
        )
        .unwrap();
    assert!(
        ledger
            .begin_attempt(
                delivery,
                terminals[0].id,
                &recipients[0],
                &fresh_checkpoint,
                10_003
            )
            .is_err(),
        "uncertain submission is fenced before any terminal write"
    );
    assert!(recipients[1].is_current(&checkpoints[1]));
    recipients[1].set_consent(false);
    let retired = ledger
        .queue(
            uuid::Uuid::new_v4(),
            addresses[1].clone(),
            DeliveryCause::RecoveryQuery,
            400_000,
            400_000,
        )
        .unwrap();
    ledger.retire_recipient(&addresses[1]).unwrap();
    recipients[1].set_consent(true);
    assert!(
        !recipients[1].is_current(&checkpoints[1]),
        "restoring consent cannot revive an old checkpoint"
    );
    let checkpoint = recipients[1].checkpoint().unwrap();
    assert!(
        ledger
            .begin_attempt(
                retired,
                terminals[1].id,
                &recipients[1],
                &checkpoint,
                400_001
            )
            .is_err(),
        "consent grant never revives retired pending input"
    );
    let cold_pending = ledger
        .queue(
            uuid::Uuid::new_v4(),
            addresses[2].clone(),
            DeliveryCause::RecoveryQuery,
            400_000,
            400_000,
        )
        .unwrap();
    let withdrawn = ledger
        .queue(
            uuid::Uuid::new_v4(),
            addresses[0].clone(),
            DeliveryCause::RecoveryQuery,
            400_000,
            400_000,
        )
        .unwrap();
    ledger.retire_terminal(terminals[0].id).unwrap();
    assert_eq!(
        ledger
            .records()
            .iter()
            .find(|r| r.id == withdrawn)
            .unwrap()
            .state,
        DeliveryState::Retired
    );
    assert_eq!(
        ledger
            .records()
            .iter()
            .find(|r| r.id == cold_pending)
            .unwrap()
            .state,
        DeliveryState::Pending,
        "withdrawing one terminal must preserve a peer's queue"
    );
    ledger.retire_pending(refresh).unwrap();
    ledger.retire_pending(delivery).unwrap();
    assert_eq!(
        ledger
            .records()
            .iter()
            .find(|r| r.id == refresh)
            .unwrap()
            .state,
        DeliveryState::Enqueued,
        "task changes must preserve accepted input history"
    );
    assert_eq!(
        ledger
            .records()
            .iter()
            .find(|r| r.id == delivery)
            .unwrap()
            .state,
        DeliveryState::Uncertain,
        "withdrawal must preserve uncertain input history"
    );
    let changed = ledger
        .queue(
            uuid::Uuid::new_v4(),
            addresses[1].clone(),
            DeliveryCause::RecoveryQuery,
            400_000,
            400_000,
        )
        .unwrap();
    ledger.retire_pending(changed).unwrap();
    assert_eq!(
        ledger
            .records()
            .iter()
            .find(|r| r.id == changed)
            .unwrap()
            .state,
        DeliveryState::Retired
    );
    drop(ledger);
    let mut ledger = DeliveryLedger::open(&ledger_path).unwrap();
    for (id, state) in [
        (delivery, DeliveryState::Uncertain),
        (refresh, DeliveryState::Enqueued),
        (retired, DeliveryState::Retired),
        (cold_pending, DeliveryState::Retired),
    ] {
        assert_eq!(
            ledger
                .records()
                .iter()
                .find(|record| record.id == id)
                .unwrap()
                .state,
            state,
            "crash/reopen cannot erase attempt history or revive queued input"
        );
    }
    assert_eq!(
        ledger
            .queue(task, addresses[0].clone(), cause, 10_000, 10_004)
            .unwrap(),
        delivery
    );
    assert!(
        ledger
            .begin_attempt(
                delivery,
                terminals[0].id,
                &recipients[0],
                &fresh_checkpoint,
                10_004
            )
            .is_err()
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
