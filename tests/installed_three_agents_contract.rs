//! Opt-in actual providers: no Enter, model turns, gateway calls or claims.
#![cfg(all(feature = "desktop", target_os = "linux"))]
use amux::{
    identity::{self, Provider},
    recipient::TerminalRecipient,
    terminal_lifetime::TerminalLifetime,
};
use iced::futures::{StreamExt, stream::BoxStream};
use iced_term::{Command, Event, Terminal, settings};
use std::{
    collections::HashSet,
    path::Path,
    time::{Duration, Instant},
};

struct Pane {
    // Native owned processes retire before the terminal and event stream drop.
    _lifetime: TerminalLifetime,
    terminal: Terminal,
    events: BoxStream<'static, Event>,
    recipient: TerminalRecipient,
    provider: Provider,
}

impl Pane {
    fn open(project: &Path, state: &Path, provider: Provider, id: u64) -> Self {
        let terminal = Terminal::new(
            id,
            settings::Settings {
                backend: settings::BackendSettings {
                    program: env!("CARGO_BIN_EXE_amux").into(),
                    args: vec![
                        "--project-dir".into(),
                        project.display().to_string(),
                        "agent".into(),
                        provider.command().into(),
                    ],
                    working_directory: Some(project.into()),
                    env: [
                        ("AMUX_STATE_DIR".into(), state.display().to_string()),
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
        let launcher = terminal.child_pid().unwrap();
        let lifetime = TerminalLifetime::capture(launcher).unwrap();
        let events = iced::advanced::subscription::into_recipes(terminal.subscription())
            .pop()
            .unwrap()
            .stream(iced::futures::stream::empty().boxed());
        Self {
            _lifetime: lifetime,
            recipient: TerminalRecipient::new(launcher, provider, project).unwrap(),
            terminal,
            events,
            provider,
        }
    }

    fn poll(&mut self, runtime: &tokio::runtime::Runtime) {
        if let Ok(Some(event)) = runtime.block_on(async {
            tokio::time::timeout(Duration::from_millis(10), self.events.next()).await
        }) {
            let Event::BackendCall(id, command) = event;
            assert_eq!(id, self.terminal.id);
            assert_ne!(
                self.terminal.handle(Command::ProxyToBackend(command)),
                iced_term::actions::Action::Shutdown,
                "a provider exited during the three-session check"
            );
        }
    }

    fn contains(&self, text: &str) -> bool {
        self.terminal
            .observation(32_768)
            .is_some_and(|screen| screen.lines.iter().any(|line| line.contains(text)))
    }

    fn composer(&self) -> bool {
        match self.provider {
            Provider::Codex => {
                self.contains(">_ OpenAI Codex (v0.163.0)")
                    && self.contains("Ask Codex to do anything")
            }
            Provider::Claude => {
                self.contains("Claude Code v2.1.308")
                    && self.contains("❯\u{a0}Try \"")
                    && self.contains("for agents")
            }
        }
    }
}

#[test]
#[ignore = "requires authenticated native Codex0.163 and Claude2.1.308; input only, no Enter/model turns"]
fn three_installed_same_project_agents_keep_distinct_identities_input_and_lifetimes() {
    let project = Path::new(env!("CARGO_MANIFEST_DIR"));
    let fixtures = project.join("target/test-fixtures");
    std::fs::create_dir_all(&fixtures).unwrap();
    let state = tempfile::tempdir_in(fixtures).unwrap();
    let mut panes: Vec<_> = [Provider::Codex, Provider::Codex, Provider::Claude]
        .into_iter()
        .enumerate()
        .map(|(index, provider)| {
            Pane::open(project, state.path(), provider, 990_001 + index as u64)
        })
        .collect();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        let mut ready = true;
        for pane in &mut panes {
            pane.poll(&runtime);
            ready &= pane.recipient.resolve(state.path()).is_ok() && pane.composer();
        }
        if ready {
            break;
        }
        assert!(
            Instant::now() < deadline,
            "providers did not reach their composers"
        );
    }
    let records = identity::live_agents(state.path()).unwrap();
    assert_eq!(records.len(), 3);
    assert_eq!(
        records
            .iter()
            .filter(|r| r.identity.provider == Provider::Codex)
            .count(),
        2
    );
    for record in &records {
        assert_eq!(
            record.identity.project_directory,
            amux::project_identity::resolve(project).unwrap().directory
        );
    }
    assert_eq!(
        records
            .iter()
            .map(|r| r.identity.session_id)
            .collect::<HashSet<_>>()
            .len(),
        3
    );
    assert_eq!(
        records
            .iter()
            .map(|r| r.identity.instance_id)
            .collect::<HashSet<_>>()
            .len(),
        3
    );
    assert_eq!(
        records.iter().map(|r| r.pid).collect::<HashSet<_>>().len(),
        3
    );
    let addresses: Vec<_> = panes
        .iter()
        .map(|pane| pane.recipient.address(pane.terminal.id).unwrap())
        .collect();
    for (pane, address) in panes.iter().zip(&addresses) {
        assert!(pane.recipient.matches_address(address));
        for other in addresses.iter().filter(|other| *other != address) {
            assert!(!pane.recipient.matches_address(other));
        }
    }
    let marker = format!("amux-three-agent-input-{}", uuid::Uuid::new_v4().simple());
    panes[0].recipient.note_human_input();
    panes[0]
        .terminal
        .try_enqueue_input(marker.as_bytes().to_vec())
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        for pane in &mut panes {
            pane.poll(&runtime);
        }
        if panes[0].contains(&marker) {
            break;
        }
        assert!(Instant::now() < deadline, "targeted input was not observed");
    }
    assert!(panes[1..].iter().all(|pane| !pane.contains(&marker)));
    // No Enter: retire the pane with its unsubmitted input, keeping peers live.
    let closed = addresses[0].identity().clone();
    drop(panes.remove(0));
    let deadline = Instant::now() + Duration::from_secs(3);
    loop {
        let live = identity::live_agents(state.path()).unwrap();
        if !live.iter().any(|record| record.identity == closed) {
            assert_eq!(live.len(), 2);
            assert!(
                panes
                    .iter()
                    .all(|pane| live.iter().any(|record| &record.identity
                        == pane.recipient.address(pane.terminal.id).unwrap().identity()))
            );
            break;
        }
        assert!(
            Instant::now() < deadline,
            "closed provider retained its live registration"
        );
        std::thread::sleep(Duration::from_millis(20));
    }
    let evidence = serde_json::json!({
        "platform":"linux", "providers":["Codex0.163.0","Codex0.163.0","Claude2.1.308"],
        "same_project":true,"distinct_sessions":3,"distinct_instances":3,"distinct_agent_processes":3,
        "cross_recipient_rejected":true,"input_observed_only_at_target":true,"closed_target_preserved_two_peers":true,
        "enter_bytes_sent":0,"model_turns":0,"gateway_mutations":0,
        "gui_acceptance":false,"claim_delegation_acceptance":false
    });
    std::fs::create_dir_all(project.join("artifacts")).unwrap();
    std::fs::write(
        project.join("artifacts/three-installed-agents-evidence.json"),
        serde_json::to_vec_pretty(&evidence).unwrap(),
    )
    .unwrap();
    println!("{evidence}");
}
