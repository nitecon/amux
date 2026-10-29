//! Controlled native-terminal delivery tests. Fixture agents are copies of this
//! repo-built test binary renamed codex/claude under target/test-fixtures and run
//! inside real native terminals (Linux PTY, Windows ConPTY). No installed provider,
//! model turn or gateway connection is ever started: server events are handed to
//! the desktop directly, and the background scan and verification tasks are
//! driven manually with the same native discovery calls.
use super::super::tests::desktop;
use super::*;
use amux::event_client::Incoming;
use iced_term::TerminalGridObservation;
use serde_json::{Value, json};
use std::{io::Write, path::Path, sync::OnceLock};

use std::io::Read;

const LOG: &str = "AMUX_DESKTOP_FIXTURE_LOG";
const LAYOUT: &str = "AMUX_DESKTOP_FIXTURE_LAYOUT";
const ENTRY: &str = "desktop::desktop_delegation::tests::fixture_process";

/// A minimal provider-shaped composer. Every submitted line is appended verbatim
/// to the log, so tests can prove exactly which bytes reached the agent's input.
struct Fixture {
    marker: char,
    layout: String,
    log: PathBuf,
    draft: Vec<char>,
    cursor: usize,
    raw: String,
    submitted: usize,
    last_text: Option<Instant>,
    pending: String,
}

impl Fixture {
    fn draw(&self) {
        let busy = self.layout == "busy";
        let multiline = self.layout == "multiline";
        let draft: String = self.draft.iter().collect();
        let mut rows = vec![
            format!("fixture agent · {} submitted", self.submitted),
            if busy {
                "  Working (esc to interrupt)".into()
            } else {
                String::new()
            },
            if self.layout == "opaque" {
                format!("custom provider UI {draft}")
            } else {
                format!("{} {draft}", self.marker)
            },
        ];
        if multiline {
            rows.push("  second draft line".into());
        }
        if self.marker == '❯' {
            rows.push("─".repeat(40));
        } else if !multiline {
            rows.push(String::new());
        }
        if self.layout == "burst" && !self.draft.is_empty() {
            rows.push("  GPT-6.1-Sol medium · ~/project · weekly 95% left".into());
        } else if self.layout != "opaque" {
            rows.push("  ? for shortcuts".into());
        }
        let paste = if self.layout == "paste" {
            "\x1b[?2004h"
        } else {
            ""
        };
        let mut out = std::io::stdout().lock();
        let _ = write!(
            out,
            "{paste}\x1b[2J\x1b[H{}\x1b[3;{}H",
            rows.join("\r\n"),
            3 + self.cursor
        );
        let _ = out.flush();
    }

    fn submit(&mut self) {
        let mut log = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(&self.log)
            .unwrap();
        writeln!(log, "{}", self.raw).unwrap();
        self.raw.clear();
        self.draft.clear();
        self.cursor = 0;
        self.submitted += 1;
    }

    /// Raw input: printable text edits the draft, Home moves the caret, Ctrl+C/U
    /// clear and Enter submits everything received since the previous submission.
    #[cfg_attr(not(target_os = "linux"), allow(dead_code))]
    fn feed(&mut self, incoming: &str) {
        self.pending.push_str(incoming);
        let input = std::mem::take(&mut self.pending);
        let mut rest = input.as_str();
        while let Some(c) = rest.chars().next() {
            let mut len = c.len_utf8();
            match c {
                '\x03' if self.layout == "interrupt-exits" => std::process::exit(94),
                '\r' | '\n'
                    if self.layout == "burst"
                        && self
                            .last_text
                            .is_some_and(|at| at.elapsed() < Duration::from_millis(120)) =>
                {
                    self.draft.push('\n');
                    self.raw.push('\n');
                    self.cursor += 1;
                }
                '\r' | '\n' => self.submit(),
                '\x03' | '\x15' => {
                    self.raw.clear();
                    self.draft.clear();
                    self.cursor = 0;
                }
                '\x7f' => {
                    self.raw.push(c);
                    if self.cursor > 0 {
                        self.cursor -= 1;
                        self.draft.remove(self.cursor);
                    }
                }
                '\x1b'
                    if rest.len() == 1
                        || rest.starts_with("\x1b[")
                        || rest.starts_with("\x1bO") =>
                {
                    let Some(end) = rest
                        .bytes()
                        .enumerate()
                        .skip(2)
                        .find(|(_, b)| (0x40..=0x7e).contains(b))
                        .map(|(i, _)| i + 1)
                    else {
                        self.pending = rest.to_owned();
                        break;
                    };
                    len = end;
                    let sequence = &rest[..len];
                    self.last_text = None;
                    if matches!(sequence, "\x1b[H" | "\x1bOH") {
                        self.cursor = 0;
                    }
                    if sequence == "\x1b[F" {
                        self.cursor = self.draft.len();
                    } else {
                        self.raw.push_str(sequence);
                    }
                }
                c if !c.is_control() => {
                    self.last_text = Some(Instant::now());
                    self.raw.push(c);
                    self.draft.insert(self.cursor, c);
                    self.cursor += 1;
                }
                c => self.raw.push(c),
            }
            rest = &rest[len..];
        }
        self.draw();
    }
}

#[test]
#[ignore = "fixture agent entry point, run only inside native terminals by these tests"]
fn fixture_process() {
    let Some(log) = std::env::var_os(LOG) else {
        return;
    };
    #[cfg(windows)]
    {
        use windows_sys::Win32::System::Console::{
            ENABLE_VIRTUAL_TERMINAL_PROCESSING, GetConsoleMode, GetStdHandle, STD_OUTPUT_HANDLE,
            SetConsoleMode,
        };
        // SAFETY: this fixture owns its inherited ConPTY console output handle.
        // Native providers enable VT processing too; otherwise ANSI is literal text.
        unsafe {
            let handle = GetStdHandle(STD_OUTPUT_HANDLE);
            let mut mode = 0;
            assert_ne!(GetConsoleMode(handle, &mut mode), 0);
            assert_ne!(
                SetConsoleMode(handle, mode | ENABLE_VIRTUAL_TERMINAL_PROCESSING),
                0
            );
        }
    }
    #[cfg(windows)]
    // SAFETY: this fixture exclusively owns the input side of its private ConPTY.
    unsafe {
        use windows_sys::Win32::System::Console::*;
        let handle = GetStdHandle(STD_INPUT_HANDLE);
        let mut mode = 0;
        assert_ne!(GetConsoleMode(handle, &mut mode), 0);
        mode = (mode & !(ENABLE_LINE_INPUT | ENABLE_ECHO_INPUT | ENABLE_PROCESSED_INPUT))
            | ENABLE_VIRTUAL_TERMINAL_INPUT;
        assert_ne!(SetConsoleMode(handle, mode), 0);
    }
    // Bound a leaked fixture even if its terminal never closes.
    std::thread::spawn(|| {
        std::thread::sleep(Duration::from_secs(120));
        std::process::exit(0);
    });
    let exe = std::env::current_exe().unwrap();
    let mut fixture = Fixture {
        marker: if exe.file_stem().unwrap() == "codex" {
            '›'
        } else {
            '❯'
        },
        layout: std::env::var(LAYOUT).unwrap_or_default(),
        log: log.into(),
        draft: Vec::new(),
        cursor: 0,
        raw: String::new(),
        submitted: 0,
        last_text: None,
        pending: String::new(),
    };
    #[cfg(target_os = "linux")]
    {
        // SAFETY: initialized termios for this process's own terminal stdin.
        unsafe {
            let mut mode: libc::termios = std::mem::zeroed();
            if libc::tcgetattr(0, &mut mode) == 0 {
                libc::cfmakeraw(&mut mode);
                libc::tcsetattr(0, libc::TCSANOW, &mode);
            }
        }
    }
    fixture.draw();
    let mut buffer = [0u8; 4096];
    let mut stdin = std::io::stdin().lock();
    while let Ok(n @ 1..) = stdin.read(&mut buffer) {
        fixture.feed(&String::from_utf8_lossy(&buffer[..n]));
    }
}

fn executable(name: &str) -> String {
    format!("{name}{}", std::env::consts::EXE_SUFFIX)
}

fn fixtures() -> &'static Path {
    static DIR: OnceLock<PathBuf> = OnceLock::new();
    DIR.get_or_init(|| {
        let parent = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&parent).unwrap();
        for stale in std::fs::read_dir(&parent).unwrap().flatten() {
            if stale
                .file_name()
                .to_string_lossy()
                .starts_with("desktop-delegation-")
            {
                let _ = std::fs::remove_dir_all(stale.path());
            }
        }
        let dir = parent.join(format!("desktop-delegation-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let exe = std::env::current_exe().unwrap();
        for name in ["claude", "codex"] {
            std::fs::copy(&exe, dir.join(executable(name))).unwrap();
        }
        std::fs::canonicalize(dir).unwrap()
    })
}

/// One native terminal per fixture agent, in its own scratch working directory.
pub(super) struct Agent {
    pub(super) terminal: u64,
    pub(super) directory: PathBuf,
    log: PathBuf,
}

impl Agent {
    pub(super) fn lines(&self) -> Vec<String> {
        std::fs::read_to_string(&self.log)
            .map(|log| log.lines().map(str::to_owned).collect())
            .unwrap_or_default()
    }
}

pub(super) fn spawn(
    app: &mut Desktop,
    workspace: usize,
    id: u64,
    provider: Provider,
    layout: &str,
) -> Agent {
    let name = match provider {
        Provider::Codex => "codex",
        Provider::Claude => "claude",
    };
    let directory = amux::workspace_launch::external_path(
        fixtures().join(format!("agent-{}-{id}", Uuid::new_v4())),
    );
    std::fs::create_dir(&directory).unwrap();
    let log = directory.join("input.log");
    let mut backend = iced_term::settings::BackendSettings {
        program: fixtures().join(executable(name)).display().to_string(),
        args: [
            "--exact",
            ENTRY,
            "--ignored",
            "--nocapture",
            "--test-threads=1",
            "-q",
        ]
        .map(String::from)
        .to_vec(),
        working_directory: Some(directory.clone()),
        ..Default::default()
    };
    backend.env.insert(LOG.into(), log.display().to_string());
    backend.env.insert(LAYOUT.into(), layout.into());
    let terminal = Terminal::new(
        id,
        iced_term::settings::Settings {
            backend,
            ..Default::default()
        },
    )
    .unwrap();
    let focused = app.workspaces[workspace].focused;
    let pane = app.workspaces[workspace].panes.get_mut(focused).unwrap();
    if !pane.tabs.iter().any(|t| t.id == id) {
        pane.tabs.push(Tab {
            id,
            title: name.into(),
            #[cfg(any(target_os = "linux", windows))]
            lifetime: None,
            terminal: None,
            stopped_output: None,
            provider: Some(provider),
            delegation: TabState::default(),
        });
    }
    let tab = pane.tabs.iter_mut().find(|t| t.id == id).unwrap();
    tab.provider = Some(provider);
    tab.delegation = TabState::capture(&terminal, id + 1, &directory);
    tab.terminal = Some(terminal);
    let agent = Agent {
        terminal: id,
        directory: amux::workspace_launch::external_path(
            std::fs::canonicalize(&directory).unwrap(),
        ),
        log,
    };
    wait(app, |app| {
        grid(app, id).is_some_and(|g| g.observation.lines[0].starts_with("fixture agent"))
    });
    agent
}

pub(super) fn find(app: &Desktop, id: u64) -> &Tab {
    app.workspaces
        .iter()
        .flat_map(|w| w.panes.iter().flat_map(|(_, p)| &p.tabs))
        .find(|t| t.id == id)
        .unwrap()
}

fn grid(app: &Desktop, id: u64) -> Option<TerminalGridObservation> {
    find(app, id).terminal.as_ref()?.grid_observation(32_768)
}

fn wait(app: &Desktop, done: impl Fn(&Desktop) -> bool) {
    let deadline = Instant::now() + Duration::from_secs(10);
    while !done(app) {
        assert!(
            Instant::now() < deadline,
            "native terminal condition timed out"
        );
        std::thread::sleep(Duration::from_millis(10));
    }
}

/// Human keyboard input through the desktop's own terminal message path.
pub(super) fn human(app: &mut Desktop, id: u64, bytes: &[u8]) {
    drop(app.update(Message::Terminal(iced_term::Event::BackendCall(
        id,
        BackendCommand::Write(bytes.to_vec()),
    ))));
}

pub(super) fn project(app: &mut Desktop, agent: &Agent, project: &str) {
    app.delegation.projects.insert(
        agent.directory.clone(),
        (Instant::now(), Some(project.into())),
    );
}

/// The background scan, run synchronously with the same native discovery.
pub(super) fn scan(app: &mut Desktop) {
    let found = app
        .delegation_roots()
        .into_iter()
        .filter_map(|root| discover_root(root, app.registry.as_deref()?))
        .collect();
    app.delegation.scan_serial += 1;
    let serial = app.delegation.scan_serial;
    let epoch = app.delegation.epoch;
    drop(app.delegation_message(epoch, Event::Scanned { serial, found }));
}

pub(super) fn wait_lines(agent: &Agent, count: usize) -> Vec<String> {
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        let lines = agent.lines();
        if lines.len() >= count {
            return lines;
        }
        assert!(Instant::now() < deadline, "fixture never received input");
        std::thread::sleep(Duration::from_millis(10));
    }
}

/// A connected event router whose outgoing frames the test reads.
fn router(app: &mut Desktop) -> mpsc::Receiver<Value> {
    app.preferences.current.automatic_notifications = true;
    let (frames, received) = mpsc::channel(256);
    app.delegation.events.outgoing = Some(frames);
    app.delegation.events.connected = true;
    received
}

fn session(app: &Desktop, agent: &Agent) -> String {
    app.delegation
        .candidates
        .iter()
        .find(|c| c.target.terminal == agent.terminal)
        .expect("scanned agent")
        .target
        .session
        .to_string()
}

/// An `agent.notify` for `agent` about a queued task, as the server sends it.
/// `require_idle` makes it a work offer (`assigned`).
fn notify(app: &Desktop, agent: &Agent, seq: i64, require_idle: bool) -> Value {
    let mode = if require_idle {
        "assigned"
    } else {
        "recipient"
    };
    json!({
        "seq": seq, "id": "agent.notify", "version": 1, "uid": Uuid::new_v4().to_string(),
        "source": "plugin:notifier", "emitted_at": unix_ms() as i64, "correlation": null,
        "payload": {
            "target": session(app, agent), "project": "alpha", "mode": mode,
            "require_idle": require_idle, "heading": null, "instructions": null,
            "event": {"kind": "task_created", "project_ident": "alpha", "created_at": unix_ms() as i64,
                      "task": {"id": Uuid::new_v4(), "kind": "normal", "title": format!("Task {seq}"),
                               "status": "todo", "owner_origin": null, "owner_agent_id": null},
                      "comment": null, "origin": null},
        },
    })
}

fn receive(app: &mut Desktop, event: Value) {
    let epoch = app.delegation.epoch;
    drop(app.delegation_message(epoch, Event::Router(Incoming::Event(event))));
}

#[cfg(any(target_os = "linux", windows))]
fn text(app: &Desktop, agent: &Agent, event: &Value) -> String {
    let candidate = app
        .delegation
        .candidates
        .iter()
        .find(|c| c.target.terminal == agent.terminal)
        .unwrap();
    events::notification_text(event, candidate).unwrap()
}

fn verification(app: &Desktop, verified: bool) -> Event {
    let delivery = app.delegation.events.delivering.as_ref().unwrap();
    Event::Verified {
        token: delivery.token,
        verified,
    }
}

/// Start pending deliveries and complete each through native verification.
fn deliver(app: &mut Desktop) {
    let deadline = Instant::now() + Duration::from_secs(10);
    while !app.delegation.events.pending.is_empty() || app.delegation.events.delivering.is_some() {
        assert!(Instant::now() < deadline, "delivery did not complete");
        drop(app.events_deliver());
        let verified = app
            .delegation
            .events
            .delivering
            .as_ref()
            .map(|delivery| events::verify(&delivery.candidate));
        if let Some(verified) = verified {
            let epoch = app.delegation.epoch;
            drop(app.delegation_message(epoch, verification(app, verified)));
        }
    }
}

#[cfg(any(target_os = "linux", windows))]
/// Outcomes reported to the server, in order.
fn results(frames: &mut mpsc::Receiver<Value>) -> Vec<String> {
    let mut results = Vec::new();
    while let Ok(frame) = frames.try_recv() {
        if frame["id"] == "agent.notify_result" {
            results.push(frame["payload"]["status"].as_str().unwrap().to_owned());
        }
    }
    results
}

#[test]
fn typed_text_is_teed_and_navigation_alone_does_not_count() {
    let mut state = TabState::default();
    for bytes in [b"\x1b[A".as_slice(), b"\x1b[H", b"\x1bOH", b"\x1b[1;2D"] {
        state.note_command(&BackendCommand::Write(bytes.to_vec()));
        assert!(state.input.is_empty());
    }
    state.note_command(&BackendCommand::Write(b"hello ".to_vec()));
    state.note_command(&BackendCommand::Write(b"\x1b[D".to_vec()));
    state.note_command(&BackendCommand::Write(b"x".to_vec()));
    assert_eq!(state.input, b"hello \x1b[Dx");
    state.note_command(&BackendCommand::Write(b"\r".to_vec()));
    assert!(state.input.is_empty());
    state.note_command(&BackendCommand::Write(b"\x1b[200~paste\x1b[201~".to_vec()));
    assert!(!state.input.is_empty());
}

#[test]
fn non_text_end_key_flushes_a_paste_burst_before_enter_without_waiting() {
    let mut fixture = Fixture {
        marker: '›',
        layout: "burst".into(),
        log: fixtures().join("burst-unit.log"),
        draft: Vec::new(),
        cursor: 0,
        raw: String::new(),
        submitted: 0,
        last_text: None,
        pending: String::new(),
    };
    fixture.feed("gateway text\r");
    assert_eq!(fixture.submitted, 0);
    assert_eq!(fixture.draft.last(), Some(&'\n'));
    fixture.feed("\x1b[F\r");
    assert_eq!(fixture.submitted, 1);
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn native_delivery_submits_the_exact_notification_once_and_reports_it() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Claude, "ready");
    app.registry = Some(agent.directory.clone());
    let mut frames = router(&mut app);
    project(&mut app, &agent, "alpha");
    wait(&app, |app| {
        grid(app, 0).is_some_and(|g| g.observation.lines[0].starts_with("fixture agent"))
    });
    scan(&mut app);
    let event = notify(&app, &agent, 1, false);
    receive(&mut app, event.clone());
    deliver(&mut app);
    assert_eq!(wait_lines(&agent, 1), [text(&app, &agent, &event)]);
    assert_eq!(results(&mut frames), ["delivered"]);
    // A resend of the same event is acknowledged without a second paste.
    receive(&mut app, event.clone());
    deliver(&mut app);
    std::thread::sleep(Duration::from_millis(200));
    assert_eq!(agent.lines().len(), 1);
    assert!(results(&mut frames).is_empty());
}

#[cfg(target_os = "linux")]
#[test]
fn native_bracketed_paste_prompt_receives_the_notification_as_one_paste() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "paste");
    app.registry = Some(agent.directory.clone());
    let _frames = router(&mut app);
    project(&mut app, &agent, "alpha");
    wait(&app, |app| {
        grid(app, 0).is_some_and(|g| {
            g.observation
                .mode
                .contains(iced_term::TermMode::BRACKETED_PASTE)
        })
    });
    scan(&mut app);
    let event = notify(&app, &agent, 1, false);
    receive(&mut app, event.clone());
    deliver(&mut app);
    assert_eq!(
        wait_lines(&agent, 1),
        [format!("\x1b[200~{}\x1b[201~", text(&app, &agent, &event))]
    );
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn native_delivery_submits_immediately_and_restores_teed_user_input() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "burst");
    app.registry = Some(agent.directory.clone());
    let _frames = router(&mut app);
    project(&mut app, &agent, "alpha");
    scan(&mut app);
    human(&mut app, 0, b"my draft ");
    let event = notify(&app, &agent, 1, false);
    receive(&mut app, event.clone());
    drop(app.events_deliver());
    // Edits arriving during process verification are part of the restored buffer.
    human(&mut app, 0, b"more");
    let verified = events::verify(&app.delegation.events.delivering.as_ref().unwrap().candidate);
    let epoch = app.delegation.epoch;
    drop(app.delegation_message(epoch, verification(&app, verified)));
    assert_eq!(wait_lines(&agent, 1), [text(&app, &agent, &event)]);
    wait(&app, |app| {
        grid(app, 0).is_some_and(|g| {
            g.observation
                .lines
                .iter()
                .any(|line| line.contains("my draft more"))
        })
    });
    assert_eq!(find(&app, 0).delegation.input, b"my draft more");
    human(&mut app, 0, b"\x1b[F");
    human(&mut app, 0, b"\r");
    assert_eq!(
        wait_lines(&agent, 2),
        [text(&app, &agent, &event), "my draft more".into()]
    );
}

#[test]
fn repeated_notifications_never_interrupt_the_provider_to_clear_a_draft() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "interrupt-exits");
    app.registry = Some(agent.directory.clone());
    let _frames = router(&mut app);
    project(&mut app, &agent, "alpha");
    scan(&mut app);
    human(&mut app, 0, b"keep this draft");
    for seq in 1..=2 {
        let event = notify(&app, &agent, seq, false);
        receive(&mut app, event);
        deliver(&mut app);
        assert_eq!(wait_lines(&agent, seq as usize).len(), seq as usize);
    }
    human(&mut app, 0, b"\x1b[F\r");
    let lines = wait_lines(&agent, 3);
    assert_eq!(lines[2], "keep this draft");
    assert!(!lines.iter().any(|line| line.contains('\x03')));
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn work_offers_skip_a_busy_or_drafting_agent_and_report_it() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
    app.registry = Some(agent.directory.clone());
    let mut frames = router(&mut app);
    project(&mut app, &agent, "alpha");
    scan(&mut app);
    // The fixture just drew its screen, so its terminal output is recent.
    let event = notify(&app, &agent, 1, true);
    receive(&mut app, event);
    deliver(&mut app);
    assert_eq!(results(&mut frames), ["skipped"]);
    std::thread::sleep(amux::session::IDLE_AFTER + Duration::from_millis(100));
    human(&mut app, 0, b"half typed");
    let event = notify(&app, &agent, 2, true);
    receive(&mut app, event);
    deliver(&mut app);
    assert_eq!(results(&mut frames), ["skipped"], "a draft keeps it busy");
    human(&mut app, 0, b"\x15");
    let event = notify(&app, &agent, 3, true);
    receive(&mut app, event.clone());
    deliver(&mut app);
    assert_eq!(results(&mut frames), ["delivered"]);
    assert_eq!(wait_lines(&agent, 1), [text(&app, &agent, &event)]);
}

#[cfg(target_os = "linux")]
#[test]
fn buffered_human_input_blocks_work_offers_before_the_provider_echoes_it() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "ready");
    app.registry = Some(agent.directory.clone());
    let mut frames = router(&mut app);
    project(&mut app, &agent, "alpha");
    scan(&mut app);
    std::thread::sleep(amux::session::IDLE_AFTER + Duration::from_millis(100));
    let pid = app
        .delegation
        .candidates
        .iter()
        .find(|candidate| candidate.target.terminal == 0)
        .unwrap()
        .target
        .pid;
    struct Resume(u32);
    impl Drop for Resume {
        fn drop(&mut self) {
            // SAFETY: resume the fixture process stopped by this test, even on failure.
            unsafe {
                libc::kill(self.0 as libc::pid_t, libc::SIGCONT);
            }
        }
    }
    let before = grid(&app, 0).unwrap().observation;
    // SAFETY: the exact native fixture was discovered in this owned terminal.
    assert_eq!(unsafe { libc::kill(pid as libc::pid_t, libc::SIGSTOP) }, 0);
    let resume = Resume(pid);
    std::thread::sleep(Duration::from_millis(20));
    human(&mut app, 0, b"buffered draft");
    let unchanged = grid(&app, 0).unwrap().observation.lines == before.lines;
    let event = notify(&app, &agent, 1, true);
    receive(&mut app, event);
    deliver(&mut app);
    let outcomes = results(&mut frames);
    let draft = find(&app, 0).delegation.input.clone();
    drop(resume);
    assert!(
        unchanged,
        "a stopped provider still shows its old empty screen"
    );
    assert_eq!(outcomes, ["skipped"]);
    assert_eq!(draft, b"buffered draft");
    assert!(
        agent.lines().is_empty(),
        "the work offer never reached the provider"
    );
    human(&mut app, 0, b"\x15");
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn compatibility_snapshot_survives_same_connection_close_and_restart_pause() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
    app.registry = Some(agent.directory.clone());
    let _frames = router(&mut app);
    app.delegation.events.server_restart_reminders = Some(false);
    project(&mut app, &agent, "alpha");
    scan(&mut app);
    app.workspaces[0].gateway.tasks = vec![
        json!({"id":Uuid::new_v4(),"status":"in_progress","owner_origin":app.delegation.candidates[0].target.origin}),
    ];
    app.save_restart_tasks();
    let path = agent.directory.join("restart-tasks.json");
    let saved = std::fs::read(&path).unwrap();
    // Shutdown of a local listener keeps the source/owners for the callback's
    // second save. Closing the window must preserve that same snapshot.
    app.events_stop();
    app.embedded.stopping = true;
    drop(app.update(super::super::Message::ServerStopped(
        super::super::ServerExit::Close,
        Ok(()),
    )));
    assert_eq!(std::fs::read(&path).unwrap(), saved);
    app.pause_for_restart();
    assert!(app.restarting);
    assert_eq!(
        std::fs::read(&path).unwrap(),
        saved,
        "restart pause cannot delete its just-saved journal"
    );
    // Changing actual server/identity still clears compatibility state.
    app.reset_delegation();
    assert!(!path.exists());
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn async_verification_rechecks_the_current_logical_instance_and_project() {
    for changed in ["instance", "project"] {
        let mut app = desktop();
        let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
        app.registry = Some(agent.directory.clone());
        let mut frames = router(&mut app);
        project(&mut app, &agent, "alpha");
        scan(&mut app);
        let mut notice = notify(&app, &agent, 1, false);
        notice["payload"]["event"]["task"]["owner_origin"] =
            json!(app.delegation.candidates[0].target.origin);
        receive(&mut app, notice);
        drop(app.events_deliver());
        assert!(app.delegation.events.delivering.is_some());
        let verified = verification(&app, true);
        // The same native process/root can outlive a changed project binding
        // or logical identity. Its earlier native verification is still true.
        let candidate = &mut app.delegation.candidates[0];
        if changed == "project" {
            candidate.target.project = "rebound".into();
        } else {
            candidate.actor.instance_id = Uuid::new_v4();
            candidate.target.origin.as_mut().unwrap().instance_id = candidate.actor.instance_id;
        }
        drop(app.delegation_message(app.delegation.epoch, verified));
        assert_eq!(results(&mut frames), ["skipped"], "{changed}");
        assert!(agent.lines().is_empty(), "{changed}");
    }
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn queued_notices_cannot_rebind_to_another_execution_project_or_requester_generation() {
    for changed in ["instance", "project", "requester_generation"] {
        let mut app = desktop();
        let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
        app.registry = Some(agent.directory.clone());
        let frames = router(&mut app);
        project(&mut app, &agent, "alpha");
        scan(&mut app);
        let candidate = app.delegation.candidates[0].clone();
        let mut event = notify(&app, &agent, 1, false);
        if changed == "requester_generation" {
            event["payload"]["event"]["kind"] = json!("task_completed");
            event["payload"]["event"]["task"]["status"] = json!("done");
            event["payload"]["event"]["task"]["requester"] = json!({"origin":candidate.target.origin,"generation":candidate.target.executor_generation()});
        } else {
            event["payload"]["mode"] = json!("assigned");
            event["payload"]["event"]["task"]["owner_origin"] = json!(candidate.target.origin);
        }
        receive(&mut app, event);
        app.events_stop();
        drop(frames);
        let mut frames = router(&mut app);
        let candidate = &mut app.delegation.candidates[0];
        match changed {
            "instance" => {
                let replacement = Uuid::new_v4();
                candidate.actor.instance_id = replacement;
                candidate.target.origin.as_mut().unwrap().instance_id = replacement;
            }
            "project" => candidate.target.project = "other-project".into(),
            _ => candidate.target.generation.push_str("-replacement"),
        }
        drop(app.events_deliver());
        assert_eq!(results(&mut frames), ["skipped"], "{changed}");
        assert!(agent.lines().is_empty(), "{changed}");
    }
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn explicit_continuity_receiver_keeps_previous_owner_facts_without_rebinding_assignment() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
    app.registry = Some(agent.directory.clone());
    let mut frames = router(&mut app);
    project(&mut app, &agent, "alpha");
    scan(&mut app);
    let current = app.delegation.candidates[0].actor.instance_id;
    let previous = Uuid::new_v4();
    let mut event = notify(&app, &agent, 1, false);
    event["payload"]["event"]["task"]["owner_origin"] = json!({"session_id":session(&app,&agent),"instance_id":previous,"provider":"codex","os":"linux"});
    event["payload"]["receiver"] =
        json!({"session_id":session(&app,&agent),"instance_id":current,"project":"alpha"});
    event["payload"]["instructions"] =
        json!("Informational continuity only, not an ownership handoff.");
    receive(&mut app, event.clone());
    deliver(&mut app);
    assert_eq!(
        event["payload"]["event"]["task"]["owner_origin"]["instance_id"],
        previous.to_string()
    );
    assert!(wait_lines(&agent, 1)[0].contains("not an ownership handoff"));
    assert_eq!(results(&mut frames), ["delivered"]);
    // A resumed native attachment may change generation while retaining the
    // same logical owner. Ordinary owner notices must still reach it.
    let mut resumed = notify(&app, &agent, 2, false);
    resumed["payload"]["event"]["task"]["owner_origin"] =
        json!(app.delegation.candidates[0].target.origin);
    receive(&mut app, resumed);
    app.delegation.candidates[0]
        .target
        .generation
        .push_str("-resumed-attachment");
    deliver(&mut app);
    assert_eq!(results(&mut frames), ["delivered"]);
    assert_eq!(wait_lines(&agent, 2).len(), 2);
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn older_router_fallback_waits_for_negotiation_and_current_servers_never_run_it() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
    app.registry = Some(agent.directory.clone());
    project(&mut app, &agent, "alpha");
    scan(&mut app);
    let task = json!({"id":Uuid::new_v4(),"kind":"normal","title":"Saved work","status":"in_progress","project_ident":"alpha",
        "owner_origin":{"session_id":session(&app,&agent),"instance_id":Uuid::new_v4(),"provider":"codex","os":"linux"}});
    app.workspaces[0].gateway.tasks = vec![task.clone()];
    app.workspaces[0].gateway.loaded_at = Some(Instant::now());
    app.save_restart_tasks();
    app.load_restart_tasks();
    let journal = agent.directory.join("restart-tasks.json");
    assert!(
        journal.exists(),
        "loading waits for a protocol decision before retiring the journal"
    );
    app.restart_reminders();
    assert!(app.delegation.events.pending.is_empty());
    drop(app.events_incoming(Incoming::Unsupported));
    app.restart_reminders();
    assert!(
        app.delegation.events.pending.is_empty(),
        "404 has no automatic fallback"
    );
    let _frames = router(&mut app);
    drop(app.events_incoming(Incoming::Connected {
        clock: None,
        server_restart_reminders: false,
    }));
    app.restart_reminders();
    assert!(!journal.exists());
    assert_eq!(
        app.delegation.events.pending.len(),
        1,
        "successful older-router subscription enables HEAD continuity"
    );
    assert_eq!(app.delegation.events.pending[0]["local"], true);
    assert!(
        app.delegation.events.pending[0]["payload"]["instructions"]
            .as_str()
            .unwrap()
            .contains("not an ownership handoff")
    );
    app.events_stop();
    assert_eq!(app.delegation.events.pending.len(), 1);
    let mut frames = router(&mut app);
    std::thread::sleep(amux::session::IDLE_AFTER + Duration::from_millis(100));
    deliver(&mut app);
    assert!(wait_lines(&agent, 1)[0].contains("not an ownership handoff"));
    assert!(
        results(&mut frames).is_empty(),
        "older-router local continuity is never reported as a work offer"
    );
    app.delegation.events.restart = vec![task.clone()];
    app.restart_reminders();
    assert_eq!(app.delegation.events.pending.len(), 1);
    drop(app.events_incoming(Incoming::Connected {
        clock: None,
        server_restart_reminders: true,
    }));
    app.delegation.events.restart = vec![task];
    app.restart_reminders();
    assert!(
        app.delegation.events.pending.is_empty(),
        "supporting server owns scheduling exclusively"
    );
    app.save_restart_tasks();
    assert!(
        !journal.exists(),
        "supporting server does not need a client journal"
    );
    std::fs::write(&journal, b"old compatibility snapshot").unwrap();
    app.reset_delegation();
    assert!(
        !journal.exists(),
        "server/identity reset retires old compatibility work"
    );
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn redelivered_notifications_are_deduplicated_during_process_verification() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
    app.registry = Some(agent.directory.clone());
    let mut frames = router(&mut app);
    project(&mut app, &agent, "alpha");
    scan(&mut app);
    let event = notify(&app, &agent, 1, false);
    receive(&mut app, event.clone());
    drop(app.events_deliver());
    assert!(app.delegation.events.delivering.is_some());
    receive(&mut app, event.clone());
    assert!(app.delegation.events.pending.is_empty());
    deliver(&mut app);
    receive(&mut app, event.clone());
    assert!(app.delegation.events.pending.is_empty());
    assert_eq!(wait_lines(&agent, 1), [text(&app, &agent, &event)]);
    assert_eq!(results(&mut frames), ["delivered"]);
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn verification_from_a_previous_subscription_cannot_settle_replay_of_the_same_event() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
    app.registry = Some(agent.directory.clone());
    let frames = router(&mut app);
    project(&mut app, &agent, "alpha");
    scan(&mut app);
    let current = notify(&app, &agent, 1, false);
    receive(&mut app, current.clone());
    drop(app.events_deliver());
    let stale = verification(&app, false);

    // Resubscribing resets delivery state without replacing the bridge epoch.
    app.events_stop();
    drop(frames);
    let mut frames = router(&mut app);
    // The unacknowledged event can be replayed with exactly the same UID.
    receive(&mut app, current.clone());
    drop(app.events_deliver());
    drop(app.delegation_message(app.delegation.epoch, stale));
    assert_eq!(
        app.delegation.events.delivering.as_ref().unwrap().event["uid"],
        current["uid"]
    );
    assert!(results(&mut frames).is_empty());
    deliver(&mut app);
    assert_eq!(results(&mut frames), ["delivered"]);
    assert_eq!(wait_lines(&agent, 1), [text(&app, &agent, &current)]);
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn notification_conditions_are_rechecked_after_process_verification() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
    app.registry = Some(agent.directory.clone());
    let mut frames = router(&mut app);
    project(&mut app, &agent, "alpha");
    scan(&mut app);
    std::thread::sleep(amux::session::IDLE_AFTER + Duration::from_millis(100));

    // The agent was idle when verification started. A user starts a draft
    // before it returns, so the work offer must go back to the server.
    let offer = notify(&app, &agent, 1, true);
    receive(&mut app, offer);
    drop(app.events_deliver());
    assert!(app.delegation.events.delivering.is_some());
    human(&mut app, 0, b"new draft");
    drop(app.delegation_message(app.delegation.epoch, verification(&app, true)));
    assert_eq!(results(&mut frames), ["skipped"]);
    assert!(agent.lines().is_empty());
    human(&mut app, 0, b"\x15");

    // An informational notice instead returns to the local queue when the
    // agent becomes busy during verification.
    let mut event = notify(&app, &agent, 2, false);
    event["payload"]["require_idle"] = json!(true);
    receive(&mut app, event);
    // Clearing the previous draft wrote output; let that settle first.
    std::thread::sleep(amux::session::IDLE_AFTER + Duration::from_millis(100));
    drop(app.events_deliver());
    assert!(app.delegation.events.delivering.is_some());
    human(&mut app, 0, b"another draft");
    drop(app.delegation_message(app.delegation.epoch, verification(&app, true)));
    assert_eq!(app.delegation.events.pending.len(), 1);
    assert!(results(&mut frames).is_empty());
    assert!(agent.lines().is_empty());
    app.delegation.events.pending.clear();

    // Verification can also outlive a notification's deadline, even when it
    // does not require idle. Model the elapsed time on the in-flight event.
    let expired = notify(&app, &agent, 3, false);
    receive(&mut app, expired);
    drop(app.events_deliver());
    app.delegation.events.delivering.as_mut().unwrap().event["emitted_at"] = json!(0);
    drop(app.delegation_message(app.delegation.epoch, verification(&app, true)));
    assert_eq!(results(&mut frames), ["skipped"]);
    assert!(agent.lines().is_empty());
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn unverified_processes_and_other_agents_never_receive_input() {
    let mut app = desktop();
    let mine = spawn(&mut app, 0, 0, Provider::Claude, "opaque");
    let peer = spawn(&mut app, 1, 1, Provider::Codex, "opaque");
    app.registry = Some(mine.directory.clone());
    let mut frames = router(&mut app);
    project(&mut app, &mine, "alpha");
    project(&mut app, &peer, "alpha");
    scan(&mut app);
    assert_ne!(session(&app, &mine), session(&app, &peer));
    // A process that no longer verifies is reported failed, without input.
    let event = notify(&app, &mine, 1, false);
    receive(&mut app, event);
    drop(app.events_deliver());
    let epoch = app.delegation.epoch;
    drop(app.delegation_message(epoch, verification(&app, false)));
    assert_eq!(results(&mut frames), ["failed"]);
    // A notification only reaches the agent it names.
    let event = notify(&app, &mine, 2, false);
    receive(&mut app, event.clone());
    deliver(&mut app);
    assert_eq!(wait_lines(&mine, 1), [text(&app, &mine, &event)]);
    std::thread::sleep(Duration::from_millis(200));
    assert!(peer.lines().is_empty());
    // An agent this desktop does not run is skipped.
    let mut elsewhere = notify(&app, &mine, 3, false);
    elsewhere["payload"]["target"] = json!(Uuid::new_v4().to_string());
    receive(&mut app, elsewhere);
    deliver(&mut app);
    assert_eq!(results(&mut frames), ["delivered", "skipped"]);
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn server_activity_drives_the_agent_indicator_and_task_events_refresh_panels() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
    app.registry = Some(agent.directory.clone());
    let _frames = router(&mut app);
    app.preferences.current.mode = amux::gateway_preferences::GatewayMode::Client;
    project(&mut app, &agent, "alpha");
    scan(&mut app);
    let me = session(&app, &agent);
    let state = |id: &str| {
        json!({"seq": 1, "id": id, "uid": Uuid::new_v4().to_string(),
               "payload": {"agent": {"session_id": me, "project": "alpha"}}})
    };
    let busy = state("agent.busy");
    receive(&mut app, busy);
    assert_eq!(find(&app, 0).delegation.server_busy, Some(true));
    let idle = state("agent.idle");
    receive(&mut app, idle);
    assert_eq!(find(&app, 0).delegation.server_busy, Some(false));
    app.workspaces[0].gateway.project = "alpha".into();
    app.workspaces[0].gateway.loading = true;
    app.workspaces[0].gateway.draft_title = "Preserve this task draft".into();
    receive(
        &mut app,
        json!({"seq": 2, "id": "task.created", "uid": Uuid::new_v4().to_string(), "payload": {"project": "alpha"}}),
    );
    assert!(app.workspaces[0].gateway.refresh_pending);
    assert_eq!(
        app.workspaces[0].gateway.draft_title,
        "Preserve this task draft"
    );
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn informational_notices_wait_for_an_idle_agent_instead_of_being_skipped() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
    app.registry = Some(agent.directory.clone());
    let mut frames = router(&mut app);
    project(&mut app, &agent, "alpha");
    scan(&mut app);
    let mut event = notify(&app, &agent, 1, false);
    event["payload"]["require_idle"] = json!(true);
    receive(&mut app, event.clone());
    // Acknowledged at once so the stream keeps flowing, but held while busy.
    let acked = std::iter::from_fn(|| frames.try_recv().ok()).any(|f| f["type"] == "ack");
    assert!(acked);
    drop(app.events_deliver());
    assert_eq!(app.delegation.events.pending.len(), 1);
    assert!(agent.lines().is_empty());
    std::thread::sleep(amux::session::IDLE_AFTER + Duration::from_millis(100));
    deliver(&mut app);
    assert_eq!(wait_lines(&agent, 1), [text(&app, &agent, &event)]);
    assert_eq!(results(&mut frames), ["delivered"]);
}

/// The real desktop against a real embedded server, through the production
/// event client: a fixture agent in a native terminal goes idle, the server
/// assigns it a new task, and the desktop delivers the offer into its terminal.
#[cfg(any(target_os = "linux", windows))]
#[test]
fn desktop_and_embedded_server_assign_an_idle_agent_end_to_end() {
    use amux_gateway_backend::{db, runtime as server};
    const KEY: &str = "desktop-e2e-fixture-key";
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let state = tempfile::tempdir().unwrap();
    let listener = runtime
        .block_on(tokio::net::TcpListener::bind("127.0.0.1:0"))
        .unwrap();
    let endpoint = format!("http://127.0.0.1:{}", listener.local_addr().unwrap().port());
    let config = server::RuntimeConfig {
        listen_addr: "127.0.0.1:1".parse().unwrap(),
        database: db::DatabaseConfig::sqlite(state.path().join("e2e.sqlite").to_string_lossy()),
        api_key: KEY.into(),
        default_channel: "discord".into(),
        retention_days: 30,
        bot_retention_days: 7,
        artifact_operations: db::ArtifactOperationsEnvelope::default(),
        artifact_body_schema_enabled: true,
        artifact_auth_enforced: false,
        ui_auth_enabled: true,
        discord: None,
        whatsapp: None,
    };
    runtime.spawn(async move { server::serve(config, listener).await.unwrap() });
    let http = reqwest::Client::new();
    let call = |method: reqwest::Method, path: String, body: Value| {
        let request = http
            .request(method, format!("{endpoint}{path}"))
            .bearer_auth(KEY)
            .json(&body);
        runtime.block_on(async move {
            let response = request.send().await.unwrap();
            assert!(
                response.status().is_success(),
                "{path}: {}",
                response.status()
            );
            response.json::<Value>().await.unwrap()
        })
    };
    call(
        reqwest::Method::POST,
        "/v1/projects".into(),
        json!({"ident": "e2e", "channel": "discord"}),
    );

    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Claude, "ready");
    app.registry = Some(agent.directory.clone());
    app.preferences.current.automatic_notifications = true;
    project(&mut app, &agent, "e2e");
    scan(&mut app);
    let me = session(&app, &agent);
    let profile = amux::gateway_transport::TransportProfile::legacy(&endpoint, KEY.into()).unwrap();
    let (outgoing, outgoing_rx) = mpsc::channel(256);
    let (incoming_tx, mut incoming) = mpsc::channel(256);
    let subscription = amux::event_client::Subscription {
        consumer: Uuid::new_v4(),
        events: ["agent.notify", "agent.idle", "agent.busy", "task.created"]
            .map(String::from)
            .to_vec(),
        targets: vec![me.clone()],
    };
    runtime.spawn(amux::event_client::run(
        profile,
        subscription,
        outgoing_rx,
        incoming_tx,
    ));
    app.delegation.events.outgoing = Some(outgoing);
    let mut task_id = None;
    let deadline = Instant::now() + Duration::from_secs(90);
    loop {
        if Instant::now() >= deadline {
            panic!(
                "the server never assigned the idle agent: connected={} busy={:?} task={:?} status={} candidates={:?} lines={:?}",
                app.delegation.events.connected,
                find(&app, 0).delegation.server_busy,
                task_id,
                app.delegation.events.status,
                app.delegation
                    .candidates
                    .iter()
                    .map(|c| (c.target.session, c.target.project.clone()))
                    .collect::<Vec<_>>(),
                agent.lines()
            );
        }
        while let Ok(message) = incoming.try_recv() {
            let epoch = app.delegation.epoch;
            drop(app.delegation_message(epoch, Event::Router(message)));
        }
        if app.delegation.events.connected {
            app.events_report();
            drop(app.events_deliver());
            let verified = app
                .delegation
                .events
                .delivering
                .as_ref()
                .map(|delivery| events::verify(&delivery.candidate));
            if let Some(verified) = verified {
                let epoch = app.delegation.epoch;
                drop(app.delegation_message(epoch, verification(&app, verified)));
            }
        }
        // Create the task once the server sees the agent idle (30 s without output).
        if task_id.is_none() && find(&app, 0).delegation.server_busy == Some(false) {
            let task = call(
                reqwest::Method::POST,
                "/v1/projects/e2e/tasks".into(),
                json!({"title": "Desktop end-to-end"}),
            );
            task_id = task["id"].as_str().map(str::to_owned);
        }
        if agent
            .lines()
            .iter()
            .any(|line| line.contains("Queued task review"))
        {
            break;
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    let id = task_id.unwrap();
    let line = agent
        .lines()
        .into_iter()
        .find(|line| line.contains("Queued task review"))
        .unwrap();
    assert!(
        line.contains("Desktop end-to-end") && line.contains(&id),
        "{line}"
    );
    let task = call(
        reqwest::Method::GET,
        format!("/v1/projects/e2e/tasks/{id}"),
        Value::Null,
    );
    assert_eq!(task["status"], "in_progress");
    assert_eq!(
        task["owner_origin"]["session_id"], me,
        "claimed as the desktop's agent"
    );
    // The Events panel shows the delivered offer.
    assert!(
        app.events_cards()
            .iter()
            .any(|card| card.detail.contains("delivered"))
    );
}

/// The server offers work based on the output time this desktop reported; a
/// slightly different local idle clock must not turn that offer away.
#[cfg(any(target_os = "linux", windows))]
#[test]
fn offers_are_judged_against_the_reported_output_time() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
    app.registry = Some(agent.directory.clone());
    let mut frames = router(&mut app);
    project(&mut app, &agent, "alpha");
    scan(&mut app);
    // Reported now, while the fixture's own output is still recent locally.
    app.events_report();
    assert!(std::iter::from_fn(|| frames.try_recv().ok()).any(|f| f["id"] == "terminal.activity"));
    let event = notify(&app, &agent, 1, true);
    receive(&mut app, event.clone());
    deliver(&mut app);
    assert_eq!(wait_lines(&agent, 1), [text(&app, &agent, &event)]);
    assert_eq!(results(&mut frames), ["delivered"]);
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn server_restart_notices_wait_for_idle_across_resubscription_and_report_outcome() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
    app.registry = Some(agent.directory.clone());
    let frames = router(&mut app);
    project(&mut app, &agent, "alpha");
    scan(&mut app);
    let mut reminder = notify(&app, &agent, 1, false);
    reminder["payload"]["require_idle"] = json!(true);
    reminder["payload"]["heading"] = json!("In-progress task reminder");
    reminder["payload"]["instructions"] = json!("Continue it, mark it done, or release it.");
    receive(&mut app, reminder.clone());
    drop(app.events_deliver());
    assert_eq!(app.delegation.events.pending.len(), 1, "busy owner waits");
    app.events_stop();
    drop(frames);
    let mut frames = router(&mut app);
    assert_eq!(app.delegation.events.pending.len(), 1);
    std::thread::sleep(amux::session::IDLE_AFTER + Duration::from_millis(100));
    deliver(&mut app);
    let lines = wait_lines(&agent, 1);
    assert!(lines[0].contains("In-progress task reminder"));
    assert!(lines[0].contains("Continue it, mark it done, or release it."));
    assert_eq!(results(&mut frames), ["delivered"]);

    // An ack lost during reconnect must not paste an already delivered notice again.
    app.events_stop();
    let _frames = router(&mut app);
    receive(&mut app, reminder);
    assert!(app.delegation.events.pending.is_empty());

    // A real bridge reset is a different logical connection and clears queued input.
    let queued = notify(&app, &agent, 2, false);
    receive(&mut app, queued);
    app.delegation.stop();
    assert!(app.delegation.events.pending.is_empty());
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn client_run_heartbeat_is_stable_across_resubscription_and_changes_with_desktop() {
    let mut app = desktop();
    let mut frames = router(&mut app);
    app.events_report();
    let heartbeat = std::iter::from_fn(|| frames.try_recv().ok())
        .find(|frame| frame["id"] == "client.heartbeat")
        .unwrap();
    let run = heartbeat["payload"]["client_run"].as_str().unwrap();
    assert_eq!(Uuid::parse_str(run).unwrap(), app.client_run);
    app.events_stop();
    let mut frames = router(&mut app);
    app.events_report();
    let heartbeat = std::iter::from_fn(|| frames.try_recv().ok())
        .find(|frame| frame["id"] == "client.heartbeat")
        .unwrap();
    assert_eq!(heartbeat["payload"]["client_run"], run);
    assert_ne!(desktop().client_run, app.client_run);
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn heartbeat_observes_unresolved_native_agents_and_empty_inventory_with_machine_os() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
    app.registry = Some(agent.directory.clone());
    let mut frames = router(&mut app);
    scan(&mut app);
    assert_eq!(app.delegation.candidates.len(), 1);
    assert!(app.delegation.candidates[0].target.project.is_empty());
    human(&mut app, 0, b"draft while project is unresolved");
    app.events_report();
    let reported: Vec<_> = std::iter::from_fn(|| frames.try_recv().ok()).collect();
    let heartbeat = reported
        .iter()
        .find(|frame| frame["id"] == "client.heartbeat")
        .unwrap();
    let unresolved = heartbeat["payload"]["agents"][0].clone();
    assert_eq!(heartbeat["payload"]["agents"].as_array().unwrap().len(), 1);
    assert_eq!(heartbeat["payload"]["os"], std::env::consts::OS);
    assert_eq!(unresolved["project"], "");
    assert!(
        unresolved.get("canonical_remote").is_none(),
        "observation does not invent a repository binding"
    );
    let activity = reported
        .iter()
        .find(|frame| frame["id"] == "terminal.activity")
        .unwrap();
    assert_eq!(activity["payload"]["session_id"], unresolved["session_id"]);
    assert_eq!(
        activity["payload"]["instance_id"],
        unresolved["instance_id"]
    );
    assert_eq!(activity["payload"]["draft"], true);
    assert!(activity["payload"]["output_at"].is_i64());

    project(&mut app, &agent, "alpha");
    scan(&mut app);
    app.events_report();
    let reported: Vec<_> = std::iter::from_fn(|| frames.try_recv().ok()).collect();
    let heartbeat = reported
        .iter()
        .find(|frame| frame["id"] == "client.heartbeat")
        .unwrap();
    let resolved = &heartbeat["payload"]["agents"][0];
    assert_eq!(resolved["project"], "alpha");
    for field in ["session_id", "instance_id", "generation"] {
        assert_eq!(
            resolved[field], unresolved[field],
            "resolving a project must not replace the native identity: {field}"
        );
    }
    assert!(
        reported
            .iter()
            .any(|frame| frame["id"] == "terminal.activity"
                && frame["payload"]["session_id"] == resolved["session_id"])
    );

    drop(tab(&mut app, 0).unwrap().terminal.take());
    scan(&mut app);
    assert!(app.delegation.candidates.is_empty());
    app.events_report();
    let reported: Vec<_> = std::iter::from_fn(|| frames.try_recv().ok()).collect();
    let heartbeat = reported
        .iter()
        .find(|frame| frame["id"] == "client.heartbeat")
        .unwrap();
    assert_eq!(heartbeat["payload"]["agents"], json!([]));
    assert_eq!(
        heartbeat["payload"]["os"],
        std::env::consts::OS,
        "an empty inventory still reports this connected machine"
    );
    assert!(
        !reported
            .iter()
            .any(|frame| frame["id"] == "terminal.activity")
    );
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn heartbeat_advertises_only_a_confirmed_live_execution_repository_binding() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
    app.registry = Some(agent.directory.clone());
    project(&mut app, &agent, "alpha");
    scan(&mut app);
    app.workspaces[0].gateway.project = "alpha".into();
    let candidate = app.delegation.candidates[0].clone();
    let mut frames = router(&mut app);
    app.events_report();
    let heartbeat = std::iter::from_fn(|| frames.try_recv().ok())
        .find(|frame| frame["id"] == "client.heartbeat")
        .unwrap();
    assert!(
        heartbeat["payload"]["agents"][0]
            .get("canonical_remote")
            .is_none()
    );
    for remote in [Some("https://github.com/example/repo".to_owned()), None] {
        let panel = &mut app.workspaces[0].gateway.routing;
        panel.checked = Some(Instant::now());
        panel.directory = Some(amux::task_routing::Directory {
            canonical_remote: remote.clone(),
            targets: vec![amux::task_routing::Available {
                target: amux::task_routing::SessionTarget {
                    origin: candidate.target.origin.clone().unwrap(),
                    generation: candidate.target.executor_generation(),
                },
                seen_at: unix_ms() as i64,
                expires_at: unix_ms() as i64 + 120_000,
                display_name: None,
                hostname: None,
            }],
        });
        app.events_report();
        let heartbeat = std::iter::from_fn(|| frames.try_recv().ok())
            .find(|frame| frame["id"] == "client.heartbeat")
            .unwrap();
        assert_eq!(
            heartbeat["payload"]["agents"][0].get("canonical_remote"),
            Some(&json!(remote))
        );
    }
    app.workspaces[0]
        .gateway
        .routing
        .directory
        .as_mut()
        .unwrap()
        .targets[0]
        .target
        .generation = Uuid::new_v4();
    app.events_report();
    let heartbeat = std::iter::from_fn(|| frames.try_recv().ok())
        .find(|frame| frame["id"] == "client.heartbeat")
        .unwrap();
    assert!(
        heartbeat["payload"]["agents"][0]
            .get("canonical_remote")
            .is_none(),
        "another execution is not confirmation"
    );
}

#[test]
fn status_bar_shows_gateway_wide_stats_only_while_connected() {
    let mut app = desktop();
    app.preferences.current = amux::gateway_preferences::GatewayPreferences {
        mode: amux::gateway_preferences::GatewayMode::Client,
        enrolled: Some(amux::gateway_preferences::EnrolledConnection {
            url: "https://gateway.example:24182".into(),
            cluster_id: Uuid::new_v4().to_string(),
            node_id: Uuid::new_v4().to_string(),
            profile_path: PathBuf::from("unused-node-profile.json"),
        }),
        automatic_notifications: true,
        ..Default::default()
    };
    app.design.gateway_stats = Some(amux::gateway::PresenceSummary {
        machines: 5,
        os: vec!["linux".into(), "macos".into(), "windows".into()],
        agents: 77,
        active: 23,
    });
    assert_eq!(app.design_gateway_line(), "Gateway: Connecting");
    app.delegation.events.connected = true;
    assert_eq!(
        app.design_gateway_line(),
        "Gateway: Connected · 5 machines · Linux, macOS, Windows · 77 agents · 23 active"
    );
    let _ = app.update(Message::GatewayStats(Err("old gateway".into())));
    assert_eq!(app.design_gateway_line(), "Gateway: Connected");
}

#[test]
fn project_inventory_rejects_old_gateway_replies_after_switch_and_disconnect() {
    use amux::{gateway::ProjectOverview, gateway_preferences::GatewayMode};
    let mut app = desktop();
    let root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    std::fs::create_dir_all(&root).unwrap();
    let settings = tempfile::tempdir_in(root).unwrap();
    app.preferences.path = Some(settings.path().join("preferences.json"));
    // Dropping the returned Tasks drives the real synchronous preference and
    // callback paths without making a gateway request or opening a provider.
    app.preferences.current.mode = GatewayMode::Legacy;
    app.preferences.current.url = "http://127.0.0.1:41001".into();
    app.preferences.current.api_key = "fixture-key".into();
    app.preferences.draft = app.preferences.current.clone();
    let projects = |ident: &str| {
        vec![ProjectOverview {
            ident: ident.into(),
            ..Default::default()
        }]
    };
    let old_epoch = app.delegation.epoch;
    drop(app.update(Message::ProjectsLoaded(
        old_epoch,
        Ok(projects("gateway-a")),
    )));
    app.design.projects_polled = Some(Instant::now());
    drop(
        app.design_message(super::super::desktop_design::Action::ProjectMenu(
            "gateway-a".into(),
        )),
    );
    app.preferences.draft.url = "http://127.0.0.1:41002".into();
    drop(app.save_preferences());
    assert_eq!(app.preferences.current.url, "http://127.0.0.1:41002");
    assert!(app.design.projects.is_empty());
    assert!(
        app.design.projects_polled.is_none(),
        "the new connection must not inherit the old poll delay"
    );
    let status = app.status.clone();
    drop(app.design_message(super::super::desktop_design::Action::CheckoutProject(false)));
    assert_eq!(
        app.status, status,
        "the old project's checkout menu must be closed"
    );
    let current_epoch = app.delegation.epoch;
    assert_ne!(old_epoch, current_epoch);
    drop(app.update(Message::ProjectsLoaded(
        current_epoch,
        Ok(projects("gateway-b")),
    )));
    for result in [Ok(projects("gateway-a")), Err("late A error".into())] {
        drop(app.update(Message::ProjectsLoaded(old_epoch, result)));
        assert_eq!(app.design.projects[0].ident, "gateway-b");
    }
    // A display preference alone must not invalidate this connection's inventory.
    app.preferences.draft.projects_dir = "projects".into();
    drop(app.save_preferences());
    assert_eq!(app.delegation.epoch, current_epoch);
    assert_eq!(app.design.projects[0].ident, "gateway-b");

    // Switching back also reloads; the first A connection's reply stays stale.
    app.preferences.draft.url = "http://127.0.0.1:41001".into();
    drop(app.save_preferences());
    let returned_epoch = app.delegation.epoch;
    assert!(app.design.projects.is_empty());
    drop(app.update(Message::ProjectsLoaded(old_epoch, Ok(projects("old-a")))));
    assert!(app.design.projects.is_empty());
    drop(app.update(Message::ProjectsLoaded(
        returned_epoch,
        Ok(projects("fresh-a")),
    )));
    assert_eq!(app.design.projects[0].ident, "fresh-a");

    app.design.projects_polled = Some(Instant::now());
    app.preferences.draft.url.clear();
    app.preferences.draft.api_key.clear();
    drop(app.save_preferences());
    assert!(!app.preferences.current.has_connection());
    assert!(app.design.projects.is_empty());
    assert!(app.design.projects_polled.is_none());
    for result in [Ok(projects("fresh-a")), Err("late A error".into())] {
        drop(app.update(Message::ProjectsLoaded(returned_epoch, result)));
        assert!(app.design.projects.is_empty());
    }
}

#[test]
fn sidebar_lists_other_projects_by_where_they_are_checked_out() {
    use amux::gateway::{ProjectMachine, ProjectOverview};
    let mut app = desktop();
    app.workspaces[0].gateway.project = "local".into();
    let project = |ident: &str, hosts: &[&str]| ProjectOverview {
        ident: ident.into(),
        canonical_remote: Some(format!("github.com/nitecon/{ident}")),
        machines: hosts
            .iter()
            .map(|host| ProjectMachine {
                hostname: (*host).into(),
                origin: Some(format!("git@github.com:nitecon/{ident}.git")),
            })
            .collect(),
        agents: 2,
        active: 1,
    };
    app.design.projects = vec![
        project("zeta", &[]),
        project("local", &["Defcon1"]),
        project("beta", &["Defcon2"]),
        project("alpha", &[]),
        project("gamma", &["Defcon1", "Defcon2"]),
    ];
    let order: Vec<_> = app
        .sidebar_projects()
        .iter()
        .map(|p| p.ident.as_str())
        .collect();
    assert_eq!(
        order,
        ["beta", "gamma", "alpha", "zeta"],
        "open workspaces are not repeated"
    );
    assert_eq!(
        app.project_lines("gamma", Some(std::path::Path::new("/src/gamma"))),
        [
            "Project: gamma",
            "Checked out at: /src/gamma",
            "Repository: github.com/nitecon/gamma",
            "Machines: Defcon1, Defcon2",
            "Agents: 2 (1 active)",
        ]
    );
    assert_eq!(
        app.project_lines("alpha", None)[2],
        "Machines: not checked out anywhere"
    );

    // Checking out into the project dir needs the preference; a project no
    // machine has checked out has no URL to clone.
    drop(
        app.design_message(crate::desktop::desktop_design::Action::ProjectMenu(
            "beta".into(),
        )),
    );
    drop(
        app.design_message(crate::desktop::desktop_design::Action::CheckoutProject(
            true,
        )),
    );
    assert!(
        app.status.contains("Default Projects directory"),
        "{}",
        app.status
    );
    assert!(!app.workspace_dialog.open);
    drop(
        app.design_message(crate::desktop::desktop_design::Action::ProjectMenu(
            "alpha".into(),
        )),
    );
    drop(
        app.design_message(crate::desktop::desktop_design::Action::CheckoutProject(
            false,
        )),
    );
    assert!(
        app.status.contains("No machine has reported a Git URL"),
        "{}",
        app.status
    );
    drop(
        app.design_message(crate::desktop::desktop_design::Action::ProjectMenu(
            "beta".into(),
        )),
    );
    drop(
        app.design_message(crate::desktop::desktop_design::Action::CheckoutProject(
            false,
        )),
    );
    assert!(
        app.workspace_dialog.open,
        "check out at location opens the file browser"
    );
}
