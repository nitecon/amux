//! Provisional native terminal/UI candidate. Agent work is never restarted implicitly.
use amux::delivery::{DeliveryCause, DeliveryLedger, DeliveryState};
use amux::gateway_preferences::GatewayPreferences;
use amux::identity::Provider;
use amux::notifications::{Notifier, Outcome};
use amux::readiness::ReadinessGate;
use amux::recipient::TerminalRecipient;
use amux::session::{Layout, SavedTab, SavedWorkspace, Snapshot};
use amux::submission::{Submission, submit_literal};
#[path = "desktop_recovery.rs"]
mod desktop_recovery;
use iced::widget::{
    button, checkbox, column, container, pane_grid, row, scrollable, text, text_editor, text_input,
    tooltip,
};
use iced::{Element, Length, Subscription, Task, Theme};
use iced_term::{Terminal, TerminalView};
use std::collections::HashSet;
use std::path::PathBuf;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use uuid::Uuid;

#[derive(Debug, Clone)]
enum Message {
    NewWorkspace,
    SelectWorkspace(usize),
    Directory(String),
    NewTab(Option<Provider>),
    SelectTab(pane_grid::Pane, usize),
    Split(pane_grid::Axis),
    Focus(pane_grid::Pane),
    CloseTab(pane_grid::Pane, u64),
    Preferences(bool),
    Preference(PreferenceField, Secret),
    SavePreferences,
    Resize(pane_grid::ResizeEvent),
    Terminal(iced_term::Event),
    GatewayProject(String),
    RefreshTasks,
    TasksLoaded(QueryScope, Result<serde_json::Value, String>),
    RecoveryLoaded(RecoveryScope, Result<desktop_recovery::Recovery, String>),
    ReceiptReported(RecoveryScope, Uuid, Result<(), String>),
    TaskTitle(String),
    TaskContext(text_editor::Action),
    SubmitTask,
    NewTaskRequest,
    TaskCreated(QueryScope, Uuid, Result<Uuid, String>),
    LocalNotifications(bool),
    Consent(u64, bool),
    Remind(QueryScope, Uuid, u64),
    DeliveryTick,
    Save,
    Close(iced::window::Id),
}

#[derive(Debug, Clone, Copy)]
enum PreferenceField {
    Url,
    ApiKey,
    Project,
    MachineId,
    MachineKey,
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
    status: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct QueryScope {
    workspace: usize,
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
    directory: PathBuf,
    panes: pane_grid::State<Pane>,
    focused: pane_grid::Pane,
    gateway: TaskPanel,
}
struct Desktop {
    preferences: Preferences,
    workspaces: Vec<Workspace>,
    active: usize,
    directory: String,
    next_id: u64,
    status: String,
    ledger: Option<DeliveryLedger>,
    registry: Option<PathBuf>,
    snapshot_path: Option<PathBuf>,
    dirty: bool,
    notify_exits: bool,
    notifier: Option<Notifier>,
    notification_status: String,
}

pub fn run(directory: PathBuf) -> anyhow::Result<()> {
    let directory = directory.canonicalize()?;
    iced::application(
        move || Desktop::new(directory.clone()),
        Desktop::update,
        Desktop::view,
    )
    .title("amux · agent workspaces")
    .theme(Theme::TokyoNight)
    .window_size((1380.0, 840.0))
    .exit_on_close_request(false)
    .subscription(Desktop::subscription)
    .run()?;
    Ok(())
}

impl Desktop {
    fn new(directory: PathBuf) -> (Self, Task<Message>) {
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
            preferences: Preferences { draft: preferences.clone(), current: preferences, path: preferences_path, open: false, status: preferences_error },
            workspaces: Vec::new(), active: 0, directory: directory.display().to_string(), next_id: 0,
            status: "Gateway reminders require per-agent consent and a supported empty prompt. Only Linux/Codex0.163 is supported; other profiles defer.".into(),
            ledger: ledger.ok().flatten(),
            registry: state.ok(),
            snapshot_path: amux::identity::state_directory().ok().map(|p| p.join("layout.json")), dirty: false,
            notify_exits: false, notifier: None, notification_status: String::new(),
        };
        match app.snapshot_path.as_deref().map(Snapshot::load).transpose() {
            Ok(Some(Some(saved))) => {
                app.active = saved.active;
                for workspace in saved.workspaces {
                    let config = app.restore_layout(workspace.layout);
                    let panes = pane_grid::State::with_configuration(config);
                    let focused = *panes.iter().next().expect("validated nonempty layout").0;
                    app.workspaces.push(Workspace {
                        directory: workspace.directory,
                        panes,
                        focused,
                        gateway: TaskPanel::default(),
                    });
                }
                app.status =
                    "Layout restored. Start terminals and agents explicitly; no work was rerun."
                        .into();
            }
            Err(error) => {
                app.status = format!(
                    "Saved layout could not be loaded: {error}. Original preserved; saving disabled for this launch."
                );
                app.snapshot_path = None;
            }
            _ => {}
        }
        if app.workspaces.is_empty() {
            app.add_workspace(directory.clone());
        }
        // An environment project binding applies only to the launch directory.
        for workspace in &mut app.workspaces {
            if workspace.directory == directory {
                workspace.gateway.project = app.preferences.current.project.clone();
            }
        }
        if let Some(error) = delivery_error {
            app.status =
                format!("Reminder storage unavailable: {error}. Local terminals remain available.");
        }
        let focus = app.focus();
        (app, focus)
    }

    fn restore_layout(&mut self, layout: Layout) -> pane_grid::Configuration<Pane> {
        match layout {
            Layout::Pane { tabs, active } => pane_grid::Configuration::Pane(Pane {
                tabs: tabs
                    .into_iter()
                    .map(|tab| {
                        self.next_id = self.next_id.max(tab.id + 1);
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
                a: Box::new(self.restore_layout(*a)),
                b: Box::new(self.restore_layout(*b)),
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
                                title: t.title.clone(),
                                provider: t.provider,
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
                    directory: w.directory.clone(),
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

    fn terminal(&mut self, directory: &std::path::Path, provider: Option<Provider>) -> Tab {
        let id = self.next_id;
        self.next_id += 1;
        let mut backend = iced_term::settings::BackendSettings {
            working_directory: Some(directory.into()),
            ..Default::default()
        };
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
        // Do not let shells or another agent launch inherit a previous agent identity.
        for name in [
            "AMUX_AGENT_SESSION_ID",
            "AMUX_AGENT_INSTANCE_ID",
            "AMUX_AGENT_PROVIDER",
            "AMUX_AGENT_OS",
            "AMUX_AGENT_PROJECT",
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
                ..Default::default()
            },
        ) {
            Ok(terminal) => {
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
                }
            }
        }
    }

    fn add_workspace(&mut self, directory: PathBuf) {
        let tab = self.terminal(&directory, None);
        let (panes, focused) = pane_grid::State::new(Pane {
            tabs: vec![tab],
            active: 0,
        });
        self.workspaces.push(Workspace {
            directory,
            panes,
            focused,
            gateway: TaskPanel::default(),
        });
        self.active = self.workspaces.len() - 1;
    }

    fn focus(&self) -> Task<Message> {
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
            Message::Save => {
                self.save();
            }
            Message::Close(window) => {
                if self.save() {
                    return iced::window::close(window);
                }
            }
            Message::Directory(value) => self.directory = value,
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
            Message::NewWorkspace => match PathBuf::from(&self.directory).canonicalize() {
                Ok(directory) if directory.is_dir() => {
                    self.add_workspace(directory);
                    return self.focus();
                }
                _ => self.status = "Choose an existing project directory".into(),
            },
            Message::SelectWorkspace(index) => {
                if index < self.workspaces.len() {
                    self.active = index;
                    return self.focus();
                }
            }
            Message::NewTab(provider) => {
                let directory = self.workspaces[self.active].directory.clone();
                let tab = self.terminal(&directory, provider);
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
                let directory = self.workspaces[self.active].directory.clone();
                let tab = self.terminal(&directory, None);
                let workspace = &mut self.workspaces[self.active];
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
                self.preferences.open = open;
                self.preferences.draft = self.preferences.current.clone();
            }
            Message::Preference(field, Secret(value)) => {
                if value.len() <= 16 * 1024 {
                    let draft = &mut self.preferences.draft;
                    *match field {
                        PreferenceField::Url => &mut draft.url,
                        PreferenceField::ApiKey => &mut draft.api_key,
                        PreferenceField::Project => &mut draft.project,
                        PreferenceField::MachineId => &mut draft.machine_id,
                        PreferenceField::MachineKey => &mut draft.machine_key,
                    } = value;
                }
            }
            Message::SavePreferences => return self.save_preferences(),
            Message::Resize(event) => self.workspaces[self.active]
                .panes
                .resize(event.split, event.ratio.clamp(0.05, 0.95)),
            Message::Terminal(iced_term::Event::BackendCall(id, command)) => {
                // Backend events carry exact terminal IDs, including hidden workspaces.
                // Never close the focused pane in response to a different process exiting.
                for workspace in &mut self.workspaces {
                    for (_, pane) in workspace.panes.iter_mut() {
                        if let Some(tab) = pane.tabs.iter_mut().find(|tab| tab.id == id) {
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
            Message::GatewayProject(project) => {
                let ids: Vec<_> = self.workspaces[self.active]
                    .panes
                    .iter()
                    .flat_map(|(_, pane)| &pane.tabs)
                    .map(|tab| tab.id)
                    .collect();
                for id in ids {
                    self.withdraw(id);
                }
                let panel = &mut self.workspaces[self.active].gateway;
                panel.project = project;
                panel.revision += 1;
                panel.loading = false;
                panel.loaded_at = None;
                panel.tasks.clear();
                panel.status = "Project changed; consent withdrawn. Refresh current tasks.".into();
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
                        panel.status = "Select a pending task to remind one consenting agent. Claims remain agent decisions.".into();
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
            Message::Remind(scope, task, terminal) => self.queue_reminder(&scope, task, terminal),
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
                return self.delivery_tick();
            }
        }
        Task::none()
    }

    fn save_preferences(&mut self) -> Task<Message> {
        if self.workspaces.iter().any(|w| {
            w.gateway.request.as_ref().is_some_and(|r| {
                matches!(
                    r.state,
                    TaskRequestState::Sending | TaskRequestState::Unconfirmed(_)
                )
            })
        }) {
            self.preferences.status = "A task submission is pending or unconfirmed. Inspect its result before changing connections; use New task only after resolving it.".into();
            return Task::none();
        }
        let mut draft = self.preferences.draft.clone();
        draft.url = draft.url.trim().to_owned();
        draft.project = draft.project.trim().to_owned();
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
            panel.loading = false;
            panel.loaded_at = None;
            panel.queried_at = None;
            panel.tasks.clear();
            panel.status =
                "Gateway preferences changed. Refresh to connect; reminder consent withdrawn."
                    .into();
        }
        self.workspaces[self.active].gateway.project = draft.project.clone();
        self.preferences.current = draft;
        self.preferences.open = false;
        self.preferences.status = "Gateway preferences saved".into();
        self.status = "Gateway preferences saved. Refresh tasks to connect.".into();
        Task::none()
    }

    fn preferences_view(&self) -> Element<'_, Message> {
        let draft = &self.preferences.draft;
        let field = |label, value, kind, secure| {
            column![
                text(label).size(13),
                text_input("", value)
                    .secure(secure)
                    .on_input(move |value| Message::Preference(kind, Secret(value)))
            ]
            .spacing(4)
        };
        container(scrollable(column![
            text("Gateway preferences").size(26),
            text("Connect this desktop to your task gateway."),
            field("Gateway URL", &draft.url, PreferenceField::Url, false),
            field("API key", &draft.api_key, PreferenceField::ApiKey, true),
            field("Project identifier", &draft.project, PreferenceField::Project, false),
            text("Optional machine credentials").size(18),
            text("Needed for automatic recovery on supported agents. Get these from your gateway administrator.").size(12),
            field("Machine UUID", &draft.machine_id, PreferenceField::MachineId, false),
            field("Machine key", &draft.machine_key, PreferenceField::MachineKey, true),
            text("Saved on this device in the application data directory. Saved preferences take precedence over launch environment variables.").size(12),
            text(&self.preferences.status).size(13),
            row![button("Save preferences").on_press(Message::SavePreferences), button("Cancel").on_press(Message::Preferences(false))].spacing(8),
        ].spacing(14).max_width(600))).padding(24).width(Length::Fill).height(Length::Fill).into()
    }

    fn scope(&self, workspace: usize) -> QueryScope {
        let current = &self.workspaces[workspace];
        QueryScope {
            workspace,
            directory: current.directory.clone(),
            project: current.gateway.project.trim().to_owned(),
            revision: current.gateway.revision,
        }
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
        if scope.project.is_empty() {
            panel.status = "Enter this workspace's gateway project identifier".into();
            return Task::none();
        }
        let gateway = self.preferences.current.gateway();
        match gateway {
            Ok(gateway) => {
                panel.loading = true;
                Task::perform(
                    async move {
                        let result = gateway
                            .list(&scope.project)
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
            panel.draft_status = "Not submitted: enter a gateway project and task title".into();
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
        if query.directory.canonicalize().ok().as_ref() != Some(&actor.project_directory) {
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

    fn queue_reminder(&mut self, scope: &QueryScope, task: Uuid, id: u64) {
        if !self.scope_current(scope) {
            return;
        }
        let workspace = &mut self.workspaces[scope.workspace];
        if !workspace.gateway.pending_task(task) {
            workspace.gateway.status = "Refresh before selecting a currently pending task.".into();
            return;
        }
        let Some(tab) = workspace
            .panes
            .iter_mut()
            .flat_map(|(_, pane)| &mut pane.tabs)
            .find(|tab| tab.id == id && tab.reminders.consent)
        else {
            return;
        };
        let result = (|| -> anyhow::Result<DeliveryState> {
            let recipient = tab
                .recipient
                .as_mut()
                .ok_or_else(|| anyhow::anyhow!("Agent stopped"))?;
            recipient.resolve(
                self.registry
                    .as_deref()
                    .ok_or_else(|| anyhow::anyhow!("Agent registry unavailable"))?,
            )?;
            let address = recipient
                .address(id)
                .ok_or_else(|| anyhow::anyhow!("Agent recipient is no longer live"))?;
            let ledger = self
                .ledger
                .as_mut()
                .ok_or_else(|| anyhow::anyhow!("Reminder storage unavailable"))?;
            let now = unix_ms();
            let delivery = ledger.queue(
                task,
                address,
                DeliveryCause::RecoveryQuery,
                workspace.gateway.observed_at_ms,
                now,
            )?;
            Ok(ledger
                .records()
                .iter()
                .find(|r| r.id == delivery)
                .unwrap()
                .state)
        })();
        tab.reminders.status = match result {
            Ok(DeliveryState::Pending) => {
                "Task queued for this agent; waiting for a supported empty prompt.".into()
            }
            Ok(state) => format!("Existing reminder {state:?}; no replay."),
            Err(error) => format!("Reminder not queued: {error}"),
        };
    }

    fn delivery_tick(&mut self) -> Task<Message> {
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
            text("New task · not submitted").size(14),
            text_input("Task title", &panel.draft_title).on_input(Message::TaskTitle),
            text_editor(&panel.draft_context)
                .placeholder("Context / handoff text (optional)")
                .on_action(Message::TaskContext)
                .height(100),
            button("Create task").on_press(Message::SubmitTask),
            text(&panel.draft_status).size(12),
        ]
        .spacing(6)
        .into()
    }

    fn task_list(&self) -> Element<'_, Message> {
        let workspace = &self.workspaces[self.active];
        let panel = &workspace.gateway;
        let recipient = workspace
            .panes
            .get(workspace.focused)
            .and_then(|pane| pane.tabs.get(pane.active))
            .filter(|tab| tab.reminders.consent && tab.terminal.is_some());
        let mut list = column![text(&panel.status).size(12)].spacing(12);
        for task in &panel.tasks {
            let id = task["id"].as_str().and_then(|id| Uuid::parse_str(id).ok());
            let mut item = column![
                text(task["title"].as_str().unwrap_or("Untitled")).size(14),
                text(format!(
                    "{} · {}",
                    task["id"].as_str().unwrap_or("?"),
                    task["status"].as_str().unwrap_or("unknown")
                ))
                .size(12),
                text(format!(
                    "Owner: {}",
                    task["owner_origin"]["session_id"]
                        .as_str()
                        .or(task["owner_agent_id"].as_str())
                        .unwrap_or("unclaimed")
                ))
                .size(12)
            ]
            .spacing(4);
            if let (Some(task), Some(tab)) = (id, recipient)
                && panel.pending_task(task)
            {
                item =
                    item.push(
                        button(text(format!("Remind agent #{}", tab.id)))
                            .on_press(Message::Remind(self.scope(self.active), task, tab.id)),
                    );
            }
            list = list.push(item);
        }
        list.into()
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
            iced::time::every(std::time::Duration::from_secs(1)).map(|_| Message::Save),
            iced::window::close_requests().map(Message::Close),
            iced::time::every(Duration::from_millis(250)).map(|_| Message::DeliveryTick),
        ])
    }

    fn view(&self) -> Element<'_, Message> {
        if self.preferences.open {
            return self.preferences_view();
        }
        let workspace = &self.workspaces[self.active];
        let mut sidebar = column![text("amux").size(30), text("WORKSPACES").size(12)].spacing(12);
        for (i, w) in self.workspaces.iter().enumerate() {
            let name = w
                .directory
                .file_name()
                .unwrap_or(w.directory.as_os_str())
                .to_string_lossy();
            sidebar = sidebar.push(
                button(text(format!(
                    "{} {}",
                    if i == self.active { "●" } else { "○" },
                    name
                )))
                .on_press(Message::SelectWorkspace(i))
                .width(Length::Fill),
            );
        }
        sidebar = sidebar
            .push(text_input("Project directory", &self.directory).on_input(Message::Directory))
            .push(button("New workspace").on_press(Message::NewWorkspace))
            .push(button("Gateway preferences").on_press(Message::Preferences(true)))
            .push(
                checkbox(self.notify_exits)
                    .label("Notify on process exit")
                    .on_toggle(Message::LocalNotifications),
            );
        let toolbar = row![
            button("Terminal").on_press(Message::NewTab(None)),
            button("Codex").on_press(Message::NewTab(Some(Provider::Codex))),
            button("Claude").on_press(Message::NewTab(Some(Provider::Claude))),
            button("Split right").on_press(Message::Split(pane_grid::Axis::Vertical)),
            button("Split down").on_press(Message::Split(pane_grid::Axis::Horizontal))
        ]
        .spacing(8);
        let grid = pane_grid::PaneGrid::new(&workspace.panes, |id, pane, _| {
            let mut tabs = row![].spacing(4);
            for (i, tab) in pane.tabs.iter().enumerate() {
                let label = match tab.provider { Some(Provider::Codex) => "Codex", Some(Provider::Claude) => "Claude", None => "Terminal" };
                // Two sibling controls in one tab container: closing never selects a peer.
                let select = button(text(label)).style(if pane.active == i { button::primary } else { button::secondary })
                    .on_press(Message::SelectTab(id, i));
                let close = button(text("×").size(14)).style(button::text).padding([4, 6])
                    .on_press(Message::CloseTab(id, tab.id));
                let subtitle = format!("{}{}", workspace.directory.file_name().unwrap_or(workspace.directory.as_os_str()).to_string_lossy(), if tab.terminal.is_none() { " · stopped" } else { "" });
                tabs = tabs.push(container(column![
                    container(row![select, close].align_y(iced::Alignment::Center)).style(container::rounded_box),
                    tooltip(text(short_title(&subtitle)).size(11).wrapping(iced::widget::text::Wrapping::None), container(text(format!("{} · {}", workspace.directory.display(), short_title(&tab.title)))).padding(6), tooltip::Position::Bottom),
                ].spacing(3)));
            }
            if let Some(tab) = pane.tabs.get(pane.active)
                && cfg!(target_os = "linux") && tab.provider == Some(Provider::Codex) && tab.recipient.is_some() && self.ledger.is_some() {
                    let terminal_id = tab.id;
                    tabs = tabs.push(checkbox(tab.reminders.consent).label("Allow gateway reminders + Enter")
                        .on_toggle(move |enabled| Message::Consent(terminal_id, enabled)));
            }
            let body: Element<'_, Message> = match pane.tabs.get(pane.active).and_then(|t| t.terminal.as_ref()) {
                Some(terminal) => {
                    let id = terminal.id;
                    let resize = move |size| Message::Terminal(iced_term::Event::BackendCall(id, iced_term::BackendCommand::Resize(Some(size), None)));
                    iced::widget::Sensor::new(TerminalView::show(terminal).map(Message::Terminal))
                        .key(id).on_show(resize).on_resize(resize).into()
                },
                None => {
                    let mut stopped = column![text("Process stopped. Launch Shell, Codex or Claude explicitly; previous work is not restarted.")].spacing(12);
                    if let Some(output) = pane.tabs.get(pane.active).and_then(|tab| tab.stopped_output.as_ref()) {
                        stopped = stopped.push(scrollable(text(output).font(iced::Font::MONOSPACE)).height(Length::Fill));
                    }
                    container(stopped).padding(20).into()
                },
            };
            let mut header = column![scrollable(tabs).horizontal().width(Length::Fill).height(64)].spacing(4);
            if let Some(tab) = pane.tabs.get(pane.active) && !tab.reminders.status.is_empty() {
                header = header.push(text(&tab.reminders.status).size(11));
            }
            pane_grid::Content::new(body).title_bar(pane_grid::TitleBar::new(header).padding(4))
        }).spacing(5).on_click(Message::Focus).on_resize(8, Message::Resize).width(Length::Fill).height(Length::Fill);
        let tasks = column![
            text("GATEWAY TASKS").size(16),
            button("Gateway preferences").on_press(Message::Preferences(true)),
            text_input("Gateway project", &workspace.gateway.project)
                .on_input(Message::GatewayProject),
            button(if workspace.gateway.loading {
                "Loading…"
            } else {
                "Refresh"
            })
            .on_press(Message::RefreshTasks),
            scrollable(column![self.task_form(), self.task_list()].spacing(16))
        ]
        .spacing(10)
        .width(280);
        container(
            column![
                row![
                    container(scrollable(sidebar)).width(200).padding(12),
                    column![
                        toolbar,
                        text(workspace.directory.display().to_string()).size(12),
                        grid
                    ]
                    .spacing(10)
                    .width(Length::Fill)
                    .height(Length::Fill),
                    container(tasks).padding(12)
                ]
                .height(Length::Fill),
                text(&self.status).size(12),
                text(&self.notification_status).size(12),
            ]
            .spacing(8),
        )
        .padding(10)
        .into()
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
    fn desktop() -> Desktop {
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
                }],
                active: 0,
            });
            workspaces.push(Workspace {
                directory: PathBuf::from(format!("/fixture/{id}")),
                panes,
                focused,
                gateway: TaskPanel {
                    project: format!("project-{id}"),
                    ..Default::default()
                },
            });
        }
        Desktop {
            preferences: Preferences {
                current: GatewayPreferences::default(),
                draft: GatewayPreferences::default(),
                path: None,
                open: false,
                status: String::new(),
            },
            workspaces,
            active: 0,
            directory: String::new(),
            next_id: 2,
            status: String::new(),
            ledger: None,
            registry: None,
            snapshot_path: None,
            dirty: false,
            notify_exits: false,
            notifier: None,
            notification_status: String::new(),
        }
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
            project: "fixture".into(),
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
        assert_eq!(app.preferences.current.project, "fixture");
        for workspace in &app.workspaces {
            assert!(!workspace.gateway.loading);
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
            project: "fixture".into(),
            title: "pending".into(),
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
        let actor = AgentIdentity::new(Provider::Codex, directory.path()).unwrap();
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
