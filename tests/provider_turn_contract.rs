//! Explicit opt-in only: this test submits ONE real model turn, unlike the input-only probe.
#![cfg(all(feature = "desktop", target_os = "linux"))]
use amux::{
    delivery::{DeliveryCause, DeliveryLedger, DeliveryState},
    identity::Provider,
    readiness::ReadinessGate,
    recipient::TerminalRecipient,
    submission::{Submission, submit_literal},
};
use iced::futures::StreamExt;
use iced_term::{Command, Event, Terminal, settings};
use std::{
    path::Path,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

#[test]
#[ignore = "requires authenticated Codex; submits one real no-tools/no-files reply request"]
fn installed_codex_defers_during_a_turn_and_becomes_ready_for_continuation() {
    let project = Path::new(env!("CARGO_MANIFEST_DIR"));
    let fixtures = project.join("target/test-fixtures");
    std::fs::create_dir_all(&fixtures).unwrap();
    let state = tempfile::tempdir_in(fixtures).unwrap();
    let mut terminal = Terminal::new(
        950_001,
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
    let mut recipient =
        TerminalRecipient::new(terminal.child_pid().unwrap(), Provider::Codex, project).unwrap();
    recipient.set_consent(true);
    let mut gate = ReadinessGate::default();
    let startup_deadline = Instant::now() + Duration::from_secs(20);
    let tick = |terminal: &mut Terminal,
                events: &mut iced::futures::stream::BoxStream<'static, Event>| {
        if let Ok(Some(event)) = runtime.block_on(async {
            tokio::time::timeout(Duration::from_millis(20), events.next()).await
        }) {
            let Event::BackendCall(id, command) = event;
            assert_eq!(id, terminal.id);
            terminal.handle(Command::ProxyToBackend(command));
        }
    };
    loop {
        let _ = recipient.resolve(state.path());
        if gate
            .observe(&terminal, &recipient, Instant::now())
            .is_some()
        {
            break;
        }
        assert!(
            Instant::now() < startup_deadline,
            "initial idle profile never settled; no turn sent"
        );
        tick(&mut terminal, &mut events);
    }
    let request = "Reply with exactly AMUX_SMOKE_ACK. Do not use tools, run commands, claim tasks, or change any files.";
    let now_ms = || {
        u64::try_from(
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_millis(),
        )
        .unwrap()
    };
    let mut ledger = DeliveryLedger::open(&state.path().join("deliveries.json")).unwrap();
    // This is a local transport fixture, not a gateway-accepted task or claim.
    let id = ledger
        .queue(
            uuid::Uuid::new_v4(),
            recipient.address(terminal.id).unwrap(),
            DeliveryCause::RecoveryQuery,
            now_ms(),
            now_ms(),
        )
        .unwrap();
    assert!(
        submit_literal(
            &mut ledger,
            &mut gate,
            &terminal,
            &mut recipient,
            id,
            "unsafe\rcontrol",
            now_ms()
        )
        .is_err()
    );
    assert_eq!(ledger.records()[0].state, DeliveryState::Pending);
    let admission_deadline = Instant::now() + Duration::from_secs(10);
    loop {
        if submit_literal(
            &mut ledger,
            &mut gate,
            &terminal,
            &mut recipient,
            id,
            request,
            now_ms(),
        )
        .unwrap()
            == Submission::Enqueued
        {
            break;
        }
        assert_eq!(
            ledger.records()[0].state,
            DeliveryState::Pending,
            "deferral before input must preserve the same pending intent"
        );
        assert!(
            Instant::now() < admission_deadline,
            "no safe submission window; no turn sent"
        );
        tick(&mut terminal, &mut events);
    }
    assert_eq!(ledger.records()[0].state, DeliveryState::Enqueued);
    assert_eq!(
        submit_literal(
            &mut ledger,
            &mut gate,
            &terminal,
            &mut recipient,
            id,
            request,
            now_ms()
        )
        .unwrap(),
        Submission::Deferred,
        "buffered submission cannot immediately admit another reminder"
    );
    let started = Instant::now();
    let deadline = started + Duration::from_secs(90);
    let mut busy_observations = 0;
    let mut response_seen = false;
    loop {
        let screen = terminal
            .observation(32_768)
            .expect("bounded complete native screen");
        let text = screen.lines.join("\n");
        let busy = text.to_ascii_lowercase().contains("esc to interrupt")
            || text.to_ascii_lowercase().contains("esc to stop");
        let response = screen
            .lines
            .iter()
            .any(|line| line.trim() == "AMUX_SMOKE_ACK" || line.trim() == "• AMUX_SMOKE_ACK");
        response_seen |= response;
        let ready = gate.observe(&terminal, &recipient, Instant::now());
        if busy {
            busy_observations += 1;
            assert!(ready.is_none(), "busy terminal must not admit reminders");
        }
        if let Some(proof) = ready {
            assert!(
                response_seen,
                "profile admitted a reminder before the explicit response"
            );
            assert!(busy_observations > 0, "no actual busy interval observed");
            assert!(proof.is_current(&terminal, &recipient, Instant::now()));
            assert!(
                !matches!(
                    submit_literal(
                        &mut ledger,
                        &mut gate,
                        &terminal,
                        &mut recipient,
                        id,
                        request,
                        now_ms()
                    ),
                    Ok(Submission::Enqueued)
                ),
                "completed native turn cannot resubmit the same delivery"
            );
            let artifacts = project.join("artifacts");
            std::fs::create_dir_all(&artifacts).unwrap();
            let evidence = serde_json::json!({"provider":"Codex0.163.0","os":"linux",
                "one_real_model_turn_submitted":true,"response_seen":response_seen,
                "busy_observations":busy_observations,"turn_and_settle_ms":started.elapsed().as_millis(),
                "continuation_profile_ready":true,"checked_submission_enqueued":true,
                "duplicate_suppressed":true,"gateway_claim_or_execution_evidence":false});
            std::fs::write(
                artifacts.join("native-turn-evidence.json"),
                serde_json::to_vec_pretty(&evidence).unwrap(),
            )
            .unwrap();
            println!("{evidence}");
            break;
        }
        if Instant::now() >= deadline {
            std::fs::create_dir_all(project.join("artifacts")).unwrap();
            std::fs::write(project.join("artifacts/native-turn-last-screen.txt"), text).unwrap();
            panic!(
                "turn/continuation check timed out; inspect native-turn-last-screen.txt; busy={busy_observations}, response_seen={response_seen}"
            );
        }
        tick(&mut terminal, &mut events);
    }
}
