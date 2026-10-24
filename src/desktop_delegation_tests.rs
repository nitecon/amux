//! Controlled native-terminal delivery tests. Fixture agents are copies of this
//! repo-built test binary renamed codex/claude under target/test-fixtures and run
//! inside real native terminals (Linux PTY, Windows ConPTY). No installed provider,
//! model turn or gateway connection is ever started; the background scan and
//! verification tasks are driven manually with the same native discovery calls.
use super::super::{Reminders, tests::desktop};
use super::*;
use amux::delegation::{Event as Lifecycle, Kind};
use std::{io::Write, path::Path, sync::OnceLock};

#[cfg(target_os = "linux")]
use std::io::Read;

const LOG: &str = "AMUX_DESKTOP_FIXTURE_LOG";
const LAYOUT: &str = "AMUX_DESKTOP_FIXTURE_LAYOUT";
const ENTRY: &str = "desktop::desktop_delegation::tests::fixture_process";
const CONNECTION: u64 = 7;

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
            format!("{} {draft}", self.marker),
        ];
        if multiline {
            rows.push("  second draft line".into());
        }
        if self.marker == '❯' {
            rows.push("─".repeat(40));
        } else if !multiline {
            rows.push(String::new());
        }
        rows.push("  ? for shortcuts".into());
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
    fn feed(&mut self, mut rest: &str) {
        while let Some(c) = rest.chars().next() {
            let mut len = c.len_utf8();
            match c {
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
                '\x1b' if rest.starts_with("\x1b[") || rest.starts_with("\x1bO") => {
                    len = rest
                        .bytes()
                        .enumerate()
                        .skip(2)
                        .find(|(_, b)| (0x40..=0x7e).contains(b))
                        .map_or(rest.len(), |(i, _)| i + 1);
                    let sequence = &rest[..len];
                    if matches!(sequence, "\x1b[H" | "\x1bOH") {
                        self.cursor = 0;
                    }
                    self.raw.push_str(sequence);
                }
                c if !c.is_control() => {
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
        fixture.draw();
        let mut buffer = [0u8; 4096];
        let mut stdin = std::io::stdin().lock();
        while let Ok(n @ 1..) = stdin.read(&mut buffer) {
            fixture.feed(&String::from_utf8_lossy(&buffer[..n]));
        }
    }
    #[cfg(not(target_os = "linux"))]
    {
        // Console line input: the console echoes the draft and Enter submits a line.
        use std::io::BufRead;
        fixture.draw();
        for line in std::io::stdin().lock().lines() {
            let Ok(line) = line else { break };
            fixture.raw = line.trim_end_matches(['\r', '\n']).to_owned();
            fixture.submit();
            fixture.draw();
        }
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
struct Agent {
    directory: PathBuf,
    log: PathBuf,
}

impl Agent {
    fn lines(&self) -> Vec<String> {
        std::fs::read_to_string(&self.log)
            .map(|log| log.lines().map(str::to_owned).collect())
            .unwrap_or_default()
    }
}

fn spawn(app: &mut Desktop, workspace: usize, id: u64, provider: Provider, layout: &str) -> Agent {
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
            recipient: None,
            reminders: Reminders::default(),
            delegation: TabState::default(),
        });
    }
    let tab = pane.tabs.iter_mut().find(|t| t.id == id).unwrap();
    tab.provider = Some(provider);
    tab.delegation = TabState::capture(&terminal, id + 1, &directory);
    tab.terminal = Some(terminal);
    let agent = Agent {
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

fn find(app: &Desktop, id: u64) -> &Tab {
    app.workspaces
        .iter()
        .flat_map(|w| w.panes.iter().flat_map(|(_, p)| &p.tabs))
        .find(|t| t.id == id)
        .unwrap()
}

fn grid(app: &Desktop, id: u64) -> Option<TerminalGridObservation> {
    find(app, id).terminal.as_ref()?.grid_observation(32_768)
}

fn classify(app: &Desktop, id: u64) -> InputState {
    let tab = find(app, id);
    delegation_readiness::classify(&grid(app, id).unwrap(), tab.provider.unwrap())
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
#[cfg(target_os = "linux")]
fn human(app: &mut Desktop, id: u64, bytes: &[u8]) {
    drop(app.update(Message::Terminal(iced_term::Event::BackendCall(
        id,
        BackendCommand::Write(bytes.to_vec()),
    ))));
}

/// An approved bridge with a live (fake) subscribed connection; no gateway.
fn bridge(app: &mut Desktop, journal: &Path) -> mpsc::Receiver<connection::Command> {
    app.registry = Some(journal.parent().unwrap().to_path_buf());
    app.preferences.current.automatic_notifications = true;
    let (commands, frames) = mpsc::channel(64);
    app.delegation.initialized = true;
    let queue = Queue::open(journal.into(), "fixture").unwrap();
    // Resume at the durable cursor with no offline backlog; later IDs are live.
    let cursor = queue.journal.cursor.unwrap_or(0);
    app.delegation.queue = Some(queue);
    app.delegation.commands = Some(commands);
    let epoch = app.delegation.epoch;
    drop(app.delegation_message(
        epoch,
        Event::Stream(ConnectionEvent::Subscribed {
            connection: CONNECTION,
            cursor,
            replay_through: cursor,
        }),
    ));
    assert_eq!(app.delegation.connection, Some(CONNECTION));
    frames
}

fn project(app: &mut Desktop, agent: &Agent, project: &str) {
    app.delegation.projects.insert(
        agent.directory.clone(),
        (Instant::now(), Some(project.into())),
    );
}

/// The background scan, run synchronously with the same native discovery.
fn scan(app: &mut Desktop, arrival: Option<Lifecycle>) {
    let found = app
        .delegation_roots()
        .into_iter()
        .filter_map(|root| discover_root(root, app.registry.as_deref()?))
        .collect();
    app.delegation.scan_serial += 1;
    let serial = app.delegation.scan_serial;
    let epoch = app.delegation.epoch;
    drop(app.delegation_message(
        epoch,
        Event::Scanned {
            serial,
            found,
            arrival,
        },
    ));
}

fn lifecycle(id: i64, project: &str) -> Lifecycle {
    Lifecycle {
        id,
        project: project.into(),
        task: Uuid::new_v4(),
        kind: Kind::Created,
        created_at: unix_ms() as i64,
        origin: None,
        goal: None,
    }
}

fn outcome(app: &Desktop, event: i64, terminal: u64) -> Option<Outcome> {
    app.delegation
        .queue
        .as_ref()?
        .journal
        .receipts
        .iter()
        .find(|r| r.event.id == event)?
        .recipients
        .iter()
        .find(|r| r.target.terminal == terminal)
        .map(|r| r.outcome)
}

/// Rebuild the final proof exactly as the tick captured it before its background check.
fn fence_proof(app: &Desktop) -> Proof {
    let queue = app.delegation.queue.as_ref().unwrap();
    let (event, target) = queue
        .journal
        .receipts
        .iter()
        .find_map(|r| {
            r.recipients
                .iter()
                .find(|x| x.outcome == Outcome::Submitting)
                .map(|x| (r.event.clone(), x.target.clone()))
        })
        .expect("a durable submitting fence");
    let candidate = app
        .delegation
        .candidates
        .iter()
        .find(|c| c.target == target)
        .cloned()
        .unwrap();
    let tab = find(app, target.terminal);
    Proof {
        attempt: Attempt { event, target },
        candidate,
        revision: tab.delegation.revision,
        grid: tab.delegation.grid.clone().unwrap(),
        started: Instant::now(),
    }
}

/// Owned-tree verification as the background task performs it.
fn native_verify(proof: &Proof) -> bool {
    let c = &proof.candidate;
    delegation_process::verify(
        c.root.pid,
        &c.root.generation,
        c.root.foreground,
        &c.process,
    )
}

fn verified(app: &mut Desktop, proof: Proof, verified: bool) {
    let epoch = app.delegation.epoch;
    drop(app.delegation_message(epoch, Event::Verified(Box::new(proof), verified)));
}

/// Acknowledgment frames sent to the (fake) connection, each recorded back as the
/// gateway would, so the next snapshot of the same event can be sent.
fn drain(
    app: &mut Desktop,
    frames: &mut mpsc::Receiver<connection::Command>,
) -> Vec<serde_json::Value> {
    let mut acks = Vec::new();
    while let Ok(command) = frames.try_recv() {
        assert_eq!(command.connection, CONNECTION);
        let event_id = command.frame["event_id"].as_i64().unwrap();
        let status = command.frame["status"].as_str().unwrap().to_owned();
        acks.push(command.frame);
        let epoch = app.delegation.epoch;
        drop(app.delegation_message(
            epoch,
            Event::Stream(ConnectionEvent::Recorded {
                event_id,
                status,
                cursor: event_id,
            }),
        ));
    }
    acks
}

/// Tick until the bridge holds a submitting fence for a final UI check.
fn until_verifying(app: &mut Desktop, frames: &mut mpsc::Receiver<connection::Command>) {
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        drop(app.delegation_tick());
        drain(app, frames);
        if app.delegation.verifying {
            return;
        }
        assert!(
            Instant::now() < deadline,
            "native prompt never became ready"
        );
        std::thread::sleep(Duration::from_millis(20));
    }
}

/// Tick for a while and assert no attempt starts.
fn never_verifies(app: &mut Desktop, frames: &mut mpsc::Receiver<connection::Command>) {
    let deadline = Instant::now() + Duration::from_millis(1500);
    while Instant::now() < deadline {
        drop(app.delegation_tick());
        drain(app, frames);
        assert!(!app.delegation.verifying, "unready prompt began delivery");
        std::thread::sleep(Duration::from_millis(20));
    }
}

/// Complete every ready attempt through native verification until `done`.
fn deliver(
    app: &mut Desktop,
    frames: &mut mpsc::Receiver<connection::Command>,
    done: impl Fn(&Desktop) -> bool,
) -> Vec<serde_json::Value> {
    let mut acks = Vec::new();
    let deadline = Instant::now() + Duration::from_secs(15);
    while !done(app) {
        assert!(Instant::now() < deadline, "delivery did not complete");
        drop(app.delegation_tick());
        if app.delegation.verifying {
            let proof = fence_proof(app);
            let ok = native_verify(&proof);
            verified(app, proof, ok);
        }
        acks.extend(drain(app, frames));
        std::thread::sleep(Duration::from_millis(20));
    }
    acks.extend(drain(app, frames));
    acks
}

fn wait_lines(agent: &Agent, count: usize) -> Vec<String> {
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

type Tamper = fn(&mut Proof);
type Change = fn(&mut Desktop, &mut Proof);
type Restore = fn(&mut Desktop);

#[test]
fn human_writes_mark_drafts_and_navigation_never_clears_them() {
    let mut state = TabState::default();
    let mut write = |bytes: &[u8]| {
        state.note_command(&BackendCommand::Write(bytes.to_vec()));
        (state.dirty, state.wait_for_output, state.revision)
    };
    assert_eq!(
        write(b"\x1b[A"),
        (false, false, 1),
        "navigation is not a draft"
    );
    assert_eq!(write(b" "), (true, false, 2), "whitespace is a draft");
    assert_eq!(write(b"\x1b[H"), (true, false, 3), "Home keeps the draft");
    assert_eq!(write(b"\x1bOH"), (true, false, 4));
    assert_eq!(write(b"\x1b[D"), (true, false, 5));
    assert_eq!(write(b"\r"), (false, true, 6), "Enter submits the draft");
    assert_eq!(write(b"\x1b[200~pasted\x1b[201~"), (true, true, 7));
    assert_eq!(write(b"\x15"), (false, true, 8));
    assert_eq!(write(b"a"), (true, true, 9));
    assert_eq!(write(b"\x03"), (false, true, 10));
    assert_eq!(write(b"\n"), (false, true, 11));
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn native_delivery_submits_the_exact_compact_notification_once() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Claude, "ready");
    let scratch = agent.directory.clone();
    let mut frames = bridge(&mut app, &scratch.join("delegation.json"));
    project(&mut app, &agent, "alpha");
    wait(&app, |app| classify(app, 0) == InputState::EmptyReady);
    let event = lifecycle(1, "alpha");
    scan(&mut app, Some(event.clone()));
    assert_eq!(outcome(&app, 1, 0), Some(Outcome::Queued));
    let target = &app.delegation.candidates[0].target;
    let pid = find(&app, 0)
        .terminal
        .as_ref()
        .unwrap()
        .child_pid()
        .unwrap();
    assert_eq!(target.pid, pid, "the PTY's own fixture agent is the target");
    assert_eq!(
        target.generation,
        amux::identity::process_generation(pid).unwrap()
    );
    let acks = deliver(&mut app, &mut frames, |app| {
        outcome(app, 1, 0) == Some(Outcome::Submitted)
    });
    assert_eq!(wait_lines(&agent, 1), [event.terminal_text()]);
    assert!(
        acks.iter()
            .any(|f| f["event_id"] == 1 && f["status"] == "injected"),
        "{acks:?}"
    );
    // A submitted notification is never replayed into the agent.
    never_verifies(&mut app, &mut frames);
    assert_eq!(agent.lines(), [event.terminal_text()]);
}

#[cfg(target_os = "linux")]
#[test]
fn native_bracketed_paste_prompt_receives_the_notification_as_one_paste() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "paste");
    let mut frames = bridge(&mut app, &agent.directory.join("delegation.json"));
    project(&mut app, &agent, "alpha");
    wait(&app, |app| {
        grid(app, 0).is_some_and(|g| {
            g.observation
                .mode
                .contains(iced_term::TermMode::BRACKETED_PASTE)
        }) && classify(app, 0) == InputState::EmptyReady
    });
    let event = lifecycle(1, "alpha");
    scan(&mut app, Some(event.clone()));
    deliver(&mut app, &mut frames, |app| {
        outcome(app, 1, 0) == Some(Outcome::Submitted)
    });
    assert_eq!(
        wait_lines(&agent, 1),
        [format!("\x1b[200~{}\x1b[201~", event.terminal_text())]
    );
}

#[cfg(target_os = "linux")]
#[test]
fn native_whitespace_home_draft_blocks_a_prompt_that_looks_empty() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Claude, "ready");
    let mut frames = bridge(&mut app, &agent.directory.join("delegation.json"));
    project(&mut app, &agent, "alpha");
    wait(&app, |app| classify(app, 0) == InputState::EmptyReady);
    human(&mut app, 0, b" ");
    wait(&app, |app| {
        grid(app, 0).is_some_and(|g| g.observation.cursor_column == 3)
    });
    human(&mut app, 0, b"\x1b[H");
    wait(&app, |app| {
        grid(app, 0).is_some_and(|g| g.observation.cursor_column == 2)
    });
    // The live grid is indistinguishable from an empty prompt; only the draft guard knows.
    assert_eq!(classify(&app, 0), InputState::EmptyReady);
    let event = lifecycle(1, "alpha");
    scan(&mut app, Some(event.clone()));
    assert_eq!(outcome(&app, 1, 0), Some(Outcome::Queued));
    never_verifies(&mut app, &mut frames);
    assert_eq!(outcome(&app, 1, 0), Some(Outcome::Queued));
    assert!(agent.lines().is_empty(), "draft was overwritten");
    // The human submits their own draft; the notification follows as a separate input.
    human(&mut app, 0, b"\r");
    assert_eq!(wait_lines(&agent, 1), [" \x1b[H"]);
    deliver(&mut app, &mut frames, |app| {
        outcome(app, 1, 0) == Some(Outcome::Submitted)
    });
    assert_eq!(
        wait_lines(&agent, 2),
        [" \x1b[H".to_owned(), event.terminal_text()]
    );
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn native_busy_and_multiline_prompts_never_receive_input() {
    let mut app = desktop();
    let busy = spawn(&mut app, 0, 0, Provider::Claude, "busy");
    let multiline = spawn(&mut app, 1, 1, Provider::Codex, "multiline");
    let mut frames = bridge(&mut app, &busy.directory.join("delegation.json"));
    project(&mut app, &busy, "alpha");
    project(&mut app, &multiline, "alpha");
    assert_eq!(classify(&app, 0), InputState::Busy);
    assert_eq!(classify(&app, 1), InputState::Unfinished);
    scan(&mut app, Some(lifecycle(1, "alpha")));
    assert_eq!(outcome(&app, 1, 0), Some(Outcome::Queued));
    assert_eq!(outcome(&app, 1, 1), Some(Outcome::Queued));
    never_verifies(&mut app, &mut frames);
    assert_eq!(outcome(&app, 1, 0), Some(Outcome::Queued));
    assert_eq!(outcome(&app, 1, 1), Some(Outcome::Queued));
    assert!(busy.lines().is_empty() && multiline.lines().is_empty());
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn stale_final_proofs_never_write_native_input() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Claude, "ready");
    let journal = agent.directory.join("delegation.json");
    let mut frames = bridge(&mut app, &journal);
    project(&mut app, &agent, "alpha");
    wait(&app, |app| classify(app, 0) == InputState::EmptyReady);
    scan(&mut app, Some(lifecycle(1, "alpha")));

    // Anything changed since the proof returns the work to the queue without input.
    let stale: [(&str, Tamper); 5] = [
        ("human input epoch", |p| p.revision += 1),
        ("screen frame", |p| p.grid.observation.cursor_column += 1),
        ("agent process generation", |p| {
            p.candidate.process.generation = "linux:stale:1".into()
        }),
        ("terminal foreground group", |p| {
            p.candidate.root.foreground = Some(1)
        }),
        ("terminal root generation", |p| {
            p.candidate.root.generation = "linux:stale:1".into()
        }),
    ];
    for (name, mutate) in stale {
        until_verifying(&mut app, &mut frames);
        let mut proof = fence_proof(&app);
        assert!(native_verify(&proof));
        mutate(&mut proof);
        verified(&mut app, proof, true);
        assert_eq!(outcome(&app, 1, 0), Some(Outcome::Queued), "{name}");
        assert!(agent.lines().is_empty(), "{name} wrote input");
    }
    // Failed owned-tree verification retires the attempt.
    until_verifying(&mut app, &mut frames);
    let proof = fence_proof(&app);
    verified(&mut app, proof, false);
    assert_eq!(outcome(&app, 1, 0), Some(Outcome::Skipped));

    // Consent, connection and proof age at the final check.
    let retired: [(&str, Change, Restore); 3] = [
        (
            "consent withdrawn",
            |app, _| app.preferences.current.automatic_notifications = false,
            |app| app.preferences.current.automatic_notifications = true,
        ),
        (
            "connection lost",
            |app, _| app.delegation.connection = None,
            |app| app.delegation.connection = Some(CONNECTION),
        ),
        (
            "expired proof",
            |_, p| p.started = Instant::now() - Duration::from_secs(3),
            |_| {},
        ),
    ];
    for (id, (name, mutate, restore)) in (2..).zip(retired) {
        scan(&mut app, Some(lifecycle(id, "alpha")));
        until_verifying(&mut app, &mut frames);
        let mut proof = fence_proof(&app);
        mutate(&mut app, &mut proof);
        verified(&mut app, proof, true);
        restore(&mut app);
        assert_eq!(outcome(&app, id, 0), Some(Outcome::Skipped), "{name}");
        assert!(agent.lines().is_empty(), "{name} wrote input");
    }

    // An event that expires while its proof is in flight is retired at the final check.
    let mut aging = lifecycle(5, "alpha");
    aging.created_at -= delegation::MAX_EVENT_AGE_MS - 1000;
    scan(&mut app, Some(aging.clone()));
    until_verifying(&mut app, &mut frames);
    let proof = fence_proof(&app);
    assert!(
        aging.fresh(unix_ms() as i64),
        "event expired before its proof"
    );
    std::thread::sleep(Duration::from_millis(1200));
    assert!(!aging.fresh(unix_ms() as i64));
    verified(&mut app, proof, true);
    assert_eq!(outcome(&app, 5, 0), Some(Outcome::Skipped), "expired event");

    // A proof that no longer matches the durable fence is ignored outright.
    scan(&mut app, Some(lifecycle(6, "alpha")));
    until_verifying(&mut app, &mut frames);
    let mut proof = fence_proof(&app);
    proof.attempt.event.task = Uuid::new_v4();
    verified(&mut app, proof, true);
    assert_eq!(outcome(&app, 6, 0), Some(Outcome::Submitting));
    let proof = fence_proof(&app);
    verified(&mut app, proof, false);
    assert_eq!(outcome(&app, 6, 0), Some(Outcome::Skipped));
    assert!(
        agent.lines().is_empty(),
        "mismatched or expired proof wrote input"
    );

    // A result from an earlier bridge epoch is ignored; stopping makes the fence uncertain.
    scan(&mut app, Some(lifecycle(7, "alpha")));
    until_verifying(&mut app, &mut frames);
    let late = fence_proof(&app);
    let epoch = app.delegation.epoch;
    drop(app.delegation_message(
        epoch.wrapping_add(1),
        Event::Verified(Box::new(late.clone()), true),
    ));
    assert_eq!(outcome(&app, 7, 0), Some(Outcome::Submitting));
    app.delegation.stop();
    drop(app.delegation_message(epoch, Event::Verified(Box::new(late), true)));
    assert!(app.delegation.queue.is_none());
    let reopened = Queue::open(journal.clone(), "fixture").unwrap();
    let fence = reopened
        .journal
        .receipts
        .iter()
        .find(|r| r.event.id == 7)
        .unwrap();
    assert_eq!(fence.recipients[0].outcome, Outcome::Uncertain);
    drop(reopened);
    assert!(agent.lines().is_empty(), "stale epoch wrote input");

    // Positive control: the same terminal still receives fresh work after reconnecting.
    let mut frames = bridge(&mut app, &journal);
    project(&mut app, &agent, "alpha");
    let event = lifecycle(8, "alpha");
    scan(&mut app, Some(event.clone()));
    deliver(&mut app, &mut frames, |app| {
        outcome(app, 8, 0) == Some(Outcome::Submitted)
    });
    assert_eq!(wait_lines(&agent, 1), [event.terminal_text()]);
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn native_peer_agents_have_unique_sessions_and_isolated_input() {
    let mut app = desktop();
    let alpha = spawn(&mut app, 0, 0, Provider::Claude, "ready");
    let codex = spawn(&mut app, 1, 1, Provider::Codex, "ready");
    let claude = spawn(&mut app, 1, 2, Provider::Claude, "ready");
    let mut frames = bridge(&mut app, &alpha.directory.join("delegation.json"));
    project(&mut app, &alpha, "alpha");
    project(&mut app, &codex, "beta");
    project(&mut app, &claude, "beta");
    for id in 0..3 {
        wait(&app, |app| classify(app, id) == InputState::EmptyReady);
    }
    scan(&mut app, None);
    let targets = app.delegation.targets();
    assert_eq!(targets.len(), 3);
    for (i, a) in targets.iter().enumerate() {
        let tab = find(&app, a.terminal);
        let pid = tab.terminal.as_ref().unwrap().child_pid().unwrap();
        assert_eq!(a.pid, pid, "each target is its own terminal's agent");
        for b in &targets[i + 1..] {
            assert_ne!(a.session, b.session);
            assert_ne!(a.pid, b.pid);
            assert_ne!(a.terminal, b.terminal);
        }
    }
    assert_eq!(
        targets
            .iter()
            .filter(|t| t.workspace == targets[1].workspace)
            .count(),
        2
    );
    // Plain-shell discovery publishes amux-owned provider/terminal identities.
    let records = amux::identity::live_agents(app.registry.as_deref().unwrap()).unwrap();
    assert_eq!(records.len(), 3);
    for target in &targets {
        let record = records.iter().find(|r| r.pid == target.pid).unwrap();
        let name = format!(
            "{}-{}",
            record.identity.provider.command(),
            target.terminal + 1
        );
        assert_eq!(record.identity.agent_id, name);
        let project = amux::project_identity::resolve(&target.directory).unwrap();
        assert_eq!(record.identity.project_id, project.id);
        assert_eq!(target.session, project.session_id(&name));
        assert_eq!(
            target.origin.as_ref().unwrap().instance_id,
            record.identity.instance_id
        );
        assert_eq!(
            find(&app, target.terminal).delegation.agent_name.as_deref(),
            Some(name.as_str())
        );
    }
    // Sessions are stable across rescans of the same process attachment.
    scan(&mut app, None);
    assert_eq!(app.delegation.targets(), targets);

    let beta = lifecycle(1, "beta");
    scan(&mut app, Some(beta.clone()));
    assert_eq!(outcome(&app, 1, 0), Some(Outcome::Skipped));
    let acks = deliver(&mut app, &mut frames, |app| {
        outcome(app, 1, 1) == Some(Outcome::Submitted)
            && outcome(app, 1, 2) == Some(Outcome::Submitted)
    });
    assert_eq!(wait_lines(&codex, 1), [beta.terminal_text()]);
    assert_eq!(wait_lines(&claude, 1), [beta.terminal_text()]);
    assert!(alpha.lines().is_empty(), "peer project received input");
    assert!(
        acks.iter()
            .any(|f| f["event_id"] == 1 && f["status"] == "injected")
    );

    let own = lifecycle(2, "alpha");
    scan(&mut app, Some(own.clone()));
    assert_eq!(outcome(&app, 2, 1), Some(Outcome::Skipped));
    assert_eq!(outcome(&app, 2, 2), Some(Outcome::Skipped));
    deliver(&mut app, &mut frames, |app| {
        outcome(app, 2, 0) == Some(Outcome::Submitted)
    });
    assert_eq!(wait_lines(&alpha, 1), [own.terminal_text()]);
    std::thread::sleep(Duration::from_millis(200));
    assert_eq!(codex.lines(), [beta.terminal_text()]);
    assert_eq!(claude.lines(), [beta.terminal_text()]);
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn native_plain_shell_identity_suppresses_its_own_notification() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Codex, "ready");
    let mut frames = bridge(&mut app, &agent.directory.join("delegation.json"));
    project(&mut app, &agent, "amux");
    wait(&app, |app| classify(app, 0) == InputState::EmptyReady);
    scan(&mut app, None);
    let target = app.delegation.targets().remove(0);
    let mut event = lifecycle(1, "amux");
    event.origin = target.origin;
    assert!(event.origin.is_some());
    scan(&mut app, Some(event));
    assert_eq!(outcome(&app, 1, 0), Some(Outcome::Skipped));
    never_verifies(&mut app, &mut frames);
    assert!(agent.lines().is_empty());
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn gateway_clock_delivers_fresh_comments_despite_local_clock_skew_and_expires_old_ones() {
    let mut app = desktop();
    let agent = spawn(&mut app, 0, 0, Provider::Claude, "ready");
    let mut frames = bridge(&mut app, &agent.directory.join("delegation.json"));
    project(&mut app, &agent, "amux");
    wait(&app, |app| classify(app, 0) == InputState::EmptyReady);
    let clock = amux::gateway_stream::GatewayClock::from_http_date("Sat, 24 Oct 2020 05:16:29 GMT")
        .unwrap();
    let mut event = lifecycle(1, "amux");
    event.kind = Kind::Commented;
    event.created_at = clock.now_ms();
    assert!(
        !event.fresh(unix_ms() as i64),
        "fixture must reproduce a skewed local clock"
    );
    let epoch = app.delegation.epoch;
    drop(app.delegation_message(epoch, Event::Stream(ConnectionEvent::Clock(clock))));
    scan(&mut app, Some(event.clone()));
    assert_eq!(outcome(&app, 1, 0), Some(Outcome::Queued));
    deliver(&mut app, &mut frames, |app| {
        outcome(app, 1, 0) == Some(Outcome::Submitted)
    });
    assert_eq!(wait_lines(&agent, 1), [event.terminal_text()]);
    let mut expired = lifecycle(2, "amux");
    expired.created_at = app.delegation.now_ms() - delegation::MAX_EVENT_AGE_MS - 1000;
    scan(&mut app, Some(expired));
    let expired = app
        .delegation
        .queue
        .as_ref()
        .unwrap()
        .journal
        .receipts
        .iter()
        .find(|receipt| receipt.event.id == 2)
        .unwrap();
    assert_eq!(expired.outcome, Outcome::Skipped);
    assert!(expired.reason.contains("expired"));
    never_verifies(&mut app, &mut frames);
    assert_eq!(agent.lines(), [event.terminal_text()]);
}
