//! Provisional native terminal/UI candidate. Agent work is never restarted implicitly.
use amux::delivery::{DeliveryCause, DeliveryLedger, DeliveryState};
use amux::gateway_preferences::GatewayPreferences;
use amux::identity::Provider;
use amux::notifications::{Notifier, Outcome};
use amux::readiness::ReadinessGate;
use amux::recipient::TerminalRecipient;
use amux::session::{Layout, SavedTab, SavedWorkspace, Snapshot};
use amux::submission::{Submission, submit_literal};
#[path = "desktop_delegation.rs"]
mod desktop_delegation;
#[path = "desktop_design.rs"]
mod desktop_design;
#[path = "desktop_recovery.rs"]
mod desktop_recovery;
#[path = "desktop_workspace.rs"]
mod desktop_workspace;
use desktop_workspace::{WorkspaceDialog, WorkspaceMessage};
use iced::widget::{
    button, checkbox, column, container, pane_grid, row, scrollable, svg, text, text_editor,
    text_input, tooltip,
};
use iced::{Element, Length, Subscription, Task, Theme};
use iced_term::{Terminal, TerminalView};
use std::collections::HashSet;
use std::path::PathBuf;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use uuid::Uuid;

#[derive(Debug, Clone)]
enum Message {
    Design(desktop_design::Action),
    Escape,
    WindowResized(iced::window::Id, iced::Size),
    NewWorkspace,
    SelectWorkspace(usize),
    CloseWorkspace(Uuid),
    RemoveWorkspace(Uuid),
    CancelCloseWorkspace,
    Workspace(WorkspaceMessage),
    NewTab(Option<Provider>),
    SelectTab(pane_grid::Pane, usize),
    Split(pane_grid::Axis),
    Focus(pane_grid::Pane),
    CloseTab(pane_grid::Pane, u64),
    Preferences(bool),
    PreferencesTab(PreferencesTab),
    WindowReady,
    Preference(PreferenceField, Secret),
    SavePreferences,
    AutomaticNotifications(bool),
    AutoResume(bool),
    ExitNotifications(bool),
    Delegation(u64, desktop_delegation::Event),
    Updates(bool),
    CheckUpdates,
    UpdatesChecked(Result<amux::updates::UpdateCheck, String>),
    InstallUpdate,
    UpdateInstalled(Result<String, String>),
    RestartUpdated,
    Resize(pane_grid::ResizeEvent),
    Terminal(iced_term::Event),
    #[cfg(test)]
    GatewayProject(String),
    ProjectResolved(QueryScope, Result<String, String>),
    RefreshTasks,
    TasksLoaded(QueryScope, Result<serde_json::Value, String>),
    RecoveryLoaded(RecoveryScope, Result<desktop_recovery::Recovery, String>),
    ReceiptReported(RecoveryScope, Uuid, Result<(), String>),
    TaskTitle(String),
    TaskContext(text_editor::Action),
    SubmitTask,
    NewTaskRequest,
    TaskCreated(QueryScope, Uuid, Result<Uuid, String>),
    #[cfg(test)]
    LocalNotifications(bool),
    #[cfg(test)]
    Consent(u64, bool),
    DeliveryTick,
    Save,
    Close(iced::window::Id),
}

#[derive(Debug, Clone, Copy)]
enum PreferenceField {
    Url,
    ApiKey,
}
#[derive(Clone)]
struct Secret(String);
impl std::fmt::Debug for Secret {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("[redacted]")
    }
}
struct Preferences {
    current: GatewayPreferences,
    draft: GatewayPreferences,
    path: Option<PathBuf>,
    open: bool,
    tab: PreferencesTab,
    status: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum PreferencesTab {
    Gateway,
    General,
    Updates,
}

struct Updates {
    running: Option<std::sync::Arc<amux::updates::Running>>,
    installing: bool,
    install_status: String,
    checking: bool,
    checked_at: Option<Instant>,
    restart_checked_at: Option<Instant>,
    report: Option<amux::updates::UpdateCheck>,
    restart: amux::updates::Restart,
    error: String,
}
impl Updates {
    fn new() -> Self {
        let running = amux::updates::Running::capture();
        Self {
            error: running
                .as_ref()
                .err()
                .map(ToString::to_string)
                .unwrap_or_default(),
            running: running.ok().map(std::sync::Arc::new),
            installing: false,
            install_status: String::new(),
            checking: false,
            checked_at: None,
            restart_checked_at: None,
            report: None,
            restart: amux::updates::Restart {
                required: false,
                path: None,
            },
        }
    }
    fn refresh_restart(&mut self) {
        if let Some(running) = &self.running {
            self.restart = amux::updates::restart_status(running);
        }
        if let Some(report) = &mut self.report {
            report.restart = self.restart.clone();
        }
        self.restart_checked_at = Some(Instant::now());
    }

    fn attention(&self) -> bool {
        self.restart.required || self.report.as_ref().is_some_and(|r| r.available.is_some())
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct QueryScope {
    workspace: usize,
    workspace_id: Uuid,
    directory: PathBuf,
    project: String,
    revision: u64,
}

#[derive(Debug, Clone)]
struct RecoveryScope {
    query: QueryScope,
    terminal: u64,
    epoch: u64,
    actor: amux::identity::AgentIdentity,
}

#[derive(Default)]
struct RecoveryBridge {
    epoch: u64,
    actor: Option<amux::identity::AgentIdentity>,
    flight: Option<iced::task::Handle>,
    queried_at: Option<Instant>,
    snapshot: Option<desktop_recovery::Recovery>,
    managed: HashSet<Uuid>,
    blocked_receipts: HashSet<Uuid>,
}

impl RecoveryBridge {
    fn reset(&mut self) {
        self.epoch += 1;
        self.flight.take(); // Abort on drop; uncertain durable requests remain fenced.
        self.actor = None;
        self.queried_at = None;
        self.snapshot = None;
        self.blocked_receipts.clear();
    }

    fn fresh(&self) -> bool {
        self.snapshot
            .as_ref()
            .is_some_and(|snapshot| snapshot.queried_at.elapsed() <= Duration::from_secs(15))
    }
}

#[derive(Default)]
struct TaskPanel {
    project: String,
    revision: u64,
    loading: bool,
    refresh_pending: bool,
    tasks: Vec<serde_json::Value>,
    status: String,
    loaded_at: Option<Instant>,
    observed_at_ms: u64,
    queried_at: Option<Instant>,
    draft_title: String,
    draft_context: text_editor::Content,
    draft_status: String,
    request: Option<TaskRequest>,
}

struct TaskRequest {
    id: Uuid,
    project: String,
    title: String,
    state: TaskRequestState,
}

enum TaskRequestState {
    Sending,
    Accepted(Uuid),
    Unconfirmed(String),
}

#[derive(Default)]
struct Reminders {
    consent: bool,
    gate: ReadinessGate,
    status: String,
    recovery: RecoveryBridge,
}

struct Tab {
    id: u64,
    title: String,
    // Fields drop in declaration order: request owned-job retirement before the PTY drops.
    #[cfg(any(target_os = "linux", windows))]
    lifetime: Option<amux::terminal_lifetime::TerminalLifetime>,
    terminal: Option<Terminal>,
    stopped_output: Option<String>,
    provider: Option<Provider>,
    recipient: Option<TerminalRecipient>,
    reminders: Reminders,
    delegation: desktop_delegation::TabState,
}

impl Drop for Tab {
    fn drop(&mut self) {
        // Cancel transport before retiring the process/PTY fields below.
        self.reminders.recovery.flight.take();
    }
}
struct Pane {
    tabs: Vec<Tab>,
    active: usize,
}
struct Workspace {
    name: Option<String>,
    id: Uuid,
    init_script: Option<PathBuf>,
    directory: PathBuf,
    panes: pane_grid::State<Pane>,
    focused: pane_grid::Pane,
    gateway: TaskPanel,
}
struct Desktop {
    design: desktop_design::State,
    main_window: iced::window::Id,
    preferences: Preferences,
    updates: Updates,
    workspaces: Vec<Workspace>,
    active: usize,
    workspace_dialog: WorkspaceDialog,
    closing_workspace: Option<Uuid>,
    next_id: u64,
    status: String,
    ledger: Option<DeliveryLedger>,
    registry: Option<PathBuf>,
    snapshot_path: Option<PathBuf>,
    dirty: bool,
    restarting: bool,
    notify_exits: bool,
    notifier: Option<Notifier>,
    notification_status: String,
    delegation: desktop_delegation::Bridge,
}

pub fn run(directory: PathBuf) -> anyhow::Result<()> {
    let directory = amux::workspace_launch::external_path(directory.canonicalize()?);
    iced::daemon(
        move || Desktop::new(directory.clone()),
        Desktop::update,
        Desktop::window_view,
    )
    .title(Desktop::window_title)
    .theme(|_: &Desktop, _| desktop_design::theme())
    .settings(iced::Settings {
        default_text_size: iced::Pixels(13.0),
        ..Default::default()
    })
    .subscription(Desktop::subscription)
    .run()?;
    Ok(())
}

impl Desktop {
    fn new(directory: PathBuf) -> (Self, Task<Message>) {
        let (main_window, open) = iced::window::open(iced::window::Settings {
            size: iced::Size::new(1600.0, 1000.0),
            min_size: Some(iced::Size::new(1100.0, 700.0)),
            exit_on_close_request: false,
            ..Default::default()
        });
        let state = amux::identity::state_directory();
        let ledger = state
            .as_ref()
            .ok()
            .map(|path| DeliveryLedger::open(&path.join("deliveries.json")))
            .transpose();
        let delivery_error = state
            .as_ref()
            .err()
            .map(ToString::to_string)
            .or_else(|| ledger.as_ref().err().map(ToString::to_string));
        let preferences_path = state
            .as_ref()
            .ok()
            .map(|p| p.join("gateway-preferences.json"));
        let loaded = preferences_path
            .as_deref()
            .map(GatewayPreferences::load)
            .transpose();
        let (preferences, preferences_error) = match loaded {
            Ok(saved) => (
                saved
                    .flatten()
                    .unwrap_or_else(GatewayPreferences::from_environment),
                String::new(),
            ),
            Err(error) => (
                GatewayPreferences::default(),
                format!("{error}. Save corrected preferences to replace them."),
            ),
        };
        let mut app = Self {
            design: desktop_design::State::default(),
            main_window,
            updates: Updates::new(),
            preferences: Preferences { draft: preferences.clone(), current: preferences, path: preferences_path, open: false, tab: PreferencesTab::Gateway, status: preferences_error },
            workspaces: Vec::new(), active: 0, workspace_dialog: WorkspaceDialog::default(), closing_workspace: None, next_id: 0,
            status: "Configure the gateway in Preferences to receive task notifications in agent terminals.".into(),
            ledger: ledger.ok().flatten(),
            registry: state.ok(),
            snapshot_path: amux::identity::state_directory().ok().map(|p| p.join("layout.json")), dirty: false, restarting: false,
            notify_exits: false, notifier: None, notification_status: String::new(),
            delegation: desktop_delegation::Bridge::default(),
        };
        app.notify_exits = app.preferences.current.notify_exits;
        if app.notify_exits {
            app.notifier = Some(Notifier::native());
        }
        match app.snapshot_path.as_deref().map(Snapshot::load).transpose() {
            Ok(Some(Some(saved))) => {
                app.active = saved.active;
                for workspace in saved.workspaces {
                    let config = app.restore_layout(workspace.layout, &workspace.directory);
                    let panes = pane_grid::State::with_configuration(config);
                    let focused = *panes.iter().next().expect("validated nonempty layout").0;
                    app.workspaces.push(Workspace {
                        name: workspace.name,
                        id: Uuid::new_v4(),
                        directory: amux::workspace_launch::external_path(workspace.directory),
                        init_script: workspace
                            .init_script
                            .map(amux::workspace_launch::external_path),
                        panes,
                        focused,
                        gateway: TaskPanel::default(),
                    });
                }
                if !app.status.starts_with("Could not restore") {
                    app.status = "Previous terminals reopened; agent tabs use their native conversation resume.".into();
                }
            }
            Err(error) => {
                app.status = format!(
                    "Saved layout could not be loaded: {error}. Original preserved; saving disabled for this launch."
                );
                app.snapshot_path = None;
            }
            _ => {}
        }
        let home = amux::workspace_launch::home_directory().unwrap_or_else(|_| directory.clone());
        // Respect saved removals: Default is a fallback, not a workspace that
        // reappears every time a nonempty layout is restored.
        if app.workspaces.is_empty() {
            app.add_workspace(home.clone());
        }
        if directory != home && !app.workspaces.iter().any(|w| w.directory == directory) {
            app.add_workspace(directory.clone());
        }
        if let Some(error) = delivery_error {
            app.status =
                format!("Reminder storage unavailable: {error}. Local terminals remain available.");
        }
        let focus = app.focus();
        let updates = app.check_updates();
        let gateways = if !app.preferences.current.url.is_empty() {
            Task::batch(
                (0..app.workspaces.len())
                    .map(|i| app.query(i))
                    .collect::<Vec<_>>(),
            )
        } else {
            Task::none()
        };
        (
            app,
            Task::batch([open.map(|_| Message::WindowReady), focus, updates, gateways]),
        )
    }

    fn restore_layout(
        &mut self,
        layout: Layout,
        directory: &std::path::Path,
    ) -> pane_grid::Configuration<Pane> {
        match layout {
            Layout::Pane { tabs, active } => pane_grid::Configuration::Pane(Pane {
                tabs: tabs
                    .into_iter()
                    .map(|tab| {
                        self.next_id = self.next_id.max(tab.id + 1);
                        if tab.running
                            && (tab.provider.is_none() || self.preferences.current.auto_resume)
                        {
                            let cwd = tab.directory.as_deref().unwrap_or(directory);
                            let mut reopened = self.open_terminal(
                                tab.id,
                                cwd,
                                tab.provider,
                                None,
                                true,
                                tab.terminal_number,
                            );
                            if reopened.terminal.is_some() {
                                reopened.title = tab.title;
                            } else {
                                self.status =
                                    format!("Could not restore {}: {}", tab.title, reopened.title);
                            }
                            return reopened;
                        }
                        Tab {
                            id: tab.id,
                            title: tab.title,
                            provider: tab.provider,
                            #[cfg(any(target_os = "linux", windows))]
                            lifetime: None,
                            terminal: None,
                            stopped_output: None,
                            recipient: None,
                            reminders: Reminders::default(),
                            delegation: {
                                let mut state = desktop_delegation::TabState::default();
                                state.terminal_number = tab.terminal_number.unwrap_or_default();
                                state
                            },
                        }
                    })
                    .collect(),
                active,
            }),
            Layout::Split {
                vertical,
                ratio,
                a,
                b,
            } => pane_grid::Configuration::Split {
                axis: if vertical {
                    pane_grid::Axis::Vertical
                } else {
                    pane_grid::Axis::Horizontal
                },
                ratio,
                a: Box::new(self.restore_layout(*a, directory)),
                b: Box::new(self.restore_layout(*b, directory)),
            },
        }
    }

    fn save(&mut self) -> bool {
        if !self.dirty {
            return true;
        }
        fn layout(node: &pane_grid::Node, panes: &pane_grid::State<Pane>) -> Layout {
            match node {
                pane_grid::Node::Pane(id) => {
                    let pane = &panes.panes[id];
                    Layout::Pane {
                        active: pane.active,
                        tabs: pane
                            .tabs
                            .iter()
                            .map(|t| SavedTab {
                                id: t.id,
                                terminal_number: Some(t.delegation.terminal_number)
                                    .filter(|n| *n > 0),
                                title: t.title.clone(),
                                provider: t.provider.or(t.delegation.provider),
                                running: t.terminal.is_some(),
                                directory: t
                                    .terminal
                                    .as_ref()
                                    .and_then(|terminal| {
                                        amux::session::working_directory(terminal.child_pid()?)
                                    })
                                    .or_else(|| t.delegation.directory.clone()),
                            })
                            .collect(),
                    }
                }
                pane_grid::Node::Split {
                    axis, ratio, a, b, ..
                } => Layout::Split {
                    vertical: *axis == pane_grid::Axis::Vertical,
                    ratio: *ratio,
                    a: Box::new(layout(a, panes)),
                    b: Box::new(layout(b, panes)),
                },
            }
        }
        let Some(path) = &self.snapshot_path else {
            return true;
        };
        let snapshot = Snapshot {
            version: 1,
            active: self.active,
            workspaces: self
                .workspaces
                .iter()
                .map(|w| SavedWorkspace {
                    name: w.name.clone(),
                    directory: w.directory.clone(),
                    init_script: w.init_script.clone(),
                    layout: layout(w.panes.layout(), &w.panes),
                })
                .collect(),
        };
        match snapshot.save(path) {
            Ok(()) => {
                self.dirty = false;
                true
            }
            Err(error) => {
                self.status = format!("Layout save failed: {error}");
                false
            }
        }
    }

    fn terminal_with_script(
        &mut self,
        directory: &std::path::Path,
        provider: Option<Provider>,
        script: Option<&std::path::Path>,
    ) -> Tab {
        let id = self.next_id;
        self.next_id += 1;
        self.open_terminal(id, directory, provider, script, false, None)
    }

    fn open_terminal(
        &mut self,
        id: u64,
        directory: &std::path::Path,
        provider: Option<Provider>,
        script: Option<&std::path::Path>,
        resume: bool,
        assigned_number: Option<u64>,
    ) -> Tab {
        let mut backend = iced_term::settings::BackendSettings {
            working_directory: Some(directory.into()),
            ..Default::default()
        };
        let terminal_number = if let Some(number) = assigned_number.filter(|n| *n > 0) {
            number
        } else {
            match self
                .registry
                .as_deref()
                .map(amux::identity::next_terminal_number_at)
                .transpose()
            {
                Ok(Some(number)) => number,
                Ok(None) => 0,
                Err(error) => {
                    self.status = format!("Terminal identity unavailable: {error}");
                    // No agent identity is published without a reserved number.
                    0
                }
            }
        };
        backend
            .env
            .insert("AMUX_TERMINAL_NUMBER".into(), terminal_number.to_string());
        if let Ok(exe) = std::env::current_exe().and_then(|exe| amux::cli_path::cli_beside(&exe)) {
            backend
                .env
                .insert("AMUX_CLI_PATH".into(), exe.display().to_string());
            if let Some(parent) = exe.parent() {
                let mut paths = vec![parent.to_path_buf()];
                paths.extend(std::env::split_paths(
                    &std::env::var_os("PATH").unwrap_or_default(),
                ));
                if let Ok(path) = std::env::join_paths(paths) {
                    backend
                        .env
                        .insert("PATH".into(), path.to_string_lossy().into_owned());
                }
            }
        }
        // Describe this terminal, independently of the process that launched amux.
        backend.env.insert("TERM".into(), "xterm-256color".into());
        backend.env.insert("COLORTERM".into(), "truecolor".into());
        backend.env.insert("TERM_PROGRAM".into(), "amux".into());
        // A shell is not registered as an agent. Explicit launches go through the
        // same wrapper as the CLI, which allocates a distinct execution identity.
        if let Some(provider) = provider {
            match std::env::current_exe().and_then(|exe| amux::cli_path::cli_beside(&exe)) {
                Ok(exe) => {
                    backend.program = exe.display().to_string();
                    backend.args = vec![
                        "--project-dir".into(),
                        directory.display().to_string(),
                        "agent".into(),
                        provider.command().into(),
                    ];
                    if resume {
                        backend.args.push("--".into());
                        backend.args.extend(
                            amux::session::resume_arguments(provider)
                                .iter()
                                .map(|s| (*s).into()),
                        );
                    }
                }
                Err(e) => {
                    return Tab {
                        id,
                        title: format!("Launch failed: {e}"),
                        #[cfg(any(target_os = "linux", windows))]
                        lifetime: None,
                        terminal: None,
                        stopped_output: None,
                        provider: Some(provider),
                        recipient: None,
                        reminders: Reminders::default(),
                        delegation: desktop_delegation::TabState::default(),
                    };
                }
            }
        } else {
            #[cfg(windows)]
            {
                backend.program = std::env::var("COMSPEC").unwrap_or_else(|_| "cmd.exe".into());
            }
            #[cfg(not(windows))]
            {
                backend.program = std::env::var("SHELL").unwrap_or_else(|_| "/bin/sh".into());
            }
        }
        if provider.is_none()
            && let Some(script) = script
        {
            match amux::workspace_launch::script_command(script) {
                Ok((program, args)) => {
                    backend.program = program;
                    backend.args = args;
                }
                Err(error) => {
                    // Creation validates scripts before getting here. A later explicit
                    // terminal launch can find a removed script; show the error instead.
                    self.status = format!("Initialization script unavailable: {error}");
                    return Tab {
                        id,
                        title: self.status.clone(),
                        #[cfg(any(target_os = "linux", windows))]
                        lifetime: None,
                        terminal: None,
                        stopped_output: None,
                        provider: None,
                        recipient: None,
                        reminders: Reminders::default(),
                        delegation: desktop_delegation::TabState::default(),
                    };
                }
            }
        }
        // Do not let shells or another agent launch inherit a previous agent identity.
        for name in [
            "AMUX_AGENT_SESSION_ID",
            "AMUX_AGENT_INSTANCE_ID",
            "AMUX_AGENT_PROVIDER",
            "AMUX_AGENT_OS",
            "AMUX_AGENT_PROJECT",
            "AMUX_AGENT_PROJECT_ID",
            "AMUX_AGENT_ID",
            "AMUX_AGENT_NAME",
        ] {
            backend.env.insert(name.into(), String::new());
        }
        let title = provider
            .map(Provider::command)
            .unwrap_or("Shell")
            .to_owned();
        match Terminal::new(
            id,
            iced_term::settings::Settings {
                backend,
                theme: iced_term::settings::ThemeSettings::new(Box::new(iced_term::ColorPalette {
                    background: "#070a0f".into(),
                    foreground: "#cbd5e1".into(),
                    ..Default::default()
                })),
                ..Default::default()
            },
        ) {
            Ok(mut terminal) => {
                terminal.handle(iced_term::Command::AddBindings(vec![(
                    iced_term::bindings::Binding {
                        target: iced_term::bindings::InputKind::Char(",".into()),
                        modifiers: iced::keyboard::Modifiers::CTRL,
                        terminal_mode_include: iced_term::TermMode::empty(),
                        terminal_mode_exclude: iced_term::TermMode::empty(),
                    },
                    iced_term::bindings::BindingAction::Suppress,
                )]));
                #[cfg(any(target_os = "linux", windows))]
                let lifetime = terminal.child_pid().and_then(|pid| {
                    match amux::terminal_lifetime::TerminalLifetime::capture(pid) {
                        Ok(lifetime) => Some(lifetime),
                        Err(error) => {
                            self.status =
                                format!("Terminal cleanup ownership unavailable: {error}");
                            None
                        }
                    }
                });
                let recipient = provider.and_then(|provider| {
                    TerminalRecipient::new(terminal.child_pid()?, provider, directory).ok()
                });
                Tab {
                    id,
                    title,
                    #[cfg(any(target_os = "linux", windows))]
                    lifetime,
                    delegation: desktop_delegation::TabState::capture(
                        &terminal,
                        terminal_number,
                        directory,
                    ),
                    terminal: Some(terminal),
                    stopped_output: None,
                    provider,
                    recipient,
                    reminders: Reminders::default(),
                }
            }
            Err(error) => {
                self.status = format!("Terminal launch failed: {error}");
                Tab {
                    id,
                    title: format!("{title} — launch failed"),
                    #[cfg(any(target_os = "linux", windows))]
                    lifetime: None,
                    terminal: None,
                    stopped_output: None,
                    provider,
                    recipient: None,
                    reminders: Reminders::default(),
                    delegation: desktop_delegation::TabState::default(),
                }
            }
        }
    }

    fn add_workspace(&mut self, directory: PathBuf) {
        self.add_workspace_with_script(directory, None);
    }

    fn add_workspace_with_script(&mut self, directory: PathBuf, init_script: Option<PathBuf>) {
        self.show_terminals();
        let tab = self.terminal_with_script(&directory, None, init_script.as_deref());
        let (panes, focused) = pane_grid::State::new(Pane {
            tabs: vec![tab],
            active: 0,
        });
        self.workspaces.push(Workspace {
            name: None,
            id: Uuid::new_v4(),
            directory,
            init_script,
            panes,
            focused,
            gateway: TaskPanel::default(),
        });
        self.active = self.workspaces.len() - 1;
        self.dirty = true;
    }

    fn focus(&self) -> Task<Message> {
        if self.preferences.open || self.workspace_dialog.open {
            return Task::none();
        }
        let workspace = &self.workspaces[self.active];
        workspace
            .panes
            .get(workspace.focused)
            .and_then(|pane| pane.tabs.get(pane.active))
            .and_then(|tab| tab.terminal.as_ref())
            .map(|terminal| TerminalView::focus(terminal.widget_id().clone()))
            .unwrap_or_else(Task::none)
    }

    fn update(&mut self, message: Message) -> Task<Message> {
        if self.restarting {
            return Task::none();
        }
        let Some(message) = self.retarget_workspace_message(message) else {
            return Task::none();
        };
        if matches!(
            &message,
            Message::NewWorkspace
                | Message::SelectWorkspace(_)
                | Message::NewTab(_)
                | Message::SelectTab(..)
                | Message::Split(_)
                | Message::CloseTab(..)
                | Message::Resize(_)
        ) {
            self.dirty = true;
        }
        match message {
            Message::WindowResized(id, size) => {
                if id == self.main_window {
                    self.set_compact(size.width < 1400.0);
                }
            }
            Message::Escape => {
                if self.workspace_dialog.open {
                    return self.close_workspace_dialog();
                }
                if self.preferences.open {
                    return self.close_preferences();
                }
                self.close_design_popovers();
            }
            Message::Design(value) => return self.design_message(value),
            Message::WindowReady => {}
            Message::AutoResume(value) => self.preferences.draft.auto_resume = value,
            Message::ExitNotifications(value) => self.preferences.draft.notify_exits = value,
            Message::AutomaticNotifications(value) => {
                self.preferences.draft.automatic_notifications = value
            }
            Message::Delegation(epoch, event) => return self.delegation_message(epoch, event),
            Message::Save => {
                self.save();
            }
            Message::Close(window) => {
                return self.close_window(window);
            }
            Message::Workspace(message) => return self.workspace_message(message),
            #[cfg(test)]
            Message::LocalNotifications(enabled) => {
                self.notify_exits = enabled;
                if enabled && self.notifier.is_none() {
                    self.notifier = Some(Notifier::native());
                }
                self.notification_status = if enabled {
                    "Desktop notifications enabled for future process exits".into()
                } else {
                    "Desktop notifications disabled for future process exits".into()
                };
            }
            Message::NewWorkspace => return self.open_workspace_dialog(),
            Message::CloseWorkspace(id) => return self.request_close_workspace(id),
            Message::RemoveWorkspace(id) => return self.remove_workspace(id),
            Message::CancelCloseWorkspace => self.closing_workspace = None,
            Message::SelectWorkspace(index) => {
                if index < self.workspaces.len() {
                    self.show_terminals();
                    self.active = index;
                    return self.focus();
                }
            }
            Message::NewTab(provider) => {
                self.show_terminals();
                let directory = self.workspaces[self.active].directory.clone();
                let script = self.workspaces[self.active].init_script.clone();
                let tab = self.terminal_with_script(&directory, provider, script.as_deref());
                let workspace = &mut self.workspaces[self.active];
                if let Some(pane) = workspace.panes.get_mut(workspace.focused) {
                    pane.tabs.push(tab);
                    pane.active = pane.tabs.len() - 1;
                }
                return self.focus();
            }
            Message::SelectTab(id, tab) => {
                let workspace = &mut self.workspaces[self.active];
                if let Some(pane) = workspace.panes.get_mut(id)
                    && tab < pane.tabs.len()
                {
                    pane.active = tab;
                    workspace.focused = id;
                }
                return self.focus();
            }
            Message::Focus(id) => {
                self.workspaces[self.active].focused = id;
                return self.focus();
            }
            Message::Split(axis) => {
                self.show_terminals();
                let directory = self.workspaces[self.active].directory.clone();
                let script = self.workspaces[self.active].init_script.clone();
                let tab = self.terminal_with_script(&directory, None, script.as_deref());
                let workspace = &mut self.workspaces[self.active];
                workspace.panes.restore();
                if let Some((pane, _)) = workspace.panes.split(
                    axis,
                    workspace.focused,
                    Pane {
                        tabs: vec![tab],
                        active: 0,
                    },
                ) {
                    workspace.focused = pane;
                }
                return self.focus();
            }
            Message::CloseTab(pane_id, terminal_id) => {
                let workspace = &self.workspaces[self.active];
                let Some(pane) = workspace.panes.get(pane_id) else {
                    return Task::none();
                };
                let Some(index) = pane.tabs.iter().position(|t| t.id == terminal_id) else {
                    return Task::none();
                };
                if pane.tabs.len() == 1 && workspace.panes.len() == 1 {
                    self.status = "Keep one terminal per workspace; exit the application to close all sessions".into();
                    return Task::none();
                }
                self.withdraw(terminal_id);
                let workspace = &mut self.workspaces[self.active];
                let pane = workspace.panes.get_mut(pane_id).expect("pane exists");
                if pane.tabs.len() > 1 {
                    pane.tabs.remove(index);
                    if index < pane.active {
                        pane.active -= 1;
                    }
                    pane.active = pane.active.min(pane.tabs.len() - 1);
                } else if let Some((_, sibling)) = workspace.panes.close(pane_id)
                    && workspace.focused == pane_id
                {
                    workspace.focused = sibling;
                }
                return self.focus();
            }
            Message::Preferences(open) => {
                return if open {
                    self.open_preferences(PreferencesTab::Gateway)
                } else {
                    self.close_preferences()
                };
            }
            Message::PreferencesTab(tab) => self.preferences.tab = tab,
            Message::Preference(field, Secret(value)) => {
                if value.len() <= 16 * 1024 {
                    let draft = &mut self.preferences.draft;
                    *match field {
                        PreferenceField::Url => &mut draft.url,
                        PreferenceField::ApiKey => &mut draft.api_key,
                    } = value;
                }
            }
            Message::SavePreferences => return self.save_preferences(),
            Message::Updates(open) => {
                return if open {
                    self.open_preferences(PreferencesTab::Updates)
                } else {
                    self.close_preferences()
                };
            }
            Message::CheckUpdates => return self.check_updates(),
            Message::UpdatesChecked(result) => {
                self.updates.checking = false;
                self.updates.checked_at = Some(Instant::now());
                match result {
                    Ok(report) => {
                        self.updates.restart = report.restart.clone();
                        self.updates.report = Some(report);
                        self.updates.error.clear();
                    }
                    Err(error) => {
                        self.updates.error = format!("Could not check for updates: {error}")
                    }
                }
                self.updates.refresh_restart();
            }
            Message::InstallUpdate => return self.install_update(),
            Message::UpdateInstalled(result) => {
                self.updates.installing = false;
                self.updates.refresh_restart();
                match result {
                    Ok(summary) => {
                        self.updates.install_status = summary;
                        return self.check_updates();
                    }
                    Err(error) => self.updates.install_status = format!("Update failed: {error}"),
                }
            }
            Message::RestartUpdated => return self.restart_updated(),
            Message::Resize(event) => self.workspaces[self.active]
                .panes
                .resize(event.split, event.ratio.clamp(0.05, 0.95)),
            Message::Terminal(iced_term::Event::BackendCall(id, command)) => {
                // Backend events carry exact terminal IDs, including hidden workspaces.
                // Never close the focused pane in response to a different process exiting.
                for workspace in &mut self.workspaces {
                    for (_, pane) in workspace.panes.iter_mut() {
                        if let Some(tab) = pane.tabs.iter_mut().find(|tab| tab.id == id) {
                            tab.delegation.note_command(&command);
                            if matches!(
                                &command,
                                iced_term::BackendCommand::Write(_)
                                    | iced_term::BackendCommand::MouseReport(..)
                            ) && let Some(recipient) = &mut tab.recipient
                            {
                                recipient.note_human_input();
                            }
                            if let Some(terminal) = &mut tab.terminal {
                                match terminal.handle(iced_term::Command::ProxyToBackend(command)) {
                                    iced_term::actions::Action::ChangeTitle(title) => {
                                        tab.title = title
                                    }
                                    iced_term::actions::Action::Shutdown => {
                                        self.dirty = true;
                                        tab.reminders.recovery.reset();
                                        // Preserve only the bounded visible text, then release
                                        // the PTY and its subscription. This is never input-ready.
                                        tab.stopped_output = Some(terminal.screen_text());
                                        if self.notify_exits
                                            && let Some(notifier) = &mut self.notifier
                                        {
                                            self.notification_status = match notifier.notify(
                                                "amux · terminal process exited",
                                                &format!("{} · terminal #{id}", tab.title),
                                            ) {
                                                Ok(_) => "Desktop notification queued".into(),
                                                Err(error) => format!(
                                                    "Desktop notification not queued: {error:?}"
                                                ),
                                            };
                                        }
                                        #[cfg(any(target_os = "linux", windows))]
                                        {
                                            tab.lifetime.take();
                                        }
                                        tab.terminal = None;
                                        tab.recipient = None;
                                        tab.reminders.consent = false;
                                        if let Some(ledger) = &mut self.ledger
                                            && let Err(error) = ledger.retire_terminal(id)
                                        {
                                            self.status =
                                                format!("Reminder retirement failed: {error}");
                                        }
                                        tab.title.push_str(" · exited");
                                    }
                                    _ => {}
                                }
                            }
                            if let Some(terminal) = &tab.terminal {
                                tab.reminders.gate.note_terminal_output(terminal);
                            }
                            return Task::none();
                        }
                    }
                }
            }
            #[cfg(test)]
            Message::GatewayProject(project) => self.bind_project(self.active, project),
            Message::ProjectResolved(scope, result) => {
                if !self.scope_current(&scope) {
                    return Task::none();
                }
                self.workspaces[scope.workspace].gateway.loading = false;
                match result {
                    Ok(project) => {
                        self.bind_project(scope.workspace, project);
                        return self.query(scope.workspace);
                    }
                    Err(error) => {
                        self.workspaces[scope.workspace].gateway.status =
                            format!("Could not connect this repository: {error}");
                    }
                }
            }
            Message::RefreshTasks => return self.query(self.active),
            Message::TaskTitle(title) => {
                let panel = &mut self.workspaces[self.active].gateway;
                if panel.request.is_none() {
                    if title.len() <= 1024 {
                        panel.draft_title = title;
                        panel.draft_status.clear();
                    } else {
                        panel.draft_status = "Task title is too long".into();
                    }
                }
            }
            Message::TaskContext(action) => {
                let panel = &mut self.workspaces[self.active].gateway;
                if panel.request.is_none() {
                    let previous = panel.draft_context.text();
                    panel.draft_context.perform(action);
                    if panel.draft_context.text().len() > 64 * 1024 {
                        panel.draft_context = text_editor::Content::with_text(&previous);
                        panel.draft_status = "Task context is too long".into();
                    } else {
                        panel.draft_status.clear();
                    }
                }
            }
            Message::SubmitTask => return self.create_task(),
            Message::NewTaskRequest => {
                let panel = &mut self.workspaces[self.active].gateway;
                if panel
                    .request
                    .as_ref()
                    .is_some_and(|request| !matches!(request.state, TaskRequestState::Sending))
                {
                    // Explicitly start a different request, never retry an uncertain POST.
                    panel.request = None;
                    panel.draft_title.clear();
                    panel.draft_context = text_editor::Content::default();
                    panel.draft_status.clear();
                }
            }
            Message::TaskCreated(scope, request_id, result) => {
                let current = self.scope_current(&scope);
                let Some(workspace) = self.workspaces.get_mut(scope.workspace) else {
                    return Task::none();
                };
                if workspace.directory != scope.directory {
                    return Task::none();
                }
                let panel = &mut workspace.gateway;
                let Some(request) = &mut panel.request else {
                    return Task::none();
                };
                if request.id != request_id
                    || request.project != scope.project
                    || !matches!(request.state, TaskRequestState::Sending)
                {
                    return Task::none();
                }
                let accepted = result.is_ok();
                request.state = match result {
                    Ok(task) => TaskRequestState::Accepted(task),
                    Err(error) => TaskRequestState::Unconfirmed(error),
                };
                // A project rebind must not discard the original request's receipt.
                // Refresh only if this workspace still shows the submitted project.
                if accepted && current && panel.project.trim() == scope.project {
                    return self.query(scope.workspace);
                }
            }
            Message::TasksLoaded(scope, result) => {
                if !self.scope_current(&scope) {
                    return Task::none();
                }
                let panel = &mut self.workspaces[scope.workspace].gateway;
                panel.loading = false;
                panel.loaded_at = None;
                match result {
                    Ok(serde_json::Value::Array(tasks)) => {
                        panel.tasks = tasks;
                        panel.loaded_at = Some(Instant::now());
                        panel.observed_at_ms = unix_ms();
                        panel.status = format!("{} gateway tasks", panel.tasks.len());
                    }
                    Ok(_) => {
                        panel.tasks.clear();
                        panel.status = "Unexpected gateway task-list shape".into();
                    }
                    Err(error) => {
                        panel.tasks.clear();
                        panel.status =
                            format!("Gateway unavailable: {error}. Queued delivery paused.");
                    }
                }
                if std::mem::take(&mut panel.refresh_pending) {
                    return self.query(scope.workspace);
                }
            }
            Message::RecoveryLoaded(scope, result) => {
                let current = self.scope_current(&scope.query);
                let registry = self.registry.clone();
                if let Some(workspace) = self.workspaces.get_mut(scope.query.workspace) {
                    for (_, pane) in workspace.panes.iter_mut() {
                        if let Some(tab) = pane.tabs.iter_mut().find(|tab| tab.id == scope.terminal)
                        {
                            if tab.reminders.recovery.epoch != scope.epoch {
                                return Task::none();
                            }
                            tab.reminders.recovery.flight.take();
                            let actor_matches = registry.as_deref().is_some_and(|registry| {
                                tab.recipient.as_mut().is_some_and(|recipient| {
                                    recipient
                                        .resolve(registry)
                                        .is_ok_and(|actor| actor == &scope.actor)
                                })
                            });
                            if !current || !tab.reminders.consent || !actor_matches {
                                return Task::none();
                            }
                            match result {
                                Ok(snapshot)
                                    if snapshot.queried_at.elapsed() <= Duration::from_secs(15) =>
                                {
                                    tab.reminders.recovery.blocked_receipts.retain(|task| {
                                        snapshot
                                            .awaiting_receipt
                                            .iter()
                                            .any(|work| work.task == *task)
                                    });
                                    for work in
                                        snapshot.ready.iter().chain(&snapshot.awaiting_receipt)
                                    {
                                        tab.reminders.recovery.managed.insert(work.task);
                                    }
                                    // Keep only current work and already queued deliveries.
                                    tab.reminders.recovery.managed.retain(|task| {
                                        snapshot
                                            .ready
                                            .iter()
                                            .chain(&snapshot.awaiting_receipt)
                                            .any(|work| work.task == *task)
                                            || self.ledger.as_ref().is_some_and(|ledger| {
                                                ledger.records().iter().any(|record| {
                                                    record.task_id == *task
                                                        && record.recipient.terminal_id()
                                                            == scope.terminal
                                                        && record.state == DeliveryState::Pending
                                                })
                                            })
                                    });
                                    tab.reminders.status = if snapshot
                                        .unresolved_receipts
                                        .is_empty()
                                    {
                                        "Gateway recovery checked; waiting for a supported empty prompt. No work was claimed or started.".into()
                                    } else {
                                        "Receipt outcome unknown; inspect gateway requests before retrying. Input stays paused for those tasks.".into()
                                    };
                                    tab.reminders.recovery.actor = Some(scope.actor);
                                    tab.reminders.recovery.snapshot = Some(snapshot);
                                }
                                Ok(_) => {
                                    tab.reminders.recovery.snapshot = None;
                                    tab.reminders.status = "Gateway recovery took too long; input paused until a fresh query.".into();
                                }
                                Err(error) => {
                                    tab.reminders.recovery.snapshot = None;
                                    tab.reminders.status =
                                        format!("Gateway recovery paused: {error}");
                                }
                            }
                            return Task::none();
                        }
                    }
                }
            }
            Message::ReceiptReported(scope, task, result) => {
                let current = self.scope_current(&scope.query);
                let registry = self.registry.clone();
                if let Some(workspace) = self.workspaces.get_mut(scope.query.workspace) {
                    for (_, pane) in workspace.panes.iter_mut() {
                        if let Some(tab) = pane.tabs.iter_mut().find(|tab| tab.id == scope.terminal)
                        {
                            if tab.reminders.recovery.epoch != scope.epoch {
                                return Task::none();
                            }
                            tab.reminders.recovery.flight.take();
                            let actor_matches = registry.as_deref().is_some_and(|registry| {
                                tab.recipient.as_mut().is_some_and(|recipient| {
                                    recipient
                                        .resolve(registry)
                                        .is_ok_and(|actor| actor == &scope.actor)
                                })
                            });
                            if !current || !tab.reminders.consent || !actor_matches {
                                return Task::none();
                            }
                            tab.reminders.recovery.snapshot = None;
                            tab.reminders.recovery.queried_at = None;
                            tab.reminders.status = match result {
                                Ok(()) => "Transport receipt reported; refreshing the task. Agent acknowledgment and execution remain unknown.".into(),
                                Err(error) => {
                                    tab.reminders.recovery.blocked_receipts.insert(task);
                                    format!("Transport receipt unconfirmed: {error}. Inspect gateway requests before retrying.")
                                }
                            };
                            return Task::none();
                        }
                    }
                }
            }
            #[cfg(test)]
            Message::Consent(id, enabled) => {
                if !enabled {
                    self.withdraw(id);
                } else if self.ledger.is_some() {
                    let mut query_workspace = None;
                    for (index, workspace) in self.workspaces.iter_mut().enumerate() {
                        for (_, pane) in workspace.panes.iter_mut() {
                            if let Some(tab) = pane.tabs.iter_mut().find(|tab| tab.id == id)
                                && let Some(recipient) = &mut tab.recipient
                            {
                                tab.reminders.consent = true;
                                tab.reminders.recovery.reset();
                                recipient.set_consent(true);
                                query_workspace = Some(index);
                                tab.reminders.status =
                                    "Waiting for supported empty prompt; select a task to queue."
                                        .into();
                            }
                        }
                    }
                    return query_workspace
                        .map(|index| self.query(index))
                        .unwrap_or_else(Task::none);
                }
            }
            Message::DeliveryTick => {
                if let Some(notifier) = &self.notifier {
                    for outcome in notifier.outcomes() {
                        self.notification_status = match outcome {
                            Outcome::Shown { .. } => {
                                "Desktop notification submitted to the notification service".into()
                            }
                            Outcome::Failed { reason, .. } => {
                                format!("Desktop notification: {reason}")
                            }
                            Outcome::TimedOut { .. } => "Desktop notification timed out; delivery outcome unknown. This attempt will not be retried.".into(),
                        };
                    }
                }
                if self
                    .updates
                    .restart_checked_at
                    .is_none_or(|t| t.elapsed() >= Duration::from_secs(5))
                {
                    self.updates.refresh_restart();
                }
                let updates = if self
                    .updates
                    .checked_at
                    .is_none_or(|t| t.elapsed() >= Duration::from_secs(3600))
                {
                    self.check_updates()
                } else {
                    Task::none()
                };
                return Task::batch([self.delivery_tick(), self.delegation_tick(), updates]);
            }
        }
        Task::none()
    }

    fn check_updates(&mut self) -> Task<Message> {
        if self.updates.checking || self.updates.installing {
            return Task::none();
        }
        self.updates.checked_at = Some(Instant::now());
        let Some(running) = self.updates.running.clone() else {
            return Task::none();
        };
        self.updates.refresh_restart();
        self.updates.checking = true;
        Task::perform(
            async move {
                amux::updates::check(&running, &amux::updates::ReleaseSource::github())
                    .await
                    .map_err(|error| error.to_string())
            },
            Message::UpdatesChecked,
        )
    }

    fn install_update(&mut self) -> Task<Message> {
        if self.updates.installing
            || self.updates.checking
            || self.updates.restart.required
            || !self.updates.report.as_ref().is_some_and(|report| {
                report.available.is_some()
                    && matches!(report.method, amux::updates::InstallMethod::Direct { .. })
                    && matches!(report.guidance, amux::updates::Guidance::CliUpdate { .. })
            })
        {
            return Task::none();
        }
        let Some(running) = self.updates.running.clone() else {
            return Task::none();
        };
        self.updates.installing = true;
        self.updates.install_status.clear();
        self.updates.error.clear();
        Task::perform(
            async move {
                amux::updates::update(&running, &amux::updates::ReleaseSource::github())
                    .await
                    .map_err(|error| error.to_string())
            },
            Message::UpdateInstalled,
        )
    }

    fn restart_updated(&mut self) -> Task<Message> {
        self.restart_with(|path, directory| {
            std::process::Command::new(path)
                .arg("--project-dir")
                .arg(directory)
                .stdin(std::process::Stdio::null())
                .stdout(std::process::Stdio::null())
                .stderr(std::process::Stdio::null())
                .spawn()?;
            Ok(())
        })
    }

    fn restart_with(
        &mut self,
        start: impl FnOnce(&std::path::Path, &std::path::Path) -> anyhow::Result<()>,
    ) -> Task<Message> {
        if self.updates.installing || self.workspace_dialog.busy {
            return Task::none();
        }
        let restart = self
            .updates
            .running
            .as_ref()
            .map(|running| amux::updates::restart_status(running));
        let Some(path) = restart.filter(|r| r.required).and_then(|r| r.path) else {
            self.updates.error = "No updated desktop is ready to restart yet.".into();
            return Task::none();
        };
        self.dirty = true;
        if !self.save() {
            self.updates.error = "Layout could not be saved; restart cancelled.".into();
            return Task::none();
        }
        let had_ledger = self.pause_for_restart();
        match start(&path, &self.workspaces[self.active].directory) {
            Ok(()) => iced::exit(),
            Err(error) => {
                self.restarting = false;
                self.updates.error = format!("Could not restart: {error}");
                if had_ledger {
                    let reopened = self
                        .registry
                        .as_ref()
                        .ok_or_else(|| anyhow::anyhow!("reminder storage path unavailable"))
                        .and_then(|p| DeliveryLedger::open(&p.join("deliveries.json")));
                    match reopened {
                        Ok(ledger) => self.ledger = Some(ledger),
                        Err(error) => self.updates.error.push_str(&format!(
                            "; reminder storage could not be reopened: {error}"
                        )),
                    }
                }
                Task::none()
            }
        }
    }

    fn pause_for_restart(&mut self) -> bool {
        let ids: Vec<_> = self
            .workspaces
            .iter()
            .flat_map(|w| w.panes.iter().flat_map(|(_, p)| &p.tabs))
            .map(|t| t.id)
            .collect();
        for id in ids {
            self.withdraw(id);
        }
        self.delegation.stop();
        let had_ledger = self.ledger.is_some();
        drop(self.ledger.take());
        self.restarting = true;
        had_ledger
    }

    fn updates_view(&self) -> Element<'_, Message> {
        let mut content = column![text("App updates").size(24)].spacing(14);
        if self.updates.installing {
            content = content
                .push(text("Downloading and installing update…").size(20))
                .push(text(
                    "You can keep using your terminals. Restart when the update is ready.",
                ));
        } else if self.updates.restart.required {
            content =
                content.push(text("An installed update is ready. Restart to use it.").size(20));
            if self.updates.restart.path.is_some() {
                let live = self
                    .workspaces
                    .iter()
                    .flat_map(|w| w.panes.iter().flat_map(|(_, p)| &p.tabs))
                    .filter(|t| t.terminal.is_some())
                    .count();
                content = content.push(text("Restart closes running processes and reopens their tabs; agent conversations use native resume.").size(13))
                    .push(button(text(format!("Restart amux · close {live} running sessions"))).on_press(Message::RestartUpdated));
            } else {
                content = content.push(text("Close and reopen amux to use the installed update."));
            }
        } else if let Some(report) = &self.updates.report {
            content = content.push(text(format!(
                "Installed: {}",
                report
                    .installed
                    .as_ref()
                    .map(|r| r.tag.as_str())
                    .unwrap_or(env!("CARGO_PKG_VERSION"))
            )));
            if let Some(release) = &report.available {
                content = content.push(text(format!("Update available: {}", release.tag)).size(20));
            } else if report.note.is_some() {
                content = content.push(text("Update status needs attention."));
            } else {
                content = content.push(text("You are up to date."));
            }
            match &report.guidance {
                amux::updates::Guidance::CliUpdate { .. } => {
                    if report.available.is_some() {
                        content = content.push(button("Update now").on_press_maybe(
                            (!self.updates.checking).then_some(Message::InstallUpdate)))
                            .push(text("Download and install without closing terminals. Restart when ready.").size(13));
                    }
                }
                amux::updates::Guidance::Homebrew { command } => {
                    content = content.push(text(format!(
                        "Managed by Homebrew. Update with {command}, then restart here."
                    )));
                }
                amux::updates::Guidance::Manual { url } => {
                    content = content.push(text(format!(
                        "Update using your installation method: {url}"
                    )));
                }
            }
            if let Some(note) = &report.note {
                content = content.push(text(note).size(13));
            }
        }
        if !self.updates.install_status.is_empty() {
            content = content.push(text(&self.updates.install_status).size(13));
        }
        if !self.updates.error.is_empty() {
            content = content.push(text(&self.updates.error));
        }
        content = content.push(
            button(if self.updates.checking {
                "Checking…"
            } else {
                "Check for updates"
            })
            .on_press_maybe(
                (!self.updates.checking && !self.updates.installing)
                    .then_some(Message::CheckUpdates),
            ),
        );
        scrollable(content)
            .width(Length::Fill)
            .height(Length::Fill)
            .into()
    }

    fn open_preferences(&mut self, tab: PreferencesTab) -> Task<Message> {
        self.preferences.tab = tab;
        if !self.preferences.open {
            self.preferences.draft = self.preferences.current.clone();
        }
        self.preferences.open = true;
        self.close_design_popovers();
        iced::widget::operation::focus("gateway-url")
    }

    fn close_preferences(&mut self) -> Task<Message> {
        self.preferences.draft = self.preferences.current.clone();
        self.preferences.open = false;
        self.focus()
    }

    fn close_window(&mut self, window: iced::window::Id) -> Task<Message> {
        if window == self.main_window {
            if self.workspace_dialog.busy {
                self.status = "Wait for the repository clone to finish before closing amux.".into();
            } else if self.updates.installing {
                self.status =
                    "An update is installing. Wait for it to finish before closing amux.".into();
            } else {
                self.dirty = true;
                if self.save() {
                    return iced::exit();
                }
            }
        }
        Task::none()
    }

    fn window_title(&self, _: iced::window::Id) -> String {
        "amux · agent workspaces".into()
    }
    fn window_view(&self, _: iced::window::Id) -> Element<'_, Message> {
        self.view()
    }

    fn save_preferences(&mut self) -> Task<Message> {
        let connection_changed = self.preferences.current.url.trim()
            != self.preferences.draft.url.trim()
            || self.preferences.current.api_key.trim() != self.preferences.draft.api_key.trim()
            || self.preferences.current.automatic_notifications
                != self.preferences.draft.automatic_notifications;
        if connection_changed
            && self.workspaces.iter().any(|w| {
                w.gateway.request.as_ref().is_some_and(|r| {
                    matches!(
                        r.state,
                        TaskRequestState::Sending | TaskRequestState::Unconfirmed(_)
                    )
                })
            })
        {
            self.preferences.status = "A task submission is pending or unconfirmed. Inspect its result before changing connections; use New task only after resolving it.".into();
            return Task::none();
        }
        let mut draft = self.preferences.draft.clone();
        draft.url = draft.url.trim().to_owned();
        draft.api_key = draft.api_key.trim().to_owned();
        draft.machine_key = draft.machine_key.trim().to_owned();
        draft.machine_id = draft.machine_id.trim().to_owned();
        let result = self
            .preferences
            .path
            .as_deref()
            .ok_or_else(|| anyhow::anyhow!("Application settings directory unavailable"))
            .and_then(|path| draft.save(path));
        if let Err(error) = result {
            self.preferences.status = format!("Not saved: {error}");
            return Task::none();
        }
        self.notify_exits = draft.notify_exits;
        if self.notify_exits && self.notifier.is_none() {
            self.notifier = Some(Notifier::native());
        }
        if !connection_changed {
            self.preferences.current = draft;
            self.preferences.status = "Preferences saved".into();
            return self.close_preferences();
        }
        let ids: Vec<_> = self
            .workspaces
            .iter()
            .flat_map(|w| w.panes.iter().flat_map(|(_, p)| &p.tabs))
            .map(|t| t.id)
            .collect();
        for id in ids {
            self.withdraw(id);
        }
        for workspace in &mut self.workspaces {
            let panel = &mut workspace.gateway;
            panel.revision += 1;
            panel.project.clear();
            panel.loading = false;
            panel.refresh_pending = false;
            panel.loaded_at = None;
            panel.queried_at = None;
            panel.tasks.clear();
            panel.status =
                "Gateway preferences changed. Refresh to connect; reminder consent withdrawn."
                    .into();
        }
        self.delegation.stop();
        self.preferences.current = draft;
        self.preferences.status = "Gateway preferences saved".into();
        self.status = "Gateway preferences saved. Connecting repositories…".into();
        let queries = Task::batch(
            (0..self.workspaces.len())
                .map(|i| self.query(i))
                .collect::<Vec<_>>(),
        );
        Task::batch([queries, self.close_preferences()])
    }

    fn preferences_view(&self) -> Element<'_, Message> {
        let draft = &self.preferences.draft;
        let field = |label, value, kind, secure| {
            column![
                text(label).size(13),
                text_input("", value)
                    .id(if matches!(kind, PreferenceField::Url) {
                        "gateway-url"
                    } else {
                        "gateway-key"
                    })
                    .secure(secure)
                    .on_input(move |value| Message::Preference(kind, Secret(value)))
            ]
            .spacing(4)
        };
        container(scrollable(column![
            text("Gateway Server").size(20),
            text("Connect with your gateway URL and API key. Projects are resolved automatically from each workspace’s Git remote."),
            field("Gateway URL", &draft.url, PreferenceField::Url, false),
            field("API key", &draft.api_key, PreferenceField::ApiKey, true),
            checkbox(draft.automatic_notifications).label("Send fresh gateway task notifications to agents in this app").on_toggle(Message::AutomaticNotifications),
            text("Notifications use assigned agent identities and required repository matching. Typed input is preserved during delivery.").size(12),
            text("Repository matching is required. Settings are saved on this device.").size(12),
            text(&self.workspaces[self.active].gateway.status).size(13),
        ].spacing(14))).width(Length::Fill).height(Length::Fill).into()
    }

    fn scope(&self, workspace: usize) -> QueryScope {
        let current = &self.workspaces[workspace];
        QueryScope {
            workspace,
            workspace_id: current.id,
            directory: current.directory.clone(),
            project: current.gateway.project.trim().to_owned(),
            revision: current.gateway.revision,
        }
    }

    fn bind_project(&mut self, workspace: usize, project: String) {
        let ids: Vec<_> = self.workspaces[workspace]
            .panes
            .iter()
            .flat_map(|(_, pane)| &pane.tabs)
            .map(|tab| tab.id)
            .collect();
        for id in ids {
            self.withdraw(id);
        }
        let panel = &mut self.workspaces[workspace].gateway;
        panel.project = project;
        panel.revision += 1;
        panel.loading = false;
        panel.refresh_pending = false;
        panel.loaded_at = None;
        panel.tasks.clear();
        panel.status = "Repository project resolved; reminder consent remains off.".into();
    }

    fn scope_current(&self, scope: &QueryScope) -> bool {
        self.workspaces.get(scope.workspace).is_some() && self.scope(scope.workspace) == *scope
    }

    fn query(&mut self, workspace: usize) -> Task<Message> {
        let scope = self.scope(workspace);
        let panel = &mut self.workspaces[workspace].gateway;
        if panel.loading {
            return Task::none();
        }
        panel.queried_at = Some(Instant::now());
        let gateway = self.preferences.current.gateway();
        match gateway {
            Ok(gateway) => {
                panel.loading = true;
                if scope.project.is_empty() {
                    panel.status = "Resolving project from repository…".into();
                    return Task::perform(
                        async move {
                            let result = gateway
                                .resolve_project(scope.directory.clone())
                                .await
                                .map_err(|e| e.to_string());
                            (scope, result)
                        },
                        |(scope, result)| Message::ProjectResolved(scope, result),
                    );
                }
                Task::perform(
                    async move {
                        let result = gateway
                            .list_all(&scope.project)
                            .await
                            .map_err(|e| e.to_string());
                        (scope, result)
                    },
                    |(scope, result)| Message::TasksLoaded(scope, result),
                )
            }
            Err(error) => {
                panel.loaded_at = None;
                panel.status = error.to_string();
                Task::none()
            }
        }
    }

    fn create_task(&mut self) -> Task<Message> {
        let scope = self.scope(self.active);
        let panel = &mut self.workspaces[self.active].gateway;
        if panel.request.is_some() {
            return Task::none();
        }
        if scope.project.is_empty() || panel.draft_title.trim().is_empty() {
            panel.draft_status =
                "Not submitted: wait for the repository connection and enter a task title".into();
            return Task::none();
        }
        if panel.draft_title.len() > 1024 || panel.draft_context.text().len() > 64 * 1024 {
            panel.draft_status = "Not submitted: task title or context is too long".into();
            return Task::none();
        }
        let gateway = self.preferences.current.gateway();
        let gateway = match gateway {
            Ok(gateway) => gateway,
            Err(error) => {
                panel.draft_status = format!("Not submitted: {error}");
                return Task::none();
            }
        };
        let request_id = Uuid::new_v4();
        let title = panel.draft_title.clone();
        let context = panel.draft_context.text();
        panel.request = Some(TaskRequest {
            id: request_id,
            project: scope.project.clone(),
            title: title.clone(),
            state: TaskRequestState::Sending,
        });
        panel.draft_status.clear();
        Task::perform(
            async move {
                // A desktop user's request is not attributed to any agent execution.
                // The shared client validates the accepted task UUID/project/status.
                let result = async {
                    let task = gateway
                        .request(
                            reqwest::Method::POST,
                            &scope.project,
                            &[],
                            Some(serde_json::json!({"title": title, "specification": context})),
                            None,
                        )
                        .await?;
                    serde_json::from_value::<Uuid>(task["id"].clone()).map_err(anyhow::Error::from)
                }
                .await
                .map_err(|error| error.to_string());
                (scope, request_id, result)
            },
            |(scope, request_id, result)| Message::TaskCreated(scope, request_id, result),
        )
    }

    fn recovery_request(&mut self, index: usize, id: u64, receive: Option<Uuid>) -> Task<Message> {
        let query = self.scope(index);
        let Some(registry) = self.registry.as_deref() else {
            return Task::none();
        };
        let workspace = &mut self.workspaces[index];
        let Some(tab) = workspace
            .panes
            .iter_mut()
            .flat_map(|(_, pane)| &mut pane.tabs)
            .find(|tab| tab.id == id)
        else {
            return Task::none();
        };
        if !tab.reminders.consent
            || tab.reminders.recovery.flight.is_some()
            || !cfg!(target_os = "linux")
            || tab.provider != Some(Provider::Codex)
        {
            return Task::none();
        }
        let Some(recipient) = &mut tab.recipient else {
            return Task::none();
        };
        let Ok(actor) = recipient.resolve(registry).cloned() else {
            return Task::none();
        };
        if amux::project_identity::resolve(&query.directory)
            .ok()
            .map(|p| p.directory)
            .as_ref()
            != Some(&actor.project_directory)
        {
            return Task::none();
        }
        let Some(terminal) = tab.terminal.as_ref() else {
            return Task::none();
        };
        if receive.is_some()
            && tab
                .reminders
                .gate
                .observe(terminal, recipient, Instant::now())
                .is_none()
        {
            return Task::none();
        }
        let credentials = match desktop_recovery::Credentials::from_preferences(
            query.project.trim(),
            &self.preferences.current,
        ) {
            Ok(credentials) => credentials,
            Err(error) => {
                tab.reminders.recovery.queried_at = Some(Instant::now());
                tab.reminders.recovery.snapshot = None;
                tab.reminders.status = format!("Gateway recovery unavailable: {error}");
                return Task::none();
            }
        };
        tab.reminders.recovery.actor = Some(actor.clone());
        let scope = RecoveryScope {
            query,
            terminal: id,
            epoch: tab.reminders.recovery.epoch,
            actor: actor.clone(),
        };
        let task = match receive {
            Some(task) => {
                if tab.reminders.recovery.blocked_receipts.contains(&task)
                    || !tab.reminders.recovery.fresh()
                    || tab
                        .reminders
                        .recovery
                        .snapshot
                        .as_ref()
                        .is_none_or(|snapshot| {
                            snapshot.unresolved_receipts.contains(&task)
                                || !snapshot
                                    .awaiting_receipt
                                    .iter()
                                    .any(|work| work.task == task)
                        })
                {
                    return Task::none();
                }
                Task::perform(
                    async move {
                        desktop_recovery::receive(credentials, actor, task)
                            .await
                            .map_err(|error| error.to_string())
                    },
                    move |result| Message::ReceiptReported(scope.clone(), task, result),
                )
            }
            None => Task::perform(
                async move {
                    desktop_recovery::query(credentials, actor)
                        .await
                        .map_err(|error| error.to_string())
                },
                move |result| Message::RecoveryLoaded(scope.clone(), result),
            ),
        };
        let (task, handle) = task.abortable();
        tab.reminders.recovery.flight = Some(handle.abort_on_drop());
        tab.reminders.recovery.queried_at = Some(Instant::now());
        if receive.is_none() {
            tab.reminders.recovery.snapshot = None;
        }
        task
    }

    fn withdraw(&mut self, id: u64) {
        for workspace in &mut self.workspaces {
            for (_, pane) in workspace.panes.iter_mut() {
                if let Some(tab) = pane.tabs.iter_mut().find(|tab| tab.id == id) {
                    tab.reminders.consent = false;
                    tab.reminders.recovery.reset();
                    tab.reminders.status = "Reminders off; pending input retired.".into();
                    if let Some(recipient) = &mut tab.recipient {
                        recipient.set_consent(false);
                    }
                }
            }
        }
        if let Some(ledger) = &mut self.ledger
            && let Err(error) = ledger.retire_terminal(id)
        {
            self.status = format!("Reminder retirement failed: {error}. Delivery disabled.");
        }
    }

    fn delivery_tick(&mut self) -> Task<Message> {
        if self.preferences.current.automatic_notifications {
            return Task::none();
        }
        let Some(ledger) = &mut self.ledger else {
            return Task::none();
        };
        let automatic = std::env::var_os("AMUX_GATEWAY_MACHINE_ID").is_some()
            || std::env::var_os("AMUX_GATEWAY_MACHINE_KEY").is_some();
        let mut legacy_queries = Vec::new();
        let mut recovery_queries = Vec::new();
        let mut receipts = Vec::new();
        for (index, workspace) in self.workspaces.iter_mut().enumerate() {
            let mut legacy_pending = false;
            for (_, pane) in workspace.panes.iter_mut() {
                for tab in &mut pane.tabs {
                    if !tab.reminders.consent {
                        continue;
                    }
                    let (Some(terminal), Some(recipient), Some(registry)) =
                        (&tab.terminal, &mut tab.recipient, &self.registry)
                    else {
                        continue;
                    };
                    let Ok(actor) = recipient.resolve(registry).cloned() else {
                        continue;
                    };
                    let bridge = &mut tab.reminders.recovery;
                    let poll_due = automatic
                        && cfg!(target_os = "linux")
                        && tab.provider == Some(Provider::Codex)
                        && bridge.flight.is_none()
                        && bridge
                            .queried_at
                            .is_none_or(|time| time.elapsed() >= Duration::from_secs(10));
                    if poll_due {
                        recovery_queries.push((index, tab.id));
                    }
                    let snapshot = bridge
                        .snapshot
                        .as_ref()
                        .filter(|_| bridge.fresh() && bridge.actor.as_ref() == Some(&actor));
                    let mut record = ledger
                        .records()
                        .iter()
                        .find(|record| {
                            record.recipient.terminal_id() == tab.id
                                && record.state == DeliveryState::Pending
                        })
                        .cloned();
                    if record.is_none()
                        && let Some(snapshot) = snapshot
                        && let Some(address) = recipient.address(tab.id)
                        && let Some(work) = snapshot.ready.iter().find(|work| {
                            !ledger.records().iter().any(|record| {
                                record.task_id == work.task
                                    && record.recipient == address
                                    && record.cause == DeliveryCause::RecoveryQuery
                            })
                        })
                    {
                        match ledger.queue(
                            work.task,
                            address,
                            DeliveryCause::RecoveryQuery,
                            snapshot.observed_at_ms,
                            unix_ms(),
                        ) {
                            Ok(id) => {
                                bridge.managed.insert(work.task);
                                record = ledger
                                    .records()
                                    .iter()
                                    .find(|record| {
                                        record.id == id && record.state == DeliveryState::Pending
                                    })
                                    .cloned();
                            }
                            Err(error) => {
                                tab.reminders.status =
                                    format!("Recovery reminder not queued: {error}")
                            }
                        }
                    }
                    if let Some(record) = record {
                        let managed = bridge.managed.contains(&record.task_id);
                        legacy_pending |= !managed;
                        let (observed_at, reminder) = if managed {
                            let Some(snapshot) = snapshot else {
                                continue;
                            };
                            let Some(work) = snapshot
                                .ready
                                .iter()
                                .find(|work| work.task == record.task_id)
                            else {
                                if let Err(error) = ledger.retire_pending(record.id) {
                                    tab.reminders.status =
                                        format!("Reminder retirement failed: {error}");
                                }
                                continue;
                            };
                            if poll_due {
                                continue;
                            }
                            (snapshot.observed_at_ms, work.text.clone())
                        } else {
                            if !workspace.gateway.fresh() {
                                continue;
                            }
                            if !workspace.gateway.pending_task(record.task_id) {
                                if let Err(error) = ledger.retire_pending(record.id) {
                                    tab.reminders.status =
                                        format!("Reminder retirement failed: {error}");
                                } else {
                                    tab.reminders.status =
                                        "Task is no longer pending; reminder retired.".into();
                                }
                                continue;
                            }
                            (
                                workspace.gateway.observed_at_ms,
                                format!(
                                    "Pending task {} in project {}. Fetch its current record and check eligibility before deciding to claim.",
                                    record.task_id,
                                    workspace.gateway.project.trim()
                                ),
                            )
                        };
                        let now = unix_ms();
                        if let Err(error) = ledger.queue(
                            record.task_id,
                            record.recipient.clone(),
                            DeliveryCause::RecoveryQuery,
                            observed_at,
                            now,
                        ) {
                            tab.reminders.status = format!("Reminder storage failed: {error}");
                            continue;
                        }
                        match submit_literal(
                            ledger,
                            &mut tab.reminders.gate,
                            terminal,
                            recipient,
                            record.id,
                            &reminder,
                            now,
                        ) {
                            Ok(Submission::Deferred) => {}
                            Ok(Submission::Enqueued) => {
                                tab.reminders.status =
                                    "Reminder enqueued; agent acknowledgment and execution unknown."
                                        .into()
                            }
                            Err(error) => {
                                tab.reminders.status = format!(
                                    "Reminder paused: {error}; no automatic retry after an input attempt."
                                )
                            }
                        }
                    } else if tab
                        .reminders
                        .gate
                        .observe(terminal, recipient, Instant::now())
                        .is_some()
                        && !poll_due
                        && bridge.flight.is_none()
                        && let Some(snapshot) = snapshot
                        && let Some(work) = snapshot.awaiting_receipt.iter().find(|work| {
                            !bridge.blocked_receipts.contains(&work.task)
                                && !snapshot.unresolved_receipts.contains(&work.task)
                        })
                    {
                        receipts.push((index, tab.id, work.task));
                    }
                }
            }
            if legacy_pending
                && !workspace.gateway.loading
                && workspace
                    .gateway
                    .queried_at
                    .is_none_or(|time| time.elapsed() >= Duration::from_secs(10))
            {
                legacy_queries.push(index);
            }
        }
        let mut tasks: Vec<_> = legacy_queries
            .into_iter()
            .map(|index| self.query(index))
            .collect();
        tasks.extend(
            recovery_queries
                .into_iter()
                .map(|(index, id)| self.recovery_request(index, id, None)),
        );
        tasks.extend(
            receipts
                .into_iter()
                .map(|(index, id, task)| self.recovery_request(index, id, Some(task))),
        );
        Task::batch(tasks)
    }

    fn task_form(&self) -> Element<'_, Message> {
        let panel = &self.workspaces[self.active].gateway;
        if let Some(request) = &panel.request {
            let mut receipt = column![text(&request.title).size(14)].spacing(6);
            match &request.state {
                TaskRequestState::Sending => {
                    receipt = receipt.push(
                        text(format!(
                            "Awaiting gateway acceptance in {}…",
                            request.project
                        ))
                        .size(12),
                    );
                }
                TaskRequestState::Accepted(task) => {
                    receipt = receipt.push(
                        text(format!("Accepted by gateway: {} / {task}", request.project)).size(12),
                    );
                }
                TaskRequestState::Unconfirmed(error) => {
                    receipt = receipt.push(text(format!(
                        "Acceptance unknown in {}: {error}. Check gateway tasks before starting a different request. This request is not retried automatically.", request.project
                    )).size(12));
                }
            }
            if !matches!(request.state, TaskRequestState::Sending) {
                receipt = receipt
                    .push(button("Start different request").on_press(Message::NewTaskRequest));
            }
            return receipt.into();
        }
        column![
            text("Target: automatic project routing").size(11),
            text_input("Task title", &panel.draft_title).on_input(Message::TaskTitle),
            text_editor(&panel.draft_context)
                .placeholder("Context / handoff text (optional)")
                .on_action(Message::TaskContext)
                .height(100),
            button("Submit Task to Gateway")
                .style(desktop_design::primary_action)
                .on_press(Message::SubmitTask)
                .width(Length::Fill),
            text(&panel.draft_status).size(12),
        ]
        .spacing(6)
        .into()
    }

    fn subscription(&self) -> Subscription<Message> {
        let terminals = Subscription::batch(
            self.workspaces
                .iter()
                .flat_map(|w| w.panes.iter())
                .flat_map(|(_, p)| &p.tabs)
                .filter_map(|t| t.terminal.as_ref())
                .map(|t| t.subscription().map(Message::Terminal)),
        );
        Subscription::batch([
            terminals,
            iced::window::resize_events().map(|(id, size)| Message::WindowResized(id, size)),
            iced::event::listen_with(|event, _, _| match event {
                iced::Event::Keyboard(iced::keyboard::Event::KeyPressed {
                    key: iced::keyboard::Key::Named(iced::keyboard::key::Named::Escape),
                    ..
                }) => Some(Message::Escape),
                iced::Event::Keyboard(iced::keyboard::Event::KeyPressed {
                    key: iced::keyboard::Key::Character(key),
                    modifiers,
                    ..
                }) if key.as_str() == "," && modifiers == iced::keyboard::Modifiers::CTRL => {
                    Some(Message::Preferences(true))
                }
                _ => None,
            }),
            iced::time::every(std::time::Duration::from_secs(1)).map(|_| Message::Save),
            iced::window::close_requests().map(Message::Close),
            iced::time::every(Duration::from_millis(250)).map(|_| Message::DeliveryTick),
        ])
    }

    fn view(&self) -> Element<'_, Message> {
        self.design_view()
    }
}

impl TaskPanel {
    fn fresh(&self) -> bool {
        self.loaded_at
            .is_some_and(|time| time.elapsed() <= Duration::from_secs(15))
    }

    fn pending_task(&self, id: Uuid) -> bool {
        self.fresh()
            && self.tasks.iter().any(|task| {
                task["id"]
                    .as_str()
                    .and_then(|value| Uuid::parse_str(value).ok())
                    == Some(id)
                    && task["status"] == "todo"
                    && task["owner_origin"].is_null()
                    && task["owner_agent_id"].as_str().is_none_or(str::is_empty)
            })
    }
}

fn unix_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis()
        .try_into()
        .unwrap_or(u64::MAX)
}

fn short_title(title: &str) -> String {
    let mut characters = title.chars();
    let mut short: String = characters
        .by_ref()
        .take(32)
        .map(|c| if c.is_control() { ' ' } else { c })
        .collect();
    if characters.next().is_some() {
        short.push('…');
    }
    short
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    // No display, provider process, network, or environment mutation is involved.
    pub(super) fn desktop() -> Desktop {
        let mut workspaces = Vec::new();
        for id in 0..2 {
            let (panes, focused) = pane_grid::State::new(Pane {
                tabs: vec![Tab {
                    id,
                    title: "stopped".into(),
                    #[cfg(any(target_os = "linux", windows))]
                    lifetime: None,
                    terminal: None,
                    stopped_output: None,
                    provider: Some(Provider::Codex),
                    recipient: None,
                    reminders: Reminders::default(),
                    delegation: desktop_delegation::TabState::default(),
                }],
                active: 0,
            });
            workspaces.push(Workspace {
                name: None,
                id: Uuid::new_v4(),
                directory: PathBuf::from(format!("/fixture/{id}")),
                init_script: None,
                panes,
                focused,
                gateway: TaskPanel {
                    project: format!("project-{id}"),
                    ..Default::default()
                },
            });
        }
        Desktop {
            design: desktop_design::State::default(),
            main_window: iced::window::Id::unique(),
            updates: Updates {
                running: None,
                installing: false,
                install_status: String::new(),
                checking: false,
                checked_at: Some(Instant::now()),
                restart_checked_at: Some(Instant::now()),
                report: None,
                restart: amux::updates::Restart {
                    required: false,
                    path: None,
                },
                error: String::new(),
            },
            preferences: Preferences {
                current: GatewayPreferences::default(),
                draft: GatewayPreferences::default(),
                path: None,
                open: false,
                tab: PreferencesTab::Gateway,
                status: String::new(),
            },
            workspaces,
            active: 0,
            workspace_dialog: WorkspaceDialog::default(),
            closing_workspace: None,
            next_id: 2,
            status: String::new(),
            ledger: None,
            registry: None,
            snapshot_path: None,
            dirty: false,
            restarting: false,
            notify_exits: false,
            notifier: None,
            notification_status: String::new(),
            delegation: desktop_delegation::Bridge::default(),
        }
    }

    #[test]
    fn workspace_removal_preserves_selected_peer_and_saves_only_remaining_layout() {
        let mut app = desktop();
        let (fixture, _, _) = restart_fixture();
        let path = fixture.path().join("removed-layout.json");
        app.snapshot_path = Some(path.clone());
        for (i, workspace) in app.workspaces.iter_mut().enumerate() {
            workspace.directory = fixture.path().join(format!("project-{i}"));
            std::fs::create_dir(&workspace.directory).unwrap();
            std::fs::write(workspace.directory.join("keep.txt"), "project data").unwrap();
        }
        let removed_directory = app.workspaces[0].directory.clone();
        app.active = 1;
        let selected = app.workspaces[1].id;
        let removed = app.workspaces[0].id;
        drop(app.update(Message::CloseWorkspace(removed)));
        assert_eq!(app.workspaces.len(), 1);
        assert_eq!(app.workspaces[app.active].id, selected);
        assert_eq!(app.active, 0);
        assert_eq!(
            std::fs::read_to_string(removed_directory.join("keep.txt")).unwrap(),
            "project data"
        );
        let saved = Snapshot::load(&path).unwrap().unwrap();
        assert_eq!(saved.workspaces.len(), 1);
        assert_eq!(saved.workspaces[0].directory, app.workspaces[0].directory);
        assert_eq!(saved.active, 0);
        drop(app.update(Message::RemoveWorkspace(removed)));
        assert_eq!(
            app.workspaces.len(),
            1,
            "stale close targets cannot remove a peer"
        );
    }

    #[test]
    fn closing_workspace_retargets_peer_callbacks_and_discards_removed_callbacks() {
        let mut app = desktop();
        // Identical paths/projects are legal; callbacks must distinguish actual
        // workspace identity, not just an index and repository path.
        app.workspaces[1].directory = app.workspaces[0].directory.clone();
        app.workspaces[1].gateway.project = app.workspaces[0].gateway.project.clone();
        let removed_scope = app.scope(0);
        let surviving_scope = app.scope(1);
        let request = Uuid::new_v4();
        let accepted = Uuid::new_v4();
        app.workspaces[1].gateway.request = Some(TaskRequest {
            id: request,
            project: surviving_scope.project.clone(),
            title: "Keep receipt".into(),
            state: TaskRequestState::Sending,
        });
        app.workspaces[1].gateway.loading = true;
        let removed = app.workspaces[0].id;
        drop(app.update(Message::RemoveWorkspace(removed)));
        drop(app.update(Message::TasksLoaded(
            removed_scope,
            Ok(json!([{"id": Uuid::new_v4(), "title":"wrong workspace", "status":"todo"}])),
        )));
        assert!(app.workspaces[0].gateway.tasks.is_empty());
        drop(app.update(Message::TasksLoaded(surviving_scope.clone(), Ok(json!([])))));
        assert!(
            !app.workspaces[0].gateway.loading,
            "shifted peer query must finish"
        );
        drop(app.update(Message::TaskCreated(surviving_scope, request, Ok(accepted))));
        assert!(
            matches!(app.workspaces[0].gateway.request.as_ref().unwrap().state, TaskRequestState::Accepted(id) if id == accepted)
        );
    }

    #[test]
    fn workspace_removal_keeps_unconfirmed_gateway_receipt_visible() {
        let mut app = desktop();
        let id = app.workspaces[0].id;
        app.workspaces[0].gateway.request = Some(TaskRequest {
            id: Uuid::new_v4(),
            project: "project-0".into(),
            title: "Unknown acceptance".into(),
            state: TaskRequestState::Unconfirmed("connection lost".into()),
        });
        drop(app.update(Message::RemoveWorkspace(id)));
        assert_eq!(app.workspaces.len(), 2);
        assert!(app.workspaces[0].gateway.request.is_some());
        assert!(app.status.contains("receipt"));
    }

    #[test]
    fn workspace_dialog_does_not_create_until_validated_and_clone_blocks_duplicate_actions() {
        use desktop_workspace::{Kind, WorkspaceMessage as W};
        let mut app = desktop();
        let before = app.workspaces.len();
        drop(app.update(Message::NewWorkspace));
        assert!(app.workspace_dialog.open);
        drop(app.update(Message::NewWorkspace));
        assert!(app.workspace_dialog.open);
        assert_eq!(app.workspaces.len(), before);
        drop(app.update(Message::Workspace(W::Folder(
            "target/no-such-workspace-directory".into(),
        ))));
        drop(app.update(Message::Workspace(W::Start)));
        assert_eq!(app.workspaces.len(), before);
        drop(app.update(Message::Workspace(W::Tab(Kind::Script))));
        drop(app.update(Message::Workspace(W::Start)));
        assert_eq!(app.workspaces.len(), before);
        app.workspace_dialog.busy = true;
        drop(app.update(Message::Workspace(W::Cancel)));
        drop(app.update(Message::Close(app.main_window)));
        assert!(app.workspace_dialog.open);
        assert_eq!(app.workspaces.len(), before);
        app.workspace_dialog.busy = false;
        drop(app.update(Message::Workspace(W::Cancel)));
        assert!(!app.workspace_dialog.open);
    }

    #[test]
    fn preferences_modal_preserves_workspaces_and_draft_between_tabs() {
        let mut app = desktop();
        let count = app.workspaces.len();
        let _ = app.update(Message::Preferences(true));
        assert!(app.preferences.open);
        app.preferences.draft.url = "https://gateway.nitecon.org".into();
        let _ = app.update(Message::Updates(true));
        assert_eq!(app.preferences.tab, PreferencesTab::Updates);
        assert_eq!(app.preferences.draft.url, "https://gateway.nitecon.org");
        let _ = app.update(Message::Preferences(false));
        assert!(!app.preferences.open);
        assert_eq!(app.workspaces.len(), count);
        assert!(app.preferences.draft.url.is_empty());
    }

    #[test]
    fn project_resolution_is_fenced_and_binds_its_own_background_workspace() {
        let mut app = desktop();
        app.workspaces[0].gateway.project.clear();
        let old = app.scope(0);
        app.workspaces[0].gateway.revision += 1;
        let _ = app.update(Message::ProjectResolved(
            old,
            Ok("wrong-old-project".into()),
        ));
        assert!(app.workspaces[0].gateway.project.is_empty());
        let fresh = app.scope(0);
        app.active = 1;
        let peer = app.workspaces[1].gateway.project.clone();
        let _ = app.update(Message::ProjectResolved(
            fresh.clone(),
            Ok("canonical-alias".into()),
        ));
        assert_eq!(app.workspaces[0].gateway.project, "canonical-alias");
        assert_eq!(app.workspaces[1].gateway.project, peer);
        let _ = app.update(Message::ProjectResolved(fresh, Err("late failure".into())));
        assert!(!app.workspaces[0].gateway.status.contains("late failure"));
    }

    #[test]
    fn ui_update_refuses_managed_installs_and_prevents_duplicate_install_or_restart() {
        let mut app = desktop();
        app.updates.report = Some(update_report());
        app.updates.report.as_mut().unwrap().guidance = amux::updates::Guidance::Homebrew {
            command: "brew upgrade amux".into(),
        };
        let _ = app.update(Message::InstallUpdate);
        assert!(!app.updates.installing);
        app.updates.report = Some(update_report());
        let report = app.updates.report.as_mut().unwrap();
        report.method = amux::updates::InstallMethod::Direct {
            dir: PathBuf::from("fixture-not-executed"),
            revision: "a".repeat(40),
        };
        report.guidance = amux::updates::Guidance::CliUpdate {
            command: "amux --update".into(),
        };
        app.updates.running = Some(std::sync::Arc::new(amux::updates::Running::at(
            PathBuf::from("fixture-not-executed"),
        )));
        assert!(app.update(Message::InstallUpdate).units() > 0);
        assert!(app.updates.installing);
        assert_eq!(app.update(Message::InstallUpdate).units(), 0);
        let _ = app.update(Message::RestartUpdated);
        assert!(!app.restarting);
        let _ = app.update(Message::Close(app.main_window));
        assert!(app.status.contains("installing"));
        let _ = app.update(Message::UpdateInstalled(Err(
            "private fixture failure".into()
        )));
        assert!(!app.updates.installing);
        assert!(
            app.updates
                .install_status
                .contains("private fixture failure")
        );
    }

    fn update_report() -> amux::updates::UpdateCheck {
        amux::updates::UpdateCheck {
            method: amux::updates::InstallMethod::Homebrew {
                prefix: PathBuf::from("/fixture/brew"),
                cask: false,
            },
            channel: amux::updates::Channel::Stable,
            installed: None,
            available: Some(amux::updates::Release {
                tag: "v0.2.0".into(),
                name: "amux0.2".into(),
                published_at: "2026-10-24T00:00:00Z".into(),
                html_url: "https://github.com/nitecon/amux/releases/tag/v0.2.0".into(),
                prerelease: false,
            }),
            guidance: amux::updates::Guidance::Homebrew {
                command: "brew upgrade amux".into(),
            },
            restart: amux::updates::Restart {
                required: false,
                path: None,
            },
            note: None,
        }
    }

    #[test]
    fn update_badge_survives_a_failed_refresh_without_changing_sessions_or_consent() {
        let mut app = desktop();
        let workspace = &mut app.workspaces[0];
        workspace.panes.get_mut(workspace.focused).unwrap().tabs[0]
            .reminders
            .consent = true;
        let _ = app.update(Message::UpdatesChecked(Ok(update_report())));
        assert!(app.updates.attention());
        let _ = app.update(Message::UpdatesChecked(Err("offline".into())));
        assert!(app.updates.attention());
        assert!(app.updates.error.contains("offline"));
        let _ = app.update(Message::Updates(true));
        assert!(app.preferences.open);
        assert_eq!(app.preferences.tab, PreferencesTab::Updates);
        assert!(
            app.workspaces[0]
                .panes
                .get(app.workspaces[0].focused)
                .unwrap()
                .tabs[0]
                .reminders
                .consent
        );
        assert_eq!(app.workspaces.len(), 2);
    }

    #[test]
    fn update_restart_rechecks_disk_instead_of_trusting_an_old_report() {
        let (dir, mut app) = preferences_fixture();
        let exe = dir.path().join("unchanged-amux");
        std::fs::write(&exe, b"unchanged fixture").unwrap();
        app.updates.running = Some(std::sync::Arc::new(amux::updates::Running::at(exe.clone())));
        let mut report = update_report();
        report.restart = amux::updates::Restart {
            required: true,
            path: Some(exe),
        };
        let _ = app.update(Message::UpdatesChecked(Ok(report)));
        let _ = app.update(Message::RestartUpdated);
        assert!(app.updates.error.contains("No updated desktop"));
        assert!(!app.dirty);
        assert_eq!(app.workspaces.len(), 2);
    }

    fn restart_fixture() -> (tempfile::TempDir, Desktop, PathBuf) {
        let (dir, mut app) = preferences_fixture();
        let exe = dir.path().join("amux-desktop-fixture");
        std::fs::write(&exe, b"old").unwrap();
        app.updates.running = Some(std::sync::Arc::new(amux::updates::Running::at(exe.clone())));
        std::fs::write(&exe, b"new replacement image").unwrap();
        for workspace in &mut app.workspaces {
            workspace.directory = dir.path().canonicalize().unwrap();
        }
        app.snapshot_path = Some(dir.path().join("layout.json"));
        app.registry = Some(dir.path().to_owned());
        let ledger_path = dir.path().join("deliveries.json");
        app.ledger = Some(DeliveryLedger::open(&ledger_path).unwrap());
        (dir, app, ledger_path)
    }

    #[test]
    fn restart_releases_the_ledger_before_the_replacement_boots() {
        let (_dir, mut app, ledger_path) = restart_fixture();
        assert!(DeliveryLedger::open(&ledger_path).is_err());
        let opened = std::cell::Cell::new(false);
        let _ = app.restart_with(|_, _| {
            let _replacement_ledger = DeliveryLedger::open(&ledger_path)?;
            opened.set(true);
            Ok(())
        });
        assert!(
            opened.get(),
            "replacement failed to acquire ledger: {}",
            app.updates.error
        );
    }

    #[test]
    fn offline_update_check_does_not_hide_a_locally_ready_restart() {
        let (_dir, mut app, _ledger) = restart_fixture();
        app.updates.report = None;
        let _ = app.update(Message::UpdatesChecked(Err("offline".into())));
        assert!(app.updates.report.is_none());
        assert!(app.updates.restart.required);
        assert!(app.updates.attention());
        assert!(app.updates.error.contains("offline"));
    }

    #[test]
    fn failed_restart_reacquires_ledger_and_keeps_the_current_app_usable() {
        let (_dir, mut app, ledger_path) = restart_fixture();
        let _ = app.restart_with(|_, _| anyhow::bail!("fixture launch failed"));
        assert!(!app.restarting);
        assert!(app.ledger.is_some());
        assert!(DeliveryLedger::open(&ledger_path).is_err());
        assert!(app.updates.error.contains("fixture launch failed"));
        let _ = app.update(Message::Updates(true));
        assert!(app.preferences.open);
        assert_eq!(app.preferences.tab, PreferencesTab::Updates);
    }

    fn preferences_fixture() -> (tempfile::TempDir, Desktop) {
        let base = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&base).unwrap();
        let dir = tempfile::tempdir_in(base).unwrap();
        let mut app = desktop();
        app.preferences.path = Some(dir.path().join("preferences.json"));
        app.preferences.draft = GatewayPreferences {
            url: "http://127.0.0.1:9".into(),
            api_key: "fixture-key".into(),
            ..Default::default()
        };
        (dir, app)
    }

    #[test]
    fn gateway_rebind_rejects_late_lists_and_withdraws_all_consent() {
        let (_dir, mut app) = preferences_fixture();
        let old_scope = app.scope(0);
        for workspace in &mut app.workspaces {
            workspace.gateway.loading = true;
            workspace.gateway.tasks.push(json!({"id": Uuid::new_v4()}));
            workspace.panes.get_mut(workspace.focused).unwrap().tabs[0]
                .reminders
                .consent = true;
        }
        let _ = app.update(Message::SavePreferences);
        assert!(app.workspaces.iter().all(|w| w.gateway.project.is_empty()));
        for workspace in &app.workspaces {
            assert!(workspace.gateway.loading);
            assert!(workspace.gateway.tasks.is_empty());
            assert!(
                !workspace.panes.get(workspace.focused).unwrap().tabs[0]
                    .reminders
                    .consent
            );
        }
        let _ = app.update(Message::TasksLoaded(
            old_scope,
            Ok(json!([{"id": Uuid::new_v4()}])),
        ));
        assert!(app.workspaces[0].gateway.tasks.is_empty());
        assert!(
            GatewayPreferences::load(app.preferences.path.as_deref().unwrap())
                .unwrap()
                .is_some()
        );
    }

    #[test]
    fn invalid_preferences_and_pending_posts_do_not_replace_the_connection() {
        let (_dir, mut app) = preferences_fixture();
        app.preferences.draft.url = "https://user:secret@example.test".into();
        let _ = app.update(Message::SavePreferences);
        assert!(app.preferences.current.url.is_empty());
        assert!(!app.preferences.status.contains("secret"));
        app.preferences.draft.url = "https://example.test".into();
        app.workspaces[0].gateway.request = Some(TaskRequest {
            id: Uuid::new_v4(),
            title: "pending".into(),
            project: "fixture".into(),
            state: TaskRequestState::Sending,
        });
        let _ = app.update(Message::SavePreferences);
        assert!(app.preferences.current.url.is_empty());
        assert!(!app.preferences.path.as_ref().unwrap().exists());
    }

    #[test]
    fn close_button_targets_its_own_unfocused_pane_and_preserves_selected_tab() {
        let mut app = desktop();
        let mut other = app.workspaces.pop().unwrap();
        let tab = other.panes.get_mut(other.focused).unwrap().tabs.remove(0);
        let w = &mut app.workspaces[0];
        let original = w.focused;
        let (other_pane, _) = w
            .panes
            .split(
                pane_grid::Axis::Vertical,
                original,
                Pane {
                    tabs: vec![tab],
                    active: 0,
                },
            )
            .unwrap();
        let _ = app.update(Message::CloseTab(other_pane, 1));
        assert!(app.workspaces[0].panes.get(other_pane).is_none());
        assert_eq!(app.workspaces[0].focused, original);
        assert_eq!(app.workspaces[0].panes.get(original).unwrap().tabs[0].id, 0);
        // Replaying a stale close must not close the remaining terminal.
        let _ = app.update(Message::CloseTab(other_pane, 1));
        assert_eq!(app.workspaces[0].panes.len(), 1);
    }

    #[cfg(any(target_os = "linux", windows))]
    #[test]
    fn startup_reopens_running_tabs_in_place_and_preserves_stopped_tabs() {
        let base = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&base).unwrap();
        let fixture = tempfile::tempdir_in(&base).unwrap();
        let directory = fixture.path().join("project");
        std::fs::create_dir(&directory).unwrap();
        let mut app = desktop();
        app.registry = Some(fixture.path().join("state"));
        let layout = Layout::Pane {
            active: 1,
            tabs: vec![
                SavedTab {
                    terminal_number: Some(42),
                    id: 42,
                    title: "Restored shell".into(),
                    provider: None,
                    running: true,
                    directory: Some(directory.clone()),
                },
                SavedTab {
                    terminal_number: Some(43),
                    id: 43,
                    title: "Exited agent".into(),
                    provider: Some(Provider::Claude),
                    running: false,
                    directory: None,
                },
            ],
        };
        let config = app.restore_layout(layout, fixture.path());
        let panes = pane_grid::State::with_configuration(config);
        let pane = panes.iter().next().unwrap().1;
        assert_eq!(pane.active, 1);
        assert_eq!(pane.tabs.len(), 2);
        assert_eq!(pane.tabs[0].id, 42);
        assert_eq!(pane.tabs[0].title, "Restored shell");
        let terminal = pane.tabs[0]
            .terminal
            .as_ref()
            .expect("restored tab must own a live terminal");
        assert_eq!(pane.tabs[0].delegation.terminal_number, 42);
        assert!(terminal.child_pid().is_some());
        assert_eq!(
            pane.tabs[0].delegation.directory.as_ref(),
            Some(&amux::workspace_launch::external_path(directory))
        );
        assert_eq!(pane.tabs[1].id, 43);
        assert!(pane.tabs[1].terminal.is_none());
        assert!(pane.tabs.iter().all(|t| !t.reminders.consent));
        assert_eq!(app.next_id, 44);
    }

    #[cfg(any(target_os = "linux", windows))]
    #[test]
    fn saved_session_keeps_live_state_and_terminal_directory() {
        let base = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&base).unwrap();
        let fixture = tempfile::tempdir_in(&base).unwrap();
        let mut app = desktop();
        app.registry = Some(fixture.path().join("state"));
        app.snapshot_path = Some(fixture.path().join("layout.json"));
        let path = app.snapshot_path.clone().unwrap();
        let directory =
            amux::workspace_launch::external_path(fixture.path().canonicalize().unwrap());
        let opened = app.terminal_with_script(&directory, None, None);
        let workspace = &mut app.workspaces[0];
        workspace.directory = directory.clone();
        workspace.panes.get_mut(workspace.focused).unwrap().tabs = vec![opened];
        app.workspaces.truncate(1);
        app.dirty = true;
        assert!(app.save());
        let snapshot = Snapshot::load(&path).unwrap().unwrap();
        let Layout::Pane { tabs, .. } = &snapshot.workspaces[0].layout else {
            panic!("expected pane")
        };
        assert!(tabs[0].running);
        assert_eq!(tabs[0].terminal_number, Some(1));
        assert_eq!(tabs[0].directory.as_ref(), Some(&directory));
    }

    #[test]
    fn titles_are_short_single_line_unicode_labels() {
        assert_eq!(short_title("cwd"), "cwd");
        assert_eq!(short_title("a\nb\tc"), "a b c");
        assert_eq!(
            short_title(&"界".repeat(80)),
            format!("{}…", "界".repeat(32))
        );
    }

    #[cfg(target_os = "linux")]
    fn recovery_fixture() -> (tempfile::TempDir, Desktop, RecoveryScope) {
        use amux::identity::{AgentIdentity, AgentRecord};
        use std::os::unix::fs::PermissionsExt;
        let target = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&target).unwrap();
        let directory = tempfile::tempdir_in(target).unwrap();
        std::fs::write(
            directory.path().join("codex"),
            "#!/bin/sh\nwhile :; do /bin/sleep 1; done\n",
        )
        .unwrap();
        std::fs::set_permissions(
            directory.path().join("codex"),
            std::fs::Permissions::from_mode(0o700),
        )
        .unwrap();
        let pid_path = directory.path().join("child.pid");
        let terminal = Terminal::new(
            0,
            iced_term::settings::Settings {
                backend: iced_term::settings::BackendSettings {
                    program: "/bin/sh".into(),
                    args: vec![
                        "-c".into(),
                        "\"$1\" & echo $! > \"$2\"; wait".into(),
                        "amux-fixture".into(),
                        directory.path().join("codex").display().to_string(),
                        pid_path.display().to_string(),
                    ],
                    working_directory: Some(directory.path().into()),
                    ..Default::default()
                },
                ..Default::default()
            },
        )
        .unwrap();
        let deadline = Instant::now() + Duration::from_secs(8);
        let child = loop {
            if let Ok(pid) = std::fs::read_to_string(&pid_path)
                && let Ok(pid) = pid.trim().parse::<u32>()
            {
                break pid;
            }
            assert!(Instant::now() < deadline);
            std::thread::sleep(Duration::from_millis(10));
        };
        let actor = AgentIdentity::for_terminal(Provider::Codex, directory.path(), 1).unwrap();
        let record = AgentRecord::inspect(actor.clone(), child).unwrap();
        std::fs::create_dir(directory.path().join("agents")).unwrap();
        std::fs::write(
            directory
                .path()
                .join("agents")
                .join(format!("{}.json", actor.session_id)),
            serde_json::to_vec(&record).unwrap(),
        )
        .unwrap();
        let lifetime =
            amux::terminal_lifetime::TerminalLifetime::capture(terminal.child_pid().unwrap())
                .unwrap();
        let mut recipient = TerminalRecipient::new(
            terminal.child_pid().unwrap(),
            Provider::Codex,
            directory.path(),
        )
        .unwrap();
        recipient.resolve(directory.path()).unwrap();
        recipient.set_consent(true);
        let mut app = desktop();
        app.registry = Some(directory.path().into());
        app.ledger = Some(DeliveryLedger::open(&directory.path().join("deliveries.json")).unwrap());
        app.workspaces[0].directory = directory.path().canonicalize().unwrap();
        let focused = app.workspaces[0].focused;
        let tab = &mut app.workspaces[0].panes.get_mut(focused).unwrap().tabs[0];
        tab.lifetime = Some(lifetime);
        tab.terminal = Some(terminal);
        tab.recipient = Some(recipient);
        tab.reminders.consent = true;
        tab.reminders.recovery.epoch = 1;
        let scope = RecoveryScope {
            query: app.scope(0),
            terminal: 0,
            epoch: 1,
            actor,
        };
        (directory, app, scope)
    }

    #[cfg(target_os = "linux")]
    fn recovery_snapshot(task: Uuid) -> desktop_recovery::Recovery {
        desktop_recovery::Recovery {
            ready: vec![desktop_recovery::Work {
                task,
                text: format!(
                    "Task {task} project project-0. Check current progress before any claim or execution."
                ),
            }],
            awaiting_receipt: Vec::new(),
            unresolved_receipts: HashSet::new(),
            observed_at_ms: unix_ms(),
            queried_at: Instant::now(),
        }
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn recovery_callbacks_bind_consent_epoch_project_and_real_recipient() {
        let (_directory, mut app, scope) = recovery_fixture();
        let task = Uuid::new_v4();
        let _ = app.update(Message::RecoveryLoaded(
            scope.clone(),
            Ok(recovery_snapshot(task)),
        ));
        let focused = app.workspaces[0].focused;
        assert!(
            app.workspaces[0].panes.get(focused).unwrap().tabs[0]
                .reminders
                .recovery
                .snapshot
                .is_some()
        );
        app.withdraw(0);
        let _ = app.update(Message::Consent(0, true));
        // Native actor is still alive; only the withdrawn consent epoch differs.
        assert_eq!(
            app.workspaces[0].panes.get_mut(focused).unwrap().tabs[0]
                .recipient
                .as_mut()
                .unwrap()
                .resolve(app.registry.as_deref().unwrap())
                .unwrap(),
            &scope.actor
        );
        let _ = app.update(Message::RecoveryLoaded(
            scope.clone(),
            Ok(recovery_snapshot(task)),
        ));
        assert!(
            app.workspaces[0].panes.get(focused).unwrap().tabs[0]
                .reminders
                .recovery
                .snapshot
                .is_none()
        );
        let mut rebound = scope.clone();
        rebound.epoch = app.workspaces[0].panes.get(focused).unwrap().tabs[0]
            .reminders
            .recovery
            .epoch;
        rebound.query.project = "other-project".into();
        let _ = app.update(Message::RecoveryLoaded(
            rebound,
            Ok(recovery_snapshot(task)),
        ));
        assert!(
            app.workspaces[0].panes.get(focused).unwrap().tabs[0]
                .reminders
                .recovery
                .snapshot
                .is_none()
        );
        let mut wrong_actor = scope.clone();
        wrong_actor.epoch = app.workspaces[0].panes.get(focused).unwrap().tabs[0]
            .reminders
            .recovery
            .epoch;
        wrong_actor.actor.session_id = Uuid::new_v4();
        let _ = app.update(Message::RecoveryLoaded(
            wrong_actor,
            Ok(recovery_snapshot(task)),
        ));
        assert!(
            app.workspaces[0].panes.get(focused).unwrap().tabs[0]
                .reminders
                .recovery
                .snapshot
                .is_none()
        );
        assert!(app.ledger.as_ref().unwrap().records().is_empty());
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn transport_receipt_requires_a_fresh_query_and_never_claims_or_queues_input() {
        let (_directory, mut app, scope) = recovery_fixture();
        let task = Uuid::new_v4();
        let _ = app.update(Message::RecoveryLoaded(
            scope.clone(),
            Ok(recovery_snapshot(task)),
        ));
        let _ = app.update(Message::ReceiptReported(scope, task, Ok(())));
        let tab = &app.workspaces[0].panes.iter().next().unwrap().1.tabs[0];
        assert!(tab.reminders.recovery.snapshot.is_none());
        assert!(tab.reminders.recovery.queried_at.is_none());
        assert!(
            tab.reminders
                .status
                .contains("Agent acknowledgment and execution remain unknown")
        );
        assert!(app.ledger.as_ref().unwrap().records().is_empty());
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn recovery_expiry_pauses_and_withdrawal_prevents_same_generation_replay() {
        let (_directory, mut app, scope) = recovery_fixture();
        let task = Uuid::new_v4();
        let _ = app.update(Message::RecoveryLoaded(
            scope.clone(),
            Ok(recovery_snapshot(task)),
        ));
        let focused = app.workspaces[0].focused;
        app.workspaces[0].panes.get_mut(focused).unwrap().tabs[0]
            .reminders
            .recovery
            .snapshot
            .as_mut()
            .unwrap()
            .queried_at = Instant::now() - Duration::from_secs(16);
        let _ = app.delivery_tick();
        assert!(app.ledger.as_ref().unwrap().records().is_empty());
        let _ = app.update(Message::RecoveryLoaded(
            scope.clone(),
            Ok(recovery_snapshot(task)),
        ));
        let _ = app.delivery_tick();
        assert_eq!(app.ledger.as_ref().unwrap().records().len(), 1);
        assert_eq!(
            app.ledger.as_ref().unwrap().records()[0].state,
            DeliveryState::Pending,
            "unsupported fixture prompt cannot attempt input"
        );
        app.withdraw(0);
        let _ = app.update(Message::Consent(0, true));
        let mut fresh = scope;
        fresh.epoch = app.workspaces[0].panes.get(focused).unwrap().tabs[0]
            .reminders
            .recovery
            .epoch;
        let _ = app.update(Message::RecoveryLoaded(fresh, Ok(recovery_snapshot(task))));
        let _ = app.delivery_tick();
        assert_eq!(app.ledger.as_ref().unwrap().records().len(), 1);
        assert_eq!(
            app.ledger.as_ref().unwrap().records()[0].state,
            DeliveryState::Retired
        );
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn exited_terminal_retains_diagnostic_text_and_releases_input() {
        use iced::futures::StreamExt;
        let mut app = desktop();
        let id = 0;
        let mut terminal = Terminal::new(
            id,
            iced_term::settings::Settings {
                backend: iced_term::settings::BackendSettings {
                    program: "/bin/sh".into(),
                    args: vec![
                        "-c".into(),
                        "printf 'FINAL_EXIT_DIAGNOSTIC e\\314\\201\\r\\n' >&2; exit 17".into(),
                    ],
                    ..Default::default()
                },
                ..Default::default()
            },
        )
        .unwrap();
        terminal.handle(iced_term::Command::ProxyToBackend(
            iced_term::BackendCommand::Resize(Some(iced::Size::new(800.0, 400.0)), None),
        ));
        let mut events = iced::advanced::subscription::into_recipes(terminal.subscription())
            .pop()
            .unwrap()
            .stream(iced::futures::stream::empty().boxed());
        let focused = app.workspaces[0].focused;
        let tab = &mut app.workspaces[0].panes.get_mut(focused).unwrap().tabs[0];
        tab.terminal = Some(terminal);
        tab.reminders.consent = true;
        let runtime = tokio::runtime::Runtime::new().unwrap();
        let deadline = Instant::now() + Duration::from_secs(8);
        while app.workspaces[0].panes.get(focused).unwrap().tabs[0]
            .terminal
            .is_some()
        {
            let event = runtime
                .block_on(async { tokio::time::timeout_at(deadline.into(), events.next()).await });
            let event = event
                .expect("terminal did not exit")
                .expect("terminal stream ended before exit");
            let _ = app.update(Message::Terminal(event));
        }
        let tab = &app.workspaces[0].panes.get(focused).unwrap().tabs[0];
        let output = tab
            .stopped_output
            .as_ref()
            .expect("exit diagnostic was discarded");
        assert!(
            output.contains("FINAL_EXIT_DIAGNOSTIC e\u{301}"),
            "{output:?}"
        );
        assert!(output.chars().count() <= 32_768);
        assert!(tab.recipient.is_none());
        assert!(!tab.reminders.consent);
        assert!(tab.title.ends_with(" · exited"));
        assert!(
            app.workspaces[1]
                .panes
                .iter()
                .all(|(_, pane)| pane.tabs[0].stopped_output.is_none())
        );
    }

    #[test]
    fn notification_failure_does_not_replace_gateway_acceptance_or_terminal_status() {
        struct Unavailable;
        impl amux::notifications::Backend for Unavailable {
            fn show(&mut self, _: &str, _: &str) -> Result<(), String> {
                Err("fixture notification service unavailable".into())
            }
        }
        let mut app = desktop();
        assert!(!app.notify_exits);
        assert!(app.notifier.is_none());
        let task = Uuid::new_v4();
        app.workspaces[0].gateway.request = Some(TaskRequest {
            id: Uuid::new_v4(),
            project: "project-0".into(),
            title: "Accepted independently".into(),
            state: TaskRequestState::Accepted(task),
        });
        app.status = "terminal status preserved".into();
        let mut notifier = Notifier::new(Unavailable);
        notifier.notify("local process exited", "fixture").unwrap();
        app.notifier = Some(notifier);
        let deadline = Instant::now() + Duration::from_secs(2);
        loop {
            let _ = app.update(Message::DeliveryTick);
            if app
                .notification_status
                .contains("fixture notification service unavailable")
            {
                break;
            }
            assert!(
                Instant::now() < deadline,
                "notification result not observed"
            );
            std::thread::sleep(Duration::from_millis(5));
        }
        assert_eq!(app.status, "terminal status preserved");
        assert!(
            matches!(app.workspaces[0].gateway.request.as_ref().unwrap().state,
            TaskRequestState::Accepted(id) if id == task)
        );
        let _ = app.update(Message::LocalNotifications(false));
        assert!(!app.notify_exits);
        assert!(app.workspaces[1].gateway.request.is_none());
    }

    #[test]
    fn task_acceptance_receipt_survives_project_and_workspace_changes() {
        let mut app = desktop();
        let scope = app.scope(0);
        let request_id = Uuid::new_v4();
        let accepted = Uuid::new_v4();
        app.workspaces[0].gateway.draft_title = "Original request".into();
        app.workspaces[0].gateway.draft_context = text_editor::Content::with_text("line 1\nline 2");
        app.workspaces[0].gateway.request = Some(TaskRequest {
            id: request_id,
            project: scope.project.clone(),
            title: "Original request".into(),
            state: TaskRequestState::Sending,
        });
        let _ = app.update(Message::SubmitTask);
        let _ = app.update(Message::NewTaskRequest);
        assert_eq!(
            app.workspaces[0].gateway.request.as_ref().unwrap().id,
            request_id,
            "a pending request cannot be resubmitted or discarded"
        );
        let _ = app.update(Message::GatewayProject("replacement".into()));
        let _ = app.update(Message::SelectWorkspace(1));
        let _ = app.update(Message::TaskCreated(
            scope.clone(),
            request_id,
            Ok(accepted),
        ));
        let panel = &app.workspaces[0].gateway;
        assert_eq!(panel.project, "replacement");
        let receipt = panel.request.as_ref().unwrap();
        assert_eq!(receipt.project, "project-0");
        assert!(matches!(receipt.state, TaskRequestState::Accepted(id) if id == accepted));
        assert_eq!(panel.draft_context.text(), "line 1\nline 2");
        assert!(app.workspaces[1].gateway.request.is_none());
        let _ = app.update(Message::TaskCreated(
            scope.clone(),
            request_id,
            Err("late failure".into()),
        ));
        assert!(
            matches!(app.workspaces[0].gateway.request.as_ref().unwrap().state,
            TaskRequestState::Accepted(id) if id == accepted)
        );

        let _ = app.update(Message::SelectWorkspace(0));
        let _ = app.update(Message::NewTaskRequest);
        assert!(app.workspaces[0].gateway.draft_title.is_empty());
        assert!(app.workspaces[0].gateway.draft_context.text().is_empty());
        let newer = Uuid::new_v4();
        app.workspaces[0].gateway.request = Some(TaskRequest {
            id: newer,
            project: "replacement".into(),
            title: "A different request".into(),
            state: TaskRequestState::Sending,
        });
        let _ = app.update(Message::TaskCreated(scope, request_id, Ok(accepted)));
        assert!(
            matches!(
                app.workspaces[0].gateway.request.as_ref().unwrap().state,
                TaskRequestState::Sending
            ),
            "old replies cannot acknowledge a different request"
        );
    }

    #[test]
    fn unconfirmed_task_submission_is_not_retried_or_hidden_by_refresh() {
        let mut app = desktop();
        let scope = app.scope(0);
        let request_id = Uuid::new_v4();
        app.workspaces[0].gateway.draft_title = "Preserve handoff".into();
        app.workspaces[0].gateway.request = Some(TaskRequest {
            id: request_id,
            project: scope.project.clone(),
            title: "Preserve handoff".into(),
            state: TaskRequestState::Sending,
        });
        let _ = app.update(Message::TaskCreated(
            scope.clone(),
            request_id,
            Err("connection lost".into()),
        ));
        let _ = app.update(Message::TasksLoaded(scope, Ok(json!([]))));
        let _ = app.update(Message::SubmitTask);
        let panel = &app.workspaces[0].gateway;
        assert_eq!(panel.draft_title, "Preserve handoff");
        let receipt = panel.request.as_ref().unwrap();
        assert_eq!(receipt.id, request_id);
        assert!(
            matches!(&receipt.state, TaskRequestState::Unconfirmed(error) if error == "connection lost")
        );
        let _ = app.update(Message::NewTaskRequest);
        let _ = app.update(Message::SubmitTask);
        assert!(app.workspaces[0].gateway.request.is_none());
        assert!(
            app.workspaces[0]
                .gateway
                .draft_status
                .starts_with("Not submitted:")
        );
    }

    #[test]
    fn task_drafts_are_bounded_before_any_gateway_request() {
        let mut app = desktop();
        let _ = app.update(Message::TaskTitle(" x ".into()));
        assert_eq!(app.workspaces[0].gateway.draft_title, " x ");
        let _ = app.update(Message::TaskTitle("x".repeat(1025)));
        assert_eq!(app.workspaces[0].gateway.draft_title, " x ");
        app.workspaces[0].gateway.draft_context =
            text_editor::Content::with_text(&"x".repeat(64 * 1024 + 1));
        let _ = app.update(Message::SubmitTask);
        assert!(app.workspaces[0].gateway.request.is_none());
        assert!(
            app.workspaces[0]
                .gateway
                .draft_status
                .starts_with("Not submitted:")
        );
    }

    #[test]
    fn delayed_query_keeps_its_workspace_and_rejects_rebound_project() {
        let mut app = desktop();
        let first = app.scope(0);
        let task = Uuid::new_v4();
        let _ = app.update(Message::SelectWorkspace(1));
        let _ = app.update(Message::TasksLoaded(
            first.clone(),
            Ok(json!([
                {"id": task, "status": "todo", "title": "first workspace"}
            ])),
        ));
        assert!(app.workspaces[0].gateway.pending_task(task));
        assert!(app.workspaces[1].gateway.tasks.is_empty());
        let _ = app.update(Message::SelectWorkspace(0));
        let _ = app.update(Message::GatewayProject("replacement".into()));
        app.workspaces[0].gateway.loading = true;
        let _ = app.update(Message::TasksLoaded(
            first,
            Ok(json!([
                {"id": task, "status": "todo"}
            ])),
        ));
        assert!(
            app.workspaces[0].gateway.loading,
            "stale result must not finish a newer query"
        );
        assert!(app.workspaces[0].gateway.tasks.is_empty());
        assert!(app.workspaces[0].gateway.loaded_at.is_none());
        let mut wrong_directory = app.scope(0);
        wrong_directory.directory = PathBuf::from("/different-project");
        assert!(!app.scope_current(&wrong_directory));
    }

    #[test]
    fn failed_expired_and_changed_task_queries_cannot_supply_pending_work() {
        let mut app = desktop();
        let task = Uuid::new_v4();
        let scope = app.scope(0);
        let _ = app.update(Message::TasksLoaded(
            scope.clone(),
            Ok(json!([
                {"id": task, "status": "todo"}
            ])),
        ));
        assert!(app.workspaces[0].gateway.pending_task(task));
        app.workspaces[0].gateway.loaded_at = Some(Instant::now() - Duration::from_secs(16));
        assert!(!app.workspaces[0].gateway.pending_task(task));
        let _ = app.update(Message::TasksLoaded(
            scope.clone(),
            Ok(json!([
                {"id": task, "status": "in_progress"}
            ])),
        ));
        assert!(
            !app.workspaces[0].gateway.pending_task(task),
            "claimed recovery needs separate provenance"
        );
        let _ = app.update(Message::TasksLoaded(
            scope.clone(),
            Ok(json!([
                {"id": task, "status": "todo", "owner_origin": {"session_id": Uuid::new_v4()}}
            ])),
        ));
        assert!(
            !app.workspaces[0].gateway.pending_task(task),
            "inconsistent ownership must defer"
        );
        let _ = app.update(Message::TasksLoaded(scope.clone(), Err("offline".into())));
        assert!(!app.workspaces[0].gateway.fresh());
        assert!(app.workspaces[0].gateway.tasks.is_empty());
        let _ = app.update(Message::TasksLoaded(scope, Ok(json!({"tasks": []}))));
        assert!(!app.workspaces[0].gateway.fresh());
    }

    #[test]
    fn consent_is_ephemeral_and_project_rebinding_withdraws_only_that_workspace() {
        let mut app = desktop();
        for workspace in &app.workspaces {
            assert!(
                !workspace.panes.get(workspace.focused).unwrap().tabs[0]
                    .reminders
                    .consent
            );
        }
        for workspace in &mut app.workspaces {
            workspace.panes.get_mut(workspace.focused).unwrap().tabs[0]
                .reminders
                .consent = true;
        }
        let _ = app.update(Message::GatewayProject("replacement".into()));
        assert!(
            !app.workspaces[0]
                .panes
                .get(app.workspaces[0].focused)
                .unwrap()
                .tabs[0]
                .reminders
                .consent
        );
        assert!(
            app.workspaces[1]
                .panes
                .get(app.workspaces[1].focused)
                .unwrap()
                .tabs[0]
                .reminders
                .consent
        );
        let _ = app.update(Message::Consent(1, false));
        assert!(
            !app.workspaces[1]
                .panes
                .get(app.workspaces[1].focused)
                .unwrap()
                .tabs[0]
                .reminders
                .consent
        );
    }
}
