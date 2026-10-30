//! Provisional native terminal/UI candidate. Agent work is never restarted implicitly.
use amux::gateway_preferences::{EnrolledConnection, GatewayMode, GatewayPreferences};
use amux::identity::Provider;
use amux::notifications::{Notifier, Outcome};
use amux::server_controller::{self, LocalServer};
use amux::session::{Layout, SavedTab, SavedWorkspace, Snapshot};
#[path = "desktop_delegation.rs"]
mod desktop_delegation;
#[path = "desktop_design.rs"]
mod desktop_design;
#[path = "desktop_harness.rs"]
mod desktop_harness;
#[path = "desktop_icon.rs"]
mod desktop_icon;
#[path = "desktop_routing.rs"]
mod desktop_routing;
#[path = "desktop_tasks.rs"]
mod desktop_tasks;
#[path = "desktop_toasts.rs"]
mod desktop_toasts;
#[path = "desktop_workspace.rs"]
mod desktop_workspace;
use desktop_workspace::{WorkspaceDialog, WorkspaceMessage};
use iced::widget::{
    button, checkbox, column, container, pane_grid, row, scrollable, svg, text, text_editor,
    text_input, tooltip,
};
use iced::{Element, Length, Subscription, Task, Theme};
use iced_term::{Terminal, TerminalView};
use std::path::PathBuf;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use uuid::Uuid;

const RELEASE_TASK_CONTEXT: &str = "Prepare and publish a new release for this project using its documented release process.\n\n\
Honor the additional notes below, including any changes or tickets that must land before releasing.\n\n\
Verify that all CI for the recent changes and final release commit is green; wait for pending checks and resolve failures before tagging. Update all required versions consistently, including manifests, lockfiles, containers and packager metadata where applicable.\n\n\
Once verified, cut the new release, publish it with an accurate changelog, and verify that release builds and publishing complete successfully with the expected artifacts. Keep this task in progress until verification is complete.\n\n\
Additional notes / directives (optional):\n";

#[derive(Debug, Clone)]
enum Message {
    ManageTask(desktop_tasks::Action),
    TaskDetailLoaded(QueryScope, Uuid, Uuid, Result<serde_json::Value, String>),
    TaskChanged(
        QueryScope,
        Uuid,
        Uuid,
        desktop_tasks::Change,
        Result<serde_json::Value, String>,
    ),
    Design(desktop_design::Action),
    Escape,
    /// Tab not consumed by a widget moves focus; `true` goes backwards.
    FocusNext(bool),
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
    /// Exit an agent tab's provider and relaunch the same session in place.
    RestartTab(pane_grid::Pane, u64),
    Preferences(bool),
    PreferencesTab(PreferencesTab),
    CheckHarness,
    InstallHarness(amux::harness::Provider),
    ManualHarness(amux::harness::Provider),
    CopyHarnessRules,
    HarnessChecked(Result<desktop_harness::Checked, String>),
    DismissToast(u64),
    DismissHarnessNotice,
    WindowReady,
    SavePreferences,
    GatewayMode(GatewayMode),
    NetworkHosting(bool),
    ServerName(String),
    PairAddress(String),
    PairInvitation(Secret),
    PairConnect,
    PairConnected(u64, Result<EnrolledConnection, String>),
    ServerReady(u64, Result<LocalServer, String>),
    GenerateInvitation,
    InvitationReady(u64, bool, Result<PeeringInvitation, String>),
    CopyInvitation,
    ServerStopped(ServerExit, Result<(), String>),
    ServerReconfigured(u64, Result<(), String>),
    AutomaticNotifications(bool),
    AutoResume(bool),
    ExitNotifications(bool),
    AutoUpdate(bool),
    ProjectsDir(String),
    Delegation(u64, desktop_delegation::Event),
    Updates(bool),
    CheckUpdates,
    UpdatesChecked(Result<Box<amux::updates::UpdateCheck>, String>),
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
    GatewayStats(Result<amux::gateway::PresenceSummary, String>),
    ProjectsLoaded(u64, Result<Vec<amux::gateway::ProjectOverview>, String>),
    RoutingLoaded(
        QueryScope,
        Result<(amux::task_routing::Directory, Option<String>), String>,
    ),
    TaskTarget(desktop_routing::Choice),
    TaskTitle(String),
    TaskContext(text_editor::Action),
    SubmitTask,
    PrepareReleaseTask,
    NewTaskRequest,
    TaskCreated(QueryScope, Uuid, Result<Uuid, String>),
    #[cfg(test)]
    LocalNotifications(bool),
    DeliveryTick,
    Save,
    Close(iced::window::Id),
}

#[derive(Clone)]
struct Secret(String);
impl std::fmt::Debug for Secret {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("[redacted]")
    }
}
#[derive(Clone)]
struct PeeringInvitation {
    value: Secret,
    expires_at: i64,
    deadline: Instant,
}
impl std::fmt::Debug for PeeringInvitation {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("[redacted peering invitation]")
    }
}
#[derive(Debug, Clone, Copy)]
enum ServerExit {
    Close,
    Restart,
}
#[derive(Default)]
struct EmbeddedServer {
    local: Option<LocalServer>,
    busy: bool,
    epoch: u64,
    invitation: Option<PeeringInvitation>,
    pair_address: String,
    pair_invitation: String,
    status: String,
    stopping: bool,
}

// A new install may start a local server. Failed settings must stay
// disconnected until the user chooses a connection, preserving the saved file.
fn startup_connection(
    saved: anyhow::Result<Option<GatewayPreferences>>,
    environment: GatewayPreferences,
) -> (GatewayPreferences, String) {
    match saved {
        Ok(saved) => (saved.unwrap_or(environment), String::new()),
        Err(error) => (
            GatewayPreferences {
                mode: GatewayMode::Client,
                ..Default::default()
            },
            format!("{error}. Save corrected preferences to replace them."),
        ),
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
    Harness,
    Templates,
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

#[derive(Default)]
struct TaskPanel {
    detail: desktop_tasks::Panel,
    routing: desktop_routing::Panel,
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

impl TaskPanel {
    fn can_prepare_release(&self) -> bool {
        self.request.is_none()
            && self.draft_title.is_empty()
            && self.draft_context.text().is_empty()
    }
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
struct Tab {
    id: u64,
    title: String,
    // Fields drop in declaration order: request owned-job retirement before the PTY drops.
    #[cfg(any(target_os = "linux", windows))]
    lifetime: Option<amux::terminal_lifetime::TerminalLifetime>,
    terminal: Option<Terminal>,
    stopped_output: Option<String>,
    provider: Option<Provider>,
    delegation: desktop_delegation::TabState,
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
    embedded: EmbeddedServer,
    updates: Updates,
    harness: desktop_harness::State,
    toasts: desktop_toasts::State,
    workspaces: Vec<Workspace>,
    active: usize,
    workspace_dialog: WorkspaceDialog,
    closing_workspace: Option<Uuid>,
    next_id: u64,
    status: String,
    registry: Option<PathBuf>,
    snapshot_path: Option<PathBuf>,
    dirty: bool,
    restarting: bool,
    notify_exits: bool,
    notifier: Option<Notifier>,
    notification_status: String,
    delegation: desktop_delegation::Bridge,
    client_run: Uuid,
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
        // Linux Wayland/NVIDIA FIFO presentation can leave a frame callback
        // pending until pointer activity. Keep this event-driven UI responsive
        // without waiting for synchronized GPU presentation.
        vsync: !cfg!(target_os = "linux"),
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
            ..desktop_icon::settings()
        });
        #[cfg(target_os = "linux")]
        if let Err(error) = desktop_icon::install() {
            eprintln!("amux desktop icon: {error:#}");
        }
        let state = amux::identity::state_directory();
        let state_error = state.as_ref().err().map(ToString::to_string);
        let preferences_path = state
            .as_ref()
            .ok()
            .map(|p| p.join("gateway-preferences.json"));
        let loaded = preferences_path
            .as_deref()
            .map(GatewayPreferences::load)
            .transpose();
        let (preferences, preferences_error) = startup_connection(
            loaded.map(Option::flatten),
            GatewayPreferences::from_environment(),
        );
        let mut app = Self {
            design: desktop_design::State::default(),
            main_window,
            updates: Updates::new(),
            embedded: EmbeddedServer::default(),
            harness: desktop_harness::State::startup(),
            toasts: desktop_toasts::State::default(),
            preferences: Preferences { draft: preferences.clone(), current: preferences, path: preferences_path, open: false, tab: PreferencesTab::Gateway, status: preferences_error },
            workspaces: Vec::new(), active: 0, workspace_dialog: WorkspaceDialog::default(), closing_workspace: None, next_id: 0,
            status: "Configure the gateway in Preferences to receive task notifications in agent terminals.".into(),
            registry: state.ok(),
            snapshot_path: amux::identity::state_directory().ok().map(|p| p.join("layout.json")), dirty: false, restarting: false,
            notify_exits: false, notifier: None, notification_status: String::new(),
            delegation: desktop_delegation::Bridge::default(),
            client_run: Uuid::new_v4(),
        };
        app.notify_exits = app.preferences.current.notify_exits;
        if app.notify_exits {
            app.notifier = Some(Notifier::native());
        }
        match app.snapshot_path.as_deref().map(Snapshot::load).transpose() {
            Ok(Some(Some(saved))) => {
                app.active = saved.active;
                app.workspace_dialog.last_directory = saved.workspace_browser_directory;
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
        if let Some(error) = state_error {
            app.status =
                format!("App state unavailable: {error}. Local terminals remain available.");
        }
        let focus = app.focus();
        let updates = app.check_updates();
        let gateways = if app.preferences.current.mode == GatewayMode::Server {
            app.start_local_server()
        } else if app.preferences.current.has_connection() {
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
            workspace_browser_directory: self.workspace_dialog.last_directory.clone(),
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

    // Native --last/--continue is shared by tabs in the same directory. Only
    // the newest tab may use it; older tabs start fresh and recover via tasks.
    fn resume_restarted_tab(
        &self,
        id: u64,
        provider: Provider,
        directory: &std::path::Path,
    ) -> bool {
        let directory = directory
            .canonicalize()
            .unwrap_or_else(|_| directory.into());
        !self.workspaces.iter().any(|workspace| {
            workspace.panes.iter().any(|(_, pane)| {
                pane.tabs.iter().any(|tab| {
                    if tab.id <= id || tab.delegation.provider.or(tab.provider) != Some(provider) {
                        return false;
                    }
                    let other_directory = tab
                        .terminal
                        .as_ref()
                        .and_then(|terminal| {
                            amux::session::working_directory(terminal.child_pid()?)
                        })
                        .or_else(|| tab.delegation.directory.clone())
                        .unwrap_or_else(|| workspace.directory.clone());
                    other_directory.canonicalize().unwrap_or(other_directory) == directory
                })
            })
        })
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
                }
            }
            Err(error) => {
                self.status = format!("Terminal launch failed in {}: {error}", directory.display());
                Tab {
                    id,
                    title: format!("{title} — launch failed"),
                    #[cfg(any(target_os = "linux", windows))]
                    lifetime: None,
                    terminal: None,
                    stopped_output: None,
                    provider,
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
        if (self.embedded.busy || self.embedded.stopping)
            && matches!(
                &message,
                Message::ManageTask(_) | Message::SubmitTask | Message::RefreshTasks
            )
        {
            return Task::none();
        }
        if matches!(
            &message,
            Message::NewWorkspace
                | Message::SelectWorkspace(_)
                | Message::NewTab(_)
                | Message::SelectTab(..)
                | Message::Split(_)
                | Message::CloseTab(..)
                | Message::RestartTab(..)
                | Message::Resize(_)
        ) {
            self.dismiss_tab_menu();
            self.dirty = true;
        }
        match message {
            Message::ManageTask(action) => return self.manage_task(action),
            Message::TaskDetailLoaded(scope, id, request, result) => {
                self.task_detail_loaded(scope, id, request, result)
            }
            Message::TaskChanged(scope, id, request, change, result) => {
                return self.task_changed(scope, id, request, change, result);
            }
            Message::GatewayStats(result) => self.design.gateway_stats = result.ok(),
            Message::ProjectsLoaded(epoch, result) => self.projects_loaded(epoch, result),
            Message::RoutingLoaded(scope, result) => self.routing_loaded(scope, result),
            Message::TaskTarget(choice) => {
                let panel = &mut self.workspaces[self.active].gateway;
                if panel.request.is_none() {
                    panel.routing.choice = choice;
                    panel.draft_status.clear();
                }
            }
            Message::WindowResized(id, size) => {
                if id == self.main_window {
                    self.design_viewport(size);
                    self.set_compact(size.width < 1400.0);
                }
            }
            Message::FocusNext(back) => {
                return if back {
                    iced::widget::operation::focus_previous()
                } else {
                    iced::widget::operation::focus_next()
                };
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
            Message::AutoUpdate(value) => self.preferences.draft.auto_update = value,
            Message::ProjectsDir(value) => self.preferences.draft.projects_dir = value,
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
            Message::RestartTab(pane_id, terminal_id) => {
                let workspace = &self.workspaces[self.active];
                let Some((index, tab)) = workspace.panes.get(pane_id).and_then(|pane| {
                    pane.tabs
                        .iter()
                        .enumerate()
                        .find(|(_, t)| t.id == terminal_id)
                }) else {
                    return Task::none();
                };
                let Some(provider) = tab.delegation.provider.or(tab.provider) else {
                    self.status = "Only Codex or Claude sessions can be restarted".into();
                    return Task::none();
                };
                let number = Some(tab.delegation.terminal_number).filter(|n| *n > 0);
                let directory = tab
                    .terminal
                    .as_ref()
                    .and_then(|terminal| amux::session::working_directory(terminal.child_pid()?))
                    .or_else(|| tab.delegation.directory.clone())
                    .unwrap_or_else(|| workspace.directory.clone());
                let title = tab.title.clone();
                let resume = self.resume_restarted_tab(terminal_id, provider, &directory);
                // Keep the assigned agent number, but retire the runtime event ID.
                // Iced retains a subscription by that ID across one update; reusing
                // it keeps the old receiver and lets delayed old Exit events stop
                // the replacement. A fresh ID fences both subscription and callbacks.
                let replacement_id = self.next_id;
                self.next_id += 1;
                let pane = self.workspaces[self.active]
                    .panes
                    .get_mut(pane_id)
                    .expect("pane exists");
                drop(pane.tabs.remove(index));
                let mut tab = self.open_terminal(
                    replacement_id,
                    &directory,
                    Some(provider),
                    None,
                    resume,
                    number,
                );
                if tab.terminal.is_some() {
                    tab.title = title;
                } else {
                    // A failed relaunch stays on the same session for a later retry.
                    tab.delegation.terminal_number = number.unwrap_or_default();
                }
                let pane = self.workspaces[self.active]
                    .panes
                    .get_mut(pane_id)
                    .expect("pane exists");
                pane.tabs.insert(index, tab);
                pane.active = index;
                return self.focus();
            }
            Message::Preferences(open) => {
                return if open {
                    self.open_preferences(PreferencesTab::Gateway)
                } else {
                    self.close_preferences()
                };
            }
            Message::PreferencesTab(tab) => {
                if !self.preferences.open {
                    let focus = self.open_preferences(tab);
                    return if tab == PreferencesTab::Harness {
                        Task::batch([focus, self.check_harness(desktop_harness::Action::Check)])
                    } else {
                        focus
                    };
                }
                self.preferences.tab = tab;
                if tab == PreferencesTab::Harness {
                    return self.check_harness(desktop_harness::Action::Check);
                }
            }
            Message::CheckHarness => return self.check_harness(desktop_harness::Action::Check),
            Message::InstallHarness(provider) => {
                return self.check_harness(desktop_harness::Action::Automatic(provider));
            }
            Message::ManualHarness(provider) => {
                let focus = self.open_preferences(PreferencesTab::Harness);
                return Task::batch([
                    focus,
                    self.check_harness(desktop_harness::Action::Manual(provider)),
                ]);
            }
            Message::CopyHarnessRules => {
                if let Some(manual) = &self.harness.manual {
                    return iced::clipboard::write(manual.clone());
                }
            }
            Message::HarnessChecked(result) => {
                self.harness.busy = false;
                match result {
                    Ok(checked) => {
                        let title = if checked.reports.iter().any(|report| report.error.is_some()) {
                            "Agent rules need attention"
                        } else {
                            "Agent instructions"
                        };
                        self.toasts.show("harness", title, &checked.status);
                        self.harness.apply(checked);
                    }
                    Err(error) => {
                        self.toasts
                            .show("harness", "Agent rules need attention", &error);
                        self.harness.status = error;
                    }
                }
            }
            Message::DismissToast(id) => self.toasts.dismiss(id),
            Message::DismissHarnessNotice => self.harness.notice_dismissed = true,
            Message::SavePreferences => return self.save_preferences(),
            Message::GatewayMode(mode) => {
                if !self.embedded.busy && !self.embedded.stopping {
                    if mode == GatewayMode::Client
                        && self.preferences.draft.mode == GatewayMode::Server
                    {
                        self.preferences.draft.enrolled = None;
                    }
                    self.preferences.draft.mode = mode;
                    self.embedded.invitation = None;
                    self.embedded.pair_invitation.clear();
                }
            }
            Message::NetworkHosting(value) => {
                if !self.embedded.busy && !self.embedded.stopping {
                    self.preferences.draft.server.network_hosting = value;
                }
            }
            Message::ServerName(value) => {
                if value.len() <= 253 && !self.embedded.busy && !self.embedded.stopping {
                    self.preferences.draft.server.server_name = value;
                }
            }
            Message::PairAddress(value) => {
                if value.len() <= 2048 {
                    self.embedded.pair_address = value;
                }
            }
            Message::PairInvitation(Secret(value)) => {
                if value.len() <= 16 * 1024 {
                    self.embedded.pair_invitation = value;
                }
            }
            Message::PairConnect => return self.pair_client(),
            Message::PairConnected(epoch, result) => {
                if epoch != self.embedded.epoch {
                    return Task::none();
                }
                self.embedded.busy = false;
                match result {
                    Ok(connection) => {
                        self.preferences.draft.mode = GatewayMode::Client;
                        self.preferences.draft.enrolled = Some(connection);
                        self.embedded.pair_invitation.clear();
                        return self.save_preferences();
                    }
                    Err(error) => self.embedded.status = format!("Pairing failed: {error}"),
                }
            }
            Message::ServerReady(epoch, result) => {
                if epoch != self.embedded.epoch {
                    if let Ok(server) = result {
                        return Task::perform(
                            async move {
                                let _ = server.stop().await;
                            },
                            |_| Message::Save,
                        );
                    }
                    return Task::none();
                }
                self.embedded.busy = false;
                match result {
                    Ok(server) => {
                        let connection = server.connection();
                        self.preferences.current.enrolled = Some(connection.clone());
                        self.preferences.draft.enrolled = Some(connection.clone());
                        self.embedded.local = Some(server);
                        self.embedded.status = format!(
                            "Server ready at {}",
                            self.preferences
                                .current
                                .server
                                .client_url()
                                .unwrap_or_else(|_| "configured endpoint".into())
                        );
                        if !self.preferences.status.starts_with("Not saved:") {
                            self.status = self.embedded.status.clone();
                        }
                        let saved = self
                            .preferences
                            .path
                            .as_deref()
                            .ok_or_else(|| {
                                anyhow::anyhow!("Application settings directory unavailable")
                            })
                            .and_then(|path| self.preferences.current.save(path));
                        if let Err(error) = saved {
                            self.embedded.status = format!(
                                "Server ready; connection settings could not be saved: {error}"
                            );
                            self.status = self.embedded.status.clone();
                        }
                        let queries = Task::batch(
                            (0..self.workspaces.len())
                                .map(|i| self.query(i))
                                .collect::<Vec<_>>(),
                        );
                        return Task::batch([queries, self.close_preferences()]);
                    }
                    Err(error) => {
                        self.embedded.status = format!("Server unavailable: {error}");
                        self.status = self.embedded.status.clone();
                    }
                }
            }
            Message::GenerateInvitation => return self.load_invitation(true),
            Message::InvitationReady(epoch, generate, result) => {
                if epoch != self.embedded.epoch {
                    return Task::none();
                }
                self.embedded.busy = false;
                match result {
                    Ok(invitation) => {
                        let copy = (!generate).then(|| invitation.value.0.clone());
                        self.embedded.invitation = Some(invitation);
                        self.embedded.status = if generate {
                            "Peering key generated. Copy it to connect a client."
                        } else {
                            "Current peering key copied"
                        }
                        .into();
                        if let Some(value) = copy {
                            return iced::clipboard::write(value);
                        }
                    }
                    Err(error) => {
                        self.embedded.invitation = None;
                        self.embedded.status = format!(
                            "Could not {} peering key: {error}. Generate a new key if the server restarted or the key expired.",
                            if generate { "generate" } else { "copy" }
                        );
                    }
                }
            }
            Message::CopyInvitation => return self.load_invitation(false),
            Message::ServerReconfigured(epoch, result) => {
                if epoch != self.embedded.epoch {
                    return Task::none();
                }
                self.embedded.busy = false;
                match result {
                    Ok(()) => {
                        self.embedded.local = None;
                        return self.save_preferences();
                    }
                    Err(error) => {
                        self.embedded.status = format!("Could not stop previous server: {error}")
                    }
                }
            }
            Message::ServerStopped(action, result) => {
                self.embedded.stopping = false;
                match result {
                    Ok(()) => {
                        self.embedded.local = None;
                        return match action {
                            ServerExit::Close => self.close_window(self.main_window),
                            ServerExit::Restart => self.restart_updated(),
                        };
                    }
                    Err(error) => {
                        // The app stays open, so later updates may install again.
                        if let Some(running) = &self.updates.running {
                            running.reopen();
                        }
                        self.embedded.status = format!("Could not stop server: {error}")
                    }
                }
            }
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
                let checked = result.is_ok();
                match result {
                    Ok(report) => {
                        self.updates.restart = report.restart.clone();
                        self.updates.report = Some(*report);
                        self.updates.error.clear();
                    }
                    Err(error) => {
                        self.updates.error = format!("Could not check for updates: {error}")
                    }
                }
                self.updates.refresh_restart();
                // Auto update downloads now; the bell then offers the restart.
                // A failed check leaves only the previous report; never install from it.
                if checked && self.preferences.current.auto_update {
                    return self.install_update();
                }
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
                            if let Some(terminal) = &mut tab.terminal {
                                match terminal.handle(iced_term::Command::ProxyToBackend(command)) {
                                    iced_term::actions::Action::ChangeTitle(title) => {
                                        tab.title = title
                                    }
                                    iced_term::actions::Action::Shutdown => {
                                        self.dirty = true;
                                        // Preserve only the bounded visible text, then release
                                        // the PTY and its subscription. This is never input-ready.
                                        tab.stopped_output = Some(terminal.screen_text());
                                        let exit_code = terminal.exit_code();
                                        self.status = match exit_code {
                                            Some(code) => format!(
                                                "Terminal #{id}: process exited with code {code}"
                                            ),
                                            None => format!(
                                                "Terminal #{id}: process exited; exit code unavailable"
                                            ),
                                        };
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
                                        tab.title.push_str(" · exited");
                                        if let Some(code) = exit_code {
                                            tab.title.push_str(&format!(" (code {code})"));
                                        }
                                    }
                                    _ => {}
                                }
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
            Message::PrepareReleaseTask => {
                let panel = &mut self.workspaces[self.active].gateway;
                if panel.can_prepare_release() {
                    panel.draft_title = "Prepare and publish a new release".into();
                    panel.draft_context = text_editor::Content::with_text(RELEASE_TASK_CONTEXT);
                    panel.draft_status =
                        "Review the release request and add any notes before submitting.".into();
                }
            }
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
                if panel.loaded_at.is_some() && panel.detail.refresh_with_list() {
                    return self.refresh_open_task(scope.workspace);
                }
            }
            Message::DeliveryTick => {
                self.toasts.expire();
                if self.embedded.invitation.is_some() && self.active_invitation().is_none() {
                    self.embedded.invitation = None;
                }
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
                let gateway_ticks = if self.embedded.busy || self.embedded.stopping {
                    Task::none()
                } else {
                    Task::batch([
                        self.delegation_tick(),
                        self.routing_tick(),
                        self.gateway_stats_tick(),
                        self.projects_tick(),
                    ])
                };
                return Task::batch([gateway_ticks, updates]);
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
                    .map(Box::new)
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
        if self.updates.installing
            || self.workspace_dialog.busy
            || self.embedded.busy
            || self.embedded.stopping
        {
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
        if self
            .embedded
            .local
            .as_ref()
            .is_some_and(LocalServer::owns_process)
        {
            return self.stop_local_server(ServerExit::Restart);
        }
        self.pause_for_restart();
        match start(&path, &self.workspaces[self.active].directory) {
            Ok(()) => iced::exit(),
            Err(error) => {
                self.restarting = false;
                self.updates.error = format!("Could not restart: {error}");
                if self.preferences.current.mode == GatewayMode::Server {
                    self.start_local_server()
                } else {
                    Task::none()
                }
            }
        }
    }

    fn pause_for_restart(&mut self) {
        // This exits the same logical client; preserve the snapshot just saved
        // for an older router server's next startup.
        self.delegation.stop();
        self.restarting = true;
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
        content.into()
    }

    fn open_preferences(&mut self, tab: PreferencesTab) -> Task<Message> {
        self.preferences.tab = tab;
        if !self.preferences.open {
            self.preferences.draft = self.preferences.current.clone();
        }
        self.preferences.open = true;
        if self.embedded.pair_address.is_empty()
            && self.preferences.draft.mode == GatewayMode::Client
            && let Some(connection) = &self.preferences.draft.enrolled
        {
            self.embedded.pair_address = connection.url.clone();
        }
        self.close_design_popovers();
        iced::widget::operation::focus("server-address")
    }

    fn close_preferences(&mut self) -> Task<Message> {
        if self.embedded.busy || self.embedded.stopping {
            return Task::none();
        }
        self.preferences.draft = self.preferences.current.clone();
        self.preferences.open = false;
        self.focus()
    }

    fn close_window(&mut self, window: iced::window::Id) -> Task<Message> {
        if self.design.task_window == Some(window) {
            return self.design_message(desktop_design::Action::DockTasks);
        }
        if window == self.main_window {
            if self.workspace_dialog.busy {
                self.status = "Wait for the repository clone to finish before closing amux.".into();
            } else if self.embedded.busy || self.embedded.stopping {
                self.status = "Wait for the server operation to finish before closing amux.".into();
            } else if !self.updates.running.as_ref().is_none_or(|r| r.close()) {
                // An update download is abandoned; only swapping files must finish.
                self.status =
                    "An update is installing. Wait for it to finish before closing amux.".into();
            } else {
                self.dirty = true;
                if !self.save() {
                    if let Some(running) = &self.updates.running {
                        running.reopen();
                    }
                } else {
                    if self
                        .embedded
                        .local
                        .as_ref()
                        .is_some_and(LocalServer::owns_process)
                    {
                        return self.stop_local_server(ServerExit::Close);
                    }
                    return iced::exit();
                }
            }
        }
        Task::none()
    }

    fn window_title(&self, window: iced::window::Id) -> String {
        if self.design.task_window == Some(window) {
            return format!(
                "amux · Gateway tasks · {}",
                self.workspaces[self.active].gateway.project
            );
        }
        "amux · agent workspaces".into()
    }
    fn window_view(&self, window: iced::window::Id) -> Element<'_, Message> {
        if self.design.task_window == Some(window) {
            if self.preferences.open || self.workspace_dialog.open {
                return container(text(
                    "Finish the open dialog in the main window to manage tasks.",
                ))
                .padding(20)
                .into();
            }
            return self.design_tasks(true);
        }
        self.view()
    }

    fn start_local_server(&mut self) -> Task<Message> {
        if self.embedded.busy || self.embedded.stopping {
            return Task::none();
        }
        let Some(directory) = self.registry.clone() else {
            self.embedded.status =
                "Application settings directory unavailable; server was not started.".into();
            return Task::none();
        };
        self.embedded.epoch = self.embedded.epoch.wrapping_add(1);
        let epoch = self.embedded.epoch;
        self.embedded.busy = true;
        self.embedded.invitation = None;
        self.embedded.status = "Starting local server…".into();
        let previous = self.embedded.local.take();
        let config = self.preferences.current.server.clone();
        Task::perform(
            async move {
                if let Some(server) = previous {
                    server.stop().await.map_err(|error| error.to_string())?;
                }
                server_controller::ensure_local(directory, config)
                    .await
                    .map_err(|error| error.to_string())
            },
            move |result| Message::ServerReady(epoch, result),
        )
    }

    fn stop_local_server(&mut self, action: ServerExit) -> Task<Message> {
        let Some(server) = self.embedded.local.clone() else {
            return Task::none();
        };
        self.embedded.stopping = true;
        self.embedded.invitation = None;
        // The close/restart callback saves again after shutdown. Keep the
        // observed owners and negotiated capability for that final save.
        self.events_stop();
        Task::perform(
            async move { server.stop().await.map_err(|error| error.to_string()) },
            move |result| Message::ServerStopped(action, result),
        )
    }

    fn active_invitation(&self) -> Option<&PeeringInvitation> {
        self.embedded.invitation.as_ref().filter(|invitation| {
            invitation.expires_at > unix_ms() as i64 && invitation.deadline > Instant::now()
        })
    }

    fn load_invitation(&mut self, generate: bool) -> Task<Message> {
        if self.embedded.busy
            || self.embedded.stopping
            || self.preferences.current.mode != GatewayMode::Server
            || self.embedded.local.is_none()
            || self.preferences.current.server != self.preferences.draft.server
        {
            return Task::none();
        }
        let Some(connection) = self.preferences.current.enrolled.clone() else {
            return Task::none();
        };
        self.embedded.invitation = None;
        self.embedded.busy = true;
        let epoch = self.embedded.epoch;
        Task::perform(
            async move {
                server_controller::invitation(connection, generate)
                    .await
                    .map(|value| {
                        let remaining = value
                            .expires_at
                            .saturating_sub(unix_ms() as i64)
                            .clamp(0, 600_000) as u64;
                        PeeringInvitation {
                            value: Secret(value.invitation),
                            expires_at: value.expires_at,
                            deadline: Instant::now() + Duration::from_millis(remaining),
                        }
                    })
                    .map_err(|error| error.to_string())
            },
            move |result| Message::InvitationReady(epoch, generate, result),
        )
    }

    fn pair_client(&mut self) -> Task<Message> {
        if self.embedded.busy || self.embedded.stopping || self.pending_gateway_action() {
            return Task::none();
        }
        let Some(directory) = self.registry.clone() else {
            self.embedded.status = "Application settings directory unavailable".into();
            return Task::none();
        };
        let url = self.embedded.pair_address.trim().to_owned();
        if url.is_empty() || self.embedded.pair_invitation.trim().is_empty() {
            self.embedded.status = "Enter the server address and peering key.".into();
            return Task::none();
        }
        self.reset_delegation();
        self.embedded.busy = true;
        self.embedded.epoch = self.embedded.epoch.wrapping_add(1);
        let epoch = self.embedded.epoch;
        let invitation = std::mem::take(&mut self.embedded.pair_invitation);
        self.embedded.status = "Connecting to server…".into();
        Task::perform(
            async move {
                server_controller::pair(directory, url, invitation)
                    .await
                    .map_err(|error| error.to_string())
            },
            move |result| Message::PairConnected(epoch, result),
        )
    }

    fn pending_gateway_action(&self) -> bool {
        self.workspaces.iter().any(|w| {
            w.gateway.detail.unresolved()
                || w.gateway.request.as_ref().is_some_and(|request| {
                    matches!(
                        request.state,
                        TaskRequestState::Sending | TaskRequestState::Unconfirmed(_)
                    )
                })
        })
    }

    fn save_preferences(&mut self) -> Task<Message> {
        if self.embedded.busy || self.embedded.stopping {
            return Task::none();
        }
        let mode_changed = self.preferences.current.mode != self.preferences.draft.mode;
        let server_changed = self.preferences.current.server.network_hosting
            != self.preferences.draft.server.network_hosting
            || self.preferences.current.server.server_name
                != self.preferences.draft.server.server_name
            || self.preferences.current.server.secure_port
                != self.preferences.draft.server.secure_port
            || self.preferences.current.server.discovery_port
                != self.preferences.draft.server.discovery_port;
        let authority_changed = self
            .preferences
            .current
            .enrolled
            .as_ref()
            .map(|c| (&c.cluster_id, &c.node_id, &c.url))
            != self
                .preferences
                .draft
                .enrolled
                .as_ref()
                .map(|c| (&c.cluster_id, &c.node_id, &c.url));
        let connection_changed = mode_changed
            || server_changed
            || authority_changed
            || self.preferences.current.automatic_notifications
                != self.preferences.draft.automatic_notifications;
        if connection_changed && self.pending_gateway_action() {
            self.preferences.status = "A gateway task action is pending or unconfirmed. Refresh and inspect its result before changing connections.".into();
            return Task::none();
        }
        let mut draft = self.preferences.draft.clone();
        draft.machine_key = draft.machine_key.trim().to_owned();
        draft.machine_id = draft.machine_id.trim().to_owned();
        if let Err(error) = draft.validate() {
            self.preferences.status = format!("Not saved: {error}");
            return Task::none();
        }
        if (mode_changed || server_changed)
            && let Some(server) = self.embedded.local.clone()
        {
            self.embedded.busy = true;
            self.embedded.invitation = None;
            self.embedded.epoch = self.embedded.epoch.wrapping_add(1);
            let epoch = self.embedded.epoch;
            self.reset_delegation();
            return Task::perform(
                async move { server.stop().await.map_err(|error| error.to_string()) },
                move |result| Message::ServerReconfigured(epoch, result),
            );
        }
        let result = self
            .preferences
            .path
            .as_deref()
            .ok_or_else(|| anyhow::anyhow!("Application settings directory unavailable"))
            .and_then(|path| draft.save(path));
        if let Err(error) = result {
            self.preferences.status = format!("Not saved: {error}");
            if self.preferences.current.mode == GatewayMode::Server && self.embedded.local.is_none()
            {
                self.status = self.preferences.status.clone();
                return self.start_local_server();
            }
            return Task::none();
        }
        self.notify_exits = draft.notify_exits;
        if self.notify_exits && self.notifier.is_none() {
            self.notifier = Some(Notifier::native());
        }
        if !connection_changed {
            self.preferences.current = draft;
            self.preferences.status = "Preferences saved".into();
            if self.preferences.current.mode == GatewayMode::Server && self.embedded.local.is_none()
            {
                return self.start_local_server();
            }
            return self.close_preferences();
        }
        for workspace in &mut self.workspaces {
            let panel = &mut workspace.gateway;
            panel.revision += 1;
            panel.project.clear();
            panel.detail = Default::default();
            panel.routing = Default::default();
            panel.loading = false;
            panel.refresh_pending = false;
            panel.loaded_at = None;
            panel.queried_at = None;
            panel.tasks.clear();
            panel.status = "Gateway preferences changed. Refresh to connect.".into();
        }
        self.reset_delegation();
        self.preferences.current = draft;
        self.embedded.invitation = None;
        self.embedded.pair_invitation.clear();
        self.preferences.status = "Gateway preferences saved".into();
        self.status = "Gateway preferences saved. Connecting repositories…".into();
        if self.preferences.current.mode == GatewayMode::Server
            && (mode_changed || server_changed || self.embedded.local.is_none())
        {
            return Task::batch([self.start_local_server(), self.close_preferences()]);
        }
        let queries = Task::batch(
            (0..self.workspaces.len())
                .map(|i| self.query(i))
                .collect::<Vec<_>>(),
        );
        Task::batch([queries, self.close_preferences()])
    }

    fn preferences_view(&self) -> Element<'_, Message> {
        let draft = &self.preferences.draft;
        let available = !self.embedded.busy && !self.embedded.stopping;
        let mode = |name, label, description, mode| {
            desktop_design::mode_card(
                name,
                label,
                description,
                draft.mode == mode,
                (available && draft.mode != mode).then_some(Message::GatewayMode(mode)),
            )
        };
        let mut content = column![
            text("Gateway Server").size(22),
            desktop_design::settings_callout("Changing gateways clears this app's task and project lists and reloads them from the selected gateway. Switching back reloads them again."),
            desktop_design::settings_label("GATEWAY OPERATING MODE"),
            row![
                mode("server", "Server", "Host task data locally and serve paired clients.", GatewayMode::Server),
                mode("cpu", "Client", "Connect to another machine running an amux Server.", GatewayMode::Client)
            ]
            .spacing(12),
        ]
        .spacing(18);
        content = match draft.mode {
            GatewayMode::Server => {
                let mut controls = column![
                    text("This machine stores task data and serves paired clients. Local connections also use mutual TLS."),
                    checkbox(draft.server.network_hosting).label("Allow connections from other machines").on_toggle(Message::NetworkHosting),
                    text("Server name or IP address used by clients"),
                    text_input("localhost", &draft.server.server_name).on_input(Message::ServerName),
                    text(format!("Secure port: {} · Discovery port: {}", draft.server.secure_port, draft.server.discovery_port)),
                ].spacing(10);
                if let Some(connection) = &self.preferences.current.enrolled
                    && self.preferences.current.mode == GatewayMode::Server
                    && self.embedded.local.is_some()
                    && self.preferences.current.server == draft.server
                {
                    controls = controls.push(text(format!(
                        "Client endpoint: {}",
                        draft
                            .server
                            .client_url()
                            .unwrap_or_else(|_| "Invalid server name or IP address".into())
                    )));
                    controls = controls
                        .push(text(format!("Local administration: {}", connection.url)).size(12));
                    if let Some(invitation) = self.active_invitation() {
                        let seconds = invitation
                            .deadline
                            .saturating_duration_since(Instant::now())
                            .as_secs()
                            .min(
                                invitation
                                    .expires_at
                                    .saturating_sub(unix_ms() as i64)
                                    .max(0) as u64
                                    / 1000,
                            );
                        controls = controls
                            .push(text(format!(
                                "Peering key: •••••••• · valid for {seconds}s"
                            )))
                            .push(
                                row![
                                    button("Copy peering key").on_press_maybe(
                                        available.then_some(Message::CopyInvitation)
                                    ),
                                    button("Generate new").on_press_maybe(
                                        available.then_some(Message::GenerateInvitation)
                                    )
                                ]
                                .spacing(8),
                            );
                    } else {
                        controls = controls.push(
                            button("Generate peering key")
                                .on_press_maybe(available.then_some(Message::GenerateInvitation)),
                        );
                    }
                    controls = controls.push(text("Peering keys expire within ten minutes. Generating a new key or restarting the server invalidates the previous key."));
                } else {
                    controls =
                        controls.push(text("Save settings to start or configure the server."));
                }
                content.push(desktop_design::settings_card(controls))
            }
            GatewayMode::Client => {
                let mut controls = column![
                    text("Join another amux server using its address and a temporary peering key."),
                    text("Server address"),
                    text_input("https://server:24182", &self.embedded.pair_address)
                        .id("server-address")
                        .on_input(Message::PairAddress),
                    text("Peering key"),
                    text_input(
                        "Paste the key from server Settings",
                        &self.embedded.pair_invitation
                    )
                    .secure(true)
                    .on_input(|value| Message::PairInvitation(Secret(value))),
                    button("Connect").on_press_maybe(available.then_some(Message::PairConnect)),
                ]
                .spacing(10);
                if let Some(connection) = &draft.enrolled {
                    controls = controls.push(text(format!(
                        "Paired with {} · node {}",
                        connection.url, connection.node_id
                    )));
                }
                content.push(desktop_design::settings_card(controls))
            }
        };
        content = content
            .push(text(&self.embedded.status))
            .push(desktop_design::settings_label("TASK AUTOMATION"))
            .push(checkbox(draft.automatic_notifications).label("Let the amux server assign queued work and send task notifications to agents in this app").on_toggle(Message::AutomaticNotifications))
            .push(text("On amux servers (Server or Client mode) the server decides: idle agents receive eligible tasks oldest first, completed tasks get one review by a different agent, and owners and requesters hear about their tasks. Explicit User review still waits for your decision.").size(12))
            .push(text("Notifications use assigned agent identities and required repository matching. Typed input is preserved during delivery.").size(12))
            .push(text("Repository matching is required. Settings are saved on this device.").size(12))
            .push(text(&self.workspaces[self.active].gateway.status).size(13));
        // The settings modal scrolls its content pane.
        content.into()
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
        let panel = &mut self.workspaces[workspace].gateway;
        panel.routing = Default::default();
        panel.project = project;
        panel.detail = Default::default();
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
        if self.embedded.busy
            || self.embedded.stopping
            || (self.preferences.current.mode == GatewayMode::Server
                && self.embedded.local.is_none())
        {
            return Task::none();
        }
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
        if !panel.routing.can_submit() {
            panel.draft_status = "Not submitted: selected recipient is unavailable; refresh or choose automatic routing".into();
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
        let routing = panel.routing.choice.routing();
        let remote = panel
            .routing
            .directory
            .as_ref()
            .map(|d| d.canonical_remote.clone());
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
                    let directory = scope.directory.clone();
                    // A local-only checkout has no origin; its recipients must say so too.
                    let actual = tokio::task::spawn_blocking(move || {
                        amux::gateway_project::configured_origin(&directory)
                    })
                    .await??;
                    if routing != amux::task_routing::Routing::Auto {
                        anyhow::ensure!(
                            remote.map(|remote| {
                                remote
                                    .as_deref()
                                    .and_then(amux::gateway_project::repository)
                            }) == Some(actual.clone()),
                            "Repository changed; refresh recipients before submitting"
                        );
                    }
                    gateway
                        .create_routed_task(
                            &scope.project,
                            &title,
                            &context,
                            actual.as_deref(),
                            &routing,
                        )
                        .await
                }
                .await
                .map_err(|error| error.to_string());
                (scope, request_id, result)
            },
            |(scope, request_id, result)| Message::TaskCreated(scope, request_id, result),
        )
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
            self.routing_form(),
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
            iced::event::listen_with(|event, status, _| match event {
                iced::Event::Mouse(iced::mouse::Event::CursorMoved { position }) => {
                    Some(Message::Design(desktop_design::Action::Cursor(position)))
                }
                iced::Event::Mouse(iced::mouse::Event::ButtonReleased(
                    iced::mouse::Button::Left,
                )) => Some(Message::Design(
                    desktop_design::Action::FinishTaskDrawerResize,
                )),
                iced::Event::Keyboard(iced::keyboard::Event::KeyPressed {
                    key: iced::keyboard::Key::Named(iced::keyboard::key::Named::Escape),
                    ..
                }) => Some(Message::Escape),
                // Terminals capture Tab for the shell; text fields leave it for focus.
                iced::Event::Keyboard(iced::keyboard::Event::KeyPressed {
                    key: iced::keyboard::Key::Named(iced::keyboard::key::Named::Tab),
                    modifiers,
                    ..
                }) if status == iced::event::Status::Ignored => {
                    Some(Message::FocusNext(modifiers.shift()))
                }
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
            if self.design.task_window.is_none()
                && self.workspaces[self.active].gateway.detail.animating()
            {
                iced::time::every(Duration::from_millis(16))
                    .map(|_| Message::ManageTask(desktop_tasks::Action::AnimateDrawer))
            } else {
                Subscription::none()
            },
        ])
    }

    fn view(&self) -> Element<'_, Message> {
        self.design_view()
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
            harness: desktop_harness::State::default(),
            toasts: desktop_toasts::State::default(),
            embedded: EmbeddedServer::default(),
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
            registry: None,
            snapshot_path: None,
            dirty: false,
            restarting: false,
            notify_exits: false,
            notifier: None,
            notification_status: String::new(),
            delegation: desktop_delegation::Bridge::default(),
            client_run: Uuid::new_v4(),
        }
    }

    #[test]
    fn restart_resumes_only_newest_matching_provider_and_directory_across_workspaces() {
        for provider in [Provider::Codex, Provider::Claude] {
            let mut app = desktop();
            let directory = app.workspaces[0].directory.clone();
            for (index, workspace) in app.workspaces.iter_mut().enumerate() {
                workspace.directory = directory.clone();
                let tab = &mut workspace.panes.get_mut(workspace.focused).unwrap().tabs[0];
                tab.id = index as u64;
                tab.provider = Some(provider);
            }
            assert!(!app.resume_restarted_tab(0, provider, &directory));
            assert!(app.resume_restarted_tab(1, provider, &directory));

            let other_provider = match provider {
                Provider::Codex => Provider::Claude,
                Provider::Claude => Provider::Codex,
            };
            let newer = &mut app.workspaces[1];
            newer.panes.get_mut(newer.focused).unwrap().tabs[0].provider = Some(other_provider);
            assert!(app.resume_restarted_tab(0, provider, &directory));
            let newer = &mut app.workspaces[1];
            newer.panes.get_mut(newer.focused).unwrap().tabs[0].provider = None;
            assert!(app.resume_restarted_tab(0, provider, &directory));
            // An agent discovered inside a shell tab also shares native resume.
            let newer = &mut app.workspaces[1];
            newer.panes.get_mut(newer.focused).unwrap().tabs[0]
                .delegation
                .provider = Some(provider);
            assert!(!app.resume_restarted_tab(0, provider, &directory));
            app.workspaces[1].directory = directory.join("other-project");
            assert!(app.resume_restarted_tab(0, provider, &directory));
        }
    }

    #[test]
    fn workspace_removal_preserves_selected_peer_and_saves_only_remaining_layout() {
        let mut app = desktop();
        let (fixture, _) = restart_fixture();
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
        app.preferences.draft.projects_dir = "fixture-projects".into();
        let _ = app.update(Message::Updates(true));
        assert_eq!(app.preferences.tab, PreferencesTab::Updates);
        assert_eq!(app.preferences.draft.projects_dir, "fixture-projects");
        let _ = app.update(Message::Preferences(false));
        assert!(!app.preferences.open);
        assert_eq!(app.workspaces.len(), count);
        assert!(app.preferences.draft.projects_dir.is_empty());
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

    #[test]
    fn auto_update_never_installs_after_a_failed_check() {
        let mut app = desktop();
        app.preferences.current.auto_update = true;
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
        let _ = app.update(Message::UpdatesChecked(Err("latest unreadable".into())));
        assert!(!app.updates.installing);
        assert!(app.updates.error.contains("latest unreadable"));
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
    fn update_badge_survives_a_failed_refresh_without_changing_sessions() {
        let mut app = desktop();
        let _ = app.update(Message::UpdatesChecked(Ok(Box::new(update_report()))));
        assert!(app.updates.attention());
        let _ = app.update(Message::UpdatesChecked(Err("offline".into())));
        assert!(app.updates.attention());
        assert!(app.updates.error.contains("offline"));
        let _ = app.update(Message::Updates(true));
        assert!(app.preferences.open);
        assert_eq!(app.preferences.tab, PreferencesTab::Updates);
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
        let _ = app.update(Message::UpdatesChecked(Ok(Box::new(report))));
        let _ = app.update(Message::RestartUpdated);
        assert!(app.updates.error.contains("No updated desktop"));
        assert!(!app.dirty);
        assert_eq!(app.workspaces.len(), 2);
    }

    fn restart_fixture() -> (tempfile::TempDir, Desktop) {
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
        (dir, app)
    }

    #[test]
    fn offline_update_check_does_not_hide_a_locally_ready_restart() {
        let (_dir, mut app) = restart_fixture();
        app.updates.report = None;
        let _ = app.update(Message::UpdatesChecked(Err("offline".into())));
        assert!(app.updates.report.is_none());
        assert!(app.updates.restart.required);
        assert!(app.updates.attention());
        assert!(app.updates.error.contains("offline"));
    }

    pub(super) fn preferences_fixture() -> (tempfile::TempDir, Desktop) {
        let base = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&base).unwrap();
        let dir = tempfile::tempdir_in(base).unwrap();
        let mut app = desktop();
        app.preferences.path = Some(dir.path().join("preferences.json"));
        let profile_dir = dir.path().join("native-profile");
        let security = amux_gateway_backend::security::SecurityStore::open(
            &profile_dir,
            &["localhost".into()],
        )
        .unwrap();
        let local = security
            .provision_local("https://127.0.0.1:9", &Uuid::new_v4().to_string())
            .unwrap();
        app.preferences.draft = GatewayPreferences {
            mode: GatewayMode::Client,
            enrolled: Some(EnrolledConnection {
                url: local.url,
                cluster_id: local.cluster_id,
                node_id: local.node_id,
                profile_path: amux_gateway_backend::security::local_profile_path(&profile_dir),
            }),
            ..Default::default()
        };
        (dir, app)
    }

    #[test]
    fn gateway_rebind_rejects_late_lists() {
        let (_dir, mut app) = preferences_fixture();
        let old_scope = app.scope(0);
        for workspace in &mut app.workspaces {
            workspace.gateway.loading = true;
            workspace.gateway.tasks.push(json!({"id": Uuid::new_v4()}));
        }
        let _ = app.update(Message::SavePreferences);
        assert!(app.workspaces.iter().all(|w| w.gateway.project.is_empty()));
        for workspace in &app.workspaces {
            assert!(workspace.gateway.loading);
            assert!(workspace.gateway.tasks.is_empty());
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
        app.preferences.draft.enrolled.as_mut().unwrap().url =
            "https://user:secret@example.test".into();
        let _ = app.update(Message::SavePreferences);
        assert!(app.preferences.current.enrolled.is_none());
        assert!(!app.preferences.status.contains("secret"));
        app.preferences.draft.enrolled.as_mut().unwrap().url = "https://example.test".into();
        app.workspaces[0].gateway.request = Some(TaskRequest {
            id: Uuid::new_v4(),
            title: "pending".into(),
            project: "fixture".into(),
            state: TaskRequestState::Sending,
        });
        let _ = app.update(Message::SavePreferences);
        assert!(app.preferences.current.enrolled.is_none());
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

    #[cfg(any(target_os = "linux", windows))]
    #[test]
    fn delayed_exit_from_retired_runtime_cannot_stop_replacement() {
        use iced::futures::StreamExt;
        let runtime = tokio::runtime::Runtime::new().unwrap();
        let mut app = desktop();
        let focused = app.workspaces[0].focused;
        let old_id = app.workspaces[0].panes.get(focused).unwrap().tabs[0].id;
        let old = Terminal::new(
            old_id,
            iced_term::settings::Settings {
                backend: iced_term::settings::BackendSettings {
                    #[cfg(windows)]
                    program: "cmd.exe".into(),
                    #[cfg(windows)]
                    args: vec!["/d".into(), "/c".into(), "exit 0".into()],
                    #[cfg(target_os = "linux")]
                    program: "/bin/sh".into(),
                    #[cfg(target_os = "linux")]
                    args: vec!["-c".into(), "exit 0".into()],
                    ..Default::default()
                },
                ..Default::default()
            },
        )
        .unwrap();
        let mut events = iced::advanced::subscription::into_recipes(old.subscription())
            .pop()
            .unwrap()
            .stream(iced::futures::stream::empty().boxed());
        let exit = runtime.block_on(async {
            tokio::time::timeout(Duration::from_secs(5), async {
                loop {
                    let event = events.next().await.expect("PTY must report its exit");
                    if matches!(
                        event,
                        iced_term::Event::BackendCall(
                            _,
                            iced_term::BackendCommand::ProcessAlacrittyEvent(
                                iced_term::AlacrittyEvent::Exit
                            )
                        )
                    ) {
                        break event;
                    }
                }
            })
            .await
            .expect("old PTY did not exit")
        });
        drop(old);
        let directory = std::env::current_dir().unwrap();
        let replacement_id = app.next_id;
        let replacement =
            app.open_terminal(replacement_id, &directory, None, None, false, Some(42));
        assert!(
            replacement.terminal.is_some(),
            "replacement PTY must be live"
        );
        app.workspaces[0].panes.get_mut(focused).unwrap().tabs[0] = replacement;
        let _ = app.update(Message::Terminal(exit));
        let replacement = &app.workspaces[0].panes.get(focused).unwrap().tabs[0];
        assert!(
            replacement.terminal.is_some(),
            "delayed old Exit stopped the new PTY"
        );
        assert_eq!(replacement.delegation.terminal_number, 42);
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
        assert!(tab.title.ends_with(" · exited (code 17)"));
        assert!(app.status.contains("process exited with code 17"));
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
    fn release_button_prepares_an_editable_workspace_task_without_submitting() {
        let mut app = desktop();
        let _ = app.update(Message::PrepareReleaseTask);
        let panel = &app.workspaces[0].gateway;
        assert_eq!(panel.draft_title, "Prepare and publish a new release");
        assert_eq!(panel.draft_context.text(), RELEASE_TASK_CONTEXT);
        assert!(panel.request.is_none(), "preparing must not send a request");
        assert_eq!(panel.routing.choice, desktop_routing::Choice::Auto);
        assert!(app.workspaces[1].gateway.draft_title.is_empty());

        let notes = "Wait for ticket ABC-123 before releasing.";
        let _ = app.update(Message::TaskContext(text_editor::Action::Move(
            text_editor::Motion::DocumentEnd,
        )));
        let _ = app.update(Message::TaskContext(text_editor::Action::Edit(
            text_editor::Edit::Paste(std::sync::Arc::new(notes.into())),
        )));
        let context = app.workspaces[0].gateway.draft_context.text();
        assert_eq!(context, format!("{RELEASE_TASK_CONTEXT}{notes}"));
        let _ = app.update(Message::PrepareReleaseTask);
        assert_eq!(app.workspaces[0].gateway.draft_context.text(), context);
    }

    #[test]
    fn release_button_preserves_drafts_and_task_acceptance_receipts() {
        let mut app = desktop();
        // A context-only draft also counts as work that must not be replaced.
        app.workspaces[0].gateway.draft_context = text_editor::Content::with_text("Keep notes");
        let _ = app.update(Message::PrepareReleaseTask);
        assert!(app.workspaces[0].gateway.draft_title.is_empty());
        assert_eq!(app.workspaces[0].gateway.draft_context.text(), "Keep notes");
        app.workspaces[0].gateway.draft_context = text_editor::Content::default();
        app.workspaces[0].gateway.draft_title = "Keep title".into();
        let _ = app.update(Message::PrepareReleaseTask);
        assert_eq!(app.workspaces[0].gateway.draft_title, "Keep title");
        app.workspaces[0].gateway.draft_title.clear();
        for state in [
            TaskRequestState::Sending,
            TaskRequestState::Accepted(Uuid::new_v4()),
            TaskRequestState::Unconfirmed("connection lost".into()),
        ] {
            let request_id = Uuid::new_v4();
            app.workspaces[0].gateway.request = Some(TaskRequest {
                id: request_id,
                project: "project-0".into(),
                title: "Existing request".into(),
                state,
            });
            let _ = app.update(Message::PrepareReleaseTask);
            let panel = &app.workspaces[0].gateway;
            assert!(panel.draft_title.is_empty());
            assert!(panel.draft_context.text().is_empty());
            assert_eq!(panel.request.as_ref().unwrap().id, request_id);
        }
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
        assert!(
            app.workspaces[0]
                .gateway
                .tasks
                .iter()
                .any(|t| t["id"] == task.to_string())
        );
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
    fn fresh_server_default_preserves_saved_client_selection() {
        let (fresh, error) = startup_connection(Ok(None), GatewayPreferences::default());
        assert_eq!(fresh.mode, GatewayMode::Server);
        assert!(error.is_empty());
        let client = GatewayPreferences {
            mode: GatewayMode::Client,
            ..Default::default()
        };
        let (restored, error) = startup_connection(Ok(Some(client)), fresh);
        assert_eq!(restored.mode, GatewayMode::Client);
        assert!(!restored.has_connection());
        assert!(error.is_empty());
        let (invalid, error) = startup_connection(
            Err(anyhow::anyhow!("Invalid settings")),
            GatewayPreferences::default(),
        );
        assert_eq!(invalid.mode, GatewayMode::Client);
        assert!(!invalid.has_connection());
        assert!(!error.is_empty());
    }

    #[test]
    fn server_initialization_gates_queries_and_does_not_start_without_state_path() {
        let mut app = desktop();
        app.preferences.current.mode = GatewayMode::Server;
        let _ = app.query(0);
        assert!(!app.workspaces[0].gateway.loading);
        assert!(app.workspaces[0].gateway.queried_at.is_none());
        let _ = app.start_local_server();
        assert!(!app.embedded.busy);
        assert!(app.embedded.local.is_none());
        assert!(app.embedded.status.contains("directory unavailable"));
        let _ = app.update(Message::ServerReady(99, Err("stale failure".into())));
        assert!(!app.embedded.status.contains("stale failure"));
        let _ = app.update(Message::ServerReady(0, Err("fixture failure".into())));
        assert!(app.embedded.status.contains("fixture failure"));
        assert!(app.embedded.local.is_none());
    }

    #[test]
    fn peering_invitation_is_redacted_expiring_and_generation_fenced() {
        let mut app = desktop();
        let secret = "fixture-private-invitation";
        let invitation = PeeringInvitation {
            value: Secret(secret.into()),
            expires_at: unix_ms() as i64 + 600_000,
            deadline: Instant::now() + Duration::from_secs(600),
        };
        assert!(!format!("{:?}", invitation).contains(secret));
        let input = Message::PairInvitation(Secret(secret.into()));
        assert!(!format!("{input:?}").contains(secret));
        let ready = Message::InvitationReady(7, true, Ok(invitation.clone()));
        assert!(!format!("{ready:?}").contains(secret));
        let _ = app.update(ready);
        assert!(app.embedded.invitation.is_none());
        let _ = app.update(Message::InvitationReady(0, true, Ok(invitation)));
        assert!(app.active_invitation().is_some());
        app.embedded.invitation.as_mut().unwrap().deadline = Instant::now();
        assert!(app.active_invitation().is_none());
        let _ = app.update(Message::DeliveryTick);
        assert!(app.embedded.invitation.is_none());
        let expired = PeeringInvitation {
            value: Secret(secret.into()),
            expires_at: unix_ms() as i64 - 1,
            deadline: Instant::now() + Duration::from_secs(600),
        };
        app.embedded.invitation = Some(expired);
        assert!(app.active_invitation().is_none());
        let _ = app.update(Message::DeliveryTick);
        assert!(app.embedded.invitation.is_none());
        assert!(!app.embedded.status.contains(secret));
        let _ = app.update(Message::InvitationReady(
            0,
            false,
            Ok(PeeringInvitation {
                value: Secret(secret.into()),
                expires_at: unix_ms() as i64 + 600_000,
                deadline: Instant::now() + Duration::from_secs(600),
            }),
        ));
        assert_eq!(app.embedded.status, "Current peering key copied");
        let _ = app.update(Message::InvitationReady(
            0,
            false,
            Err("no active key".into()),
        ));
        assert!(app.embedded.invitation.is_none());
        assert!(app.embedded.status.contains("Generate a new key"));
    }

    #[test]
    fn mode_selection_and_pairing_secret_are_generation_fenced() {
        let mut app = desktop();
        app.embedded.pair_invitation = "temporary-secret".into();
        let _ = app.update(Message::GatewayMode(GatewayMode::Server));
        assert!(app.embedded.pair_invitation.is_empty());
        let _ = app.update(Message::GatewayMode(GatewayMode::Client));
        assert_eq!(app.preferences.current.mode, GatewayMode::Server);
        app.embedded.busy = true;
        let _ = app.update(Message::GatewayMode(GatewayMode::Server));
        assert_eq!(app.preferences.draft.mode, GatewayMode::Client);
        let _ = app.update(Message::PairConnected(99, Err("stale-pair".into())));
        assert!(!app.embedded.status.contains("stale-pair"));
        let stale_connection = EnrolledConnection {
            url: "https://stale.example:24182".into(),
            cluster_id: Uuid::new_v4().to_string(),
            node_id: Uuid::new_v4().to_string(),
            profile_path: PathBuf::from("target/test-fixtures/unused-stale-client-profile.json"),
        };
        let _ = app.update(Message::PairConnected(99, Ok(stale_connection)));
        assert_eq!(app.preferences.current.mode, GatewayMode::Server);
        assert_eq!(app.preferences.draft.mode, GatewayMode::Client);
        assert!(app.preferences.current.enrolled.is_none());
        assert!(app.preferences.draft.enrolled.is_none());
        assert!(app.embedded.local.is_none());
        assert!(app.embedded.busy);
    }
    #[test]
    fn lifecycle_failures_keep_the_window_and_existing_connection_usable() {
        let mut app = desktop();
        app.preferences.current.server.server_name = "existing-server".into();
        app.embedded.stopping = true;
        let _ = app.update(Message::ServerStopped(
            ServerExit::Close,
            Err("fixture stop failure".into()),
        ));
        assert!(!app.embedded.stopping);
        assert!(!app.restarting);
        assert!(app.embedded.status.contains("fixture stop failure"));
        assert_eq!(
            app.preferences.current.server.server_name,
            "existing-server"
        );
        app.embedded.busy = true;
        app.preferences.open = true;
        let _ = app.close_preferences();
        assert!(app.preferences.open);
        let _ = app.update(Message::SubmitTask);
        assert!(app.workspaces[0].gateway.request.is_none());
        let _ = app.update(Message::ServerReconfigured(
            0,
            Err("fixture reconfigure failure".into()),
        ));
        assert!(!app.embedded.busy);
        assert_eq!(app.preferences.current.mode, GatewayMode::Server);
        assert_eq!(
            app.preferences.current.server.server_name,
            "existing-server"
        );
    }
    #[test]
    fn saving_unchanged_server_preferences_retries_a_failed_start() {
        let (_directory, mut app) = preferences_fixture();
        app.preferences.current = GatewayPreferences {
            mode: GatewayMode::Server,
            ..Default::default()
        };
        app.preferences.draft = app.preferences.current.clone();
        app.preferences.open = true;
        app.embedded.status = "Previous startup failed".into();
        // No registry path: prove the retry reaches the controller precondition
        // without starting a process or touching installed application state.
        assert!(app.registry.is_none());
        let _ = app.save_preferences();
        assert!(app.embedded.status.contains("directory unavailable"));
        assert!(app.preferences.open);
        assert!(app.embedded.local.is_none());
        assert_eq!(
            GatewayPreferences::load(app.preferences.path.as_deref().unwrap())
                .unwrap()
                .unwrap()
                .mode,
            GatewayMode::Server
        );
    }

    #[test]
    fn replacing_the_node_on_the_same_cluster_fences_old_workspace_callbacks() {
        let (directory, mut app) = preferences_fixture();
        let original = EnrolledConnection {
            url: "https://same-cluster.example:24182".into(),
            cluster_id: Uuid::new_v4().to_string(),
            node_id: Uuid::new_v4().to_string(),
            profile_path: directory.path().join("unused-node-profile.json"),
        };
        app.preferences.current = GatewayPreferences {
            mode: GatewayMode::Client,
            enrolled: Some(original.clone()),
            ..Default::default()
        };
        app.preferences.draft = app.preferences.current.clone();
        let replacement = EnrolledConnection {
            node_id: Uuid::new_v4().to_string(),
            ..original
        };
        app.preferences.draft.enrolled = Some(replacement.clone());
        app.workspaces[0].gateway.project = "old-project".into();
        app.workspaces[0].gateway.tasks = vec![serde_json::json!({"id": "old-visible-task"})];
        let previous_scope = app.scope(0);
        let _ = app.save_preferences();
        assert_eq!(
            app.preferences.current.enrolled.as_ref().unwrap().node_id,
            replacement.node_id
        );
        assert!(!app.scope_current(&previous_scope));
        assert!(app.workspaces[0].gateway.tasks.is_empty());
        assert!(app.workspaces[0].gateway.project.is_empty());
        assert!(!replacement.profile_path.exists());
    }
}
