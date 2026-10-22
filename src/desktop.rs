//! Provisional native terminal/UI candidate. Agent work is never restarted implicitly.
use amux::delivery::{DeliveryCause, DeliveryLedger, DeliveryState};
use amux::readiness::ReadinessGate;
use amux::recipient::TerminalRecipient;
use amux::session::{Layout, SavedTab, SavedWorkspace, Snapshot};
use amux::submission::{Submission, submit_literal};
use amux::{gateway::Gateway, identity::Provider};
use iced::widget::{
    button, checkbox, column, container, pane_grid, row, scrollable, text, text_editor, text_input,
};
use iced::{Element, Length, Subscription, Task, Theme};
use iced_term::{Terminal, TerminalView};
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
    CloseTab,
    Resize(pane_grid::ResizeEvent),
    Terminal(iced_term::Event),
    GatewayProject(String),
    RefreshTasks,
    TasksLoaded(QueryScope, Result<serde_json::Value, String>),
    TaskTitle(String),
    TaskContext(text_editor::Action),
    SubmitTask,
    NewTaskRequest,
    TaskCreated(QueryScope, Uuid, Result<Uuid, String>),
    Consent(u64, bool),
    Remind(QueryScope, Uuid, u64),
    DeliveryTick,
    Save,
    Close(iced::window::Id),
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct QueryScope {
    workspace: usize,
    directory: PathBuf,
    project: String,
    revision: u64,
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
}

struct Tab {
    id: u64,
    title: String,
    // Fields drop in declaration order: request owned-job retirement before the PTY drops.
    #[cfg(any(target_os = "linux", windows))]
    lifetime: Option<amux::terminal_lifetime::TerminalLifetime>,
    terminal: Option<Terminal>,
    provider: Option<Provider>,
    recipient: Option<TerminalRecipient>,
    reminders: Reminders,
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
    workspaces: Vec<Workspace>,
    active: usize,
    directory: String,
    next_id: u64,
    status: String,
    ledger: Option<DeliveryLedger>,
    registry: Option<PathBuf>,
    snapshot_path: Option<PathBuf>,
    dirty: bool,
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
        let mut app = Self {
            workspaces: Vec::new(), active: 0, directory: directory.display().to_string(), next_id: 0,
            status: "Reminders require per-agent consent and a task selection. Only Linux/Codex0.163 prompts are supported; other profiles defer.".into(),
            ledger: ledger.ok().flatten(),
            registry: state.ok(),
            snapshot_path: amux::identity::state_directory().ok().map(|p| p.join("layout.json")), dirty: false,
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
                workspace.gateway.project =
                    std::env::var("AMUX_GATEWAY_PROJECT").unwrap_or_default();
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
                | Message::CloseTab
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
            Message::CloseTab => {
                let workspace = &self.workspaces[self.active];
                if let Some(pane) = workspace.panes.get(workspace.focused)
                    && (pane.tabs.len() > 1 || workspace.panes.len() > 1)
                    && let Some(tab) = pane.tabs.get(pane.active)
                {
                    self.withdraw(tab.id);
                }
                let workspace = &mut self.workspaces[self.active];
                let focused = workspace.focused;
                if let Some(pane) = workspace.panes.get_mut(focused) {
                    if pane.tabs.len() > 1 {
                        pane.tabs.remove(pane.active);
                        pane.active = pane.active.min(pane.tabs.len() - 1);
                    } else if workspace.panes.len() > 1 {
                        if let Some((_, sibling)) = workspace.panes.close(focused) {
                            workspace.focused = sibling;
                        }
                    } else {
                        self.status = "Keep one terminal per workspace; exit the application to close all sessions".into();
                    }
                }
                return self.focus();
            }
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
                if accepted && panel.project.trim() == scope.project {
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
            Message::DeliveryTick => return self.delivery_tick(),
        }
        Task::none()
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
        let gateway = std::env::var("AMUX_GATEWAY_URL")
            .ok()
            .zip(std::env::var("AMUX_GATEWAY_KEY").ok())
            .ok_or_else(|| anyhow::anyhow!("Set AMUX_GATEWAY_URL and AMUX_GATEWAY_KEY"))
            .and_then(|(url, key)| Gateway::new(&url, key));
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
        let gateway = std::env::var("AMUX_GATEWAY_URL")
            .ok()
            .zip(std::env::var("AMUX_GATEWAY_KEY").ok())
            .ok_or_else(|| anyhow::anyhow!("Set AMUX_GATEWAY_URL and AMUX_GATEWAY_KEY"))
            .and_then(|(url, key)| Gateway::new(&url, key));
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

    fn withdraw(&mut self, id: u64) {
        for workspace in &mut self.workspaces {
            for (_, pane) in workspace.panes.iter_mut() {
                if let Some(tab) = pane.tabs.iter_mut().find(|tab| tab.id == id) {
                    tab.reminders.consent = false;
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
        let mut queries = Vec::new();
        for (index, workspace) in self.workspaces.iter_mut().enumerate() {
            let mut pending = false;
            for (_, pane) in workspace.panes.iter_mut() {
                for tab in &mut pane.tabs {
                    if !tab.reminders.consent {
                        continue;
                    }
                    let record = ledger
                        .records()
                        .iter()
                        .find(|record| {
                            record.recipient.terminal_id() == tab.id
                                && record.state == DeliveryState::Pending
                        })
                        .cloned();
                    pending |= record.is_some();
                    let (Some(terminal), Some(recipient), Some(registry)) =
                        (&tab.terminal, &mut tab.recipient, &self.registry)
                    else {
                        continue;
                    };
                    if recipient.resolve(registry).is_err() {
                        continue;
                    }
                    if let Some(record) = record {
                        // Only a new authoritative successful query can retire changed work.
                        // Missing/error/expired snapshots pause all submission.
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
                        let now = unix_ms();
                        // A refresh preserves this reminder's stable identity.
                        if let Err(error) = ledger.queue(
                            record.task_id,
                            record.recipient.clone(),
                            DeliveryCause::RecoveryQuery,
                            workspace.gateway.observed_at_ms,
                            now,
                        ) {
                            tab.reminders.status = format!("Reminder storage failed: {error}");
                            continue;
                        }
                        let text = format!(
                            "Pending task {} in project {}. Fetch its current record and check eligibility before deciding to claim.",
                            record.task_id,
                            workspace.gateway.project.trim()
                        );
                        match submit_literal(
                            ledger,
                            &mut tab.reminders.gate,
                            terminal,
                            recipient,
                            record.id,
                            &text,
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
                    } else {
                        // Learn the installed profile before any task is selected.
                        tab.reminders
                            .gate
                            .observe(terminal, recipient, Instant::now());
                    }
                }
            }
            if pending
                && !workspace.gateway.loading
                && workspace
                    .gateway
                    .queried_at
                    .is_none_or(|time| time.elapsed() >= Duration::from_secs(10))
            {
                queries.push(index);
            }
        }
        Task::batch(queries.into_iter().map(|index| self.query(index)))
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
            .push(button("New workspace").on_press(Message::NewWorkspace));
        let toolbar = row![
            button("Shell").on_press(Message::NewTab(None)),
            button("Codex").on_press(Message::NewTab(Some(Provider::Codex))),
            button("Claude").on_press(Message::NewTab(Some(Provider::Claude))),
            button("Split right").on_press(Message::Split(pane_grid::Axis::Vertical)),
            button("Split down").on_press(Message::Split(pane_grid::Axis::Horizontal)),
            button("Close tab").on_press(Message::CloseTab)
        ]
        .spacing(8);
        let grid = pane_grid::PaneGrid::new(&workspace.panes, |id, pane, _| {
            let mut tabs = row![].spacing(4);
            for (i, tab) in pane.tabs.iter().enumerate() { tabs = tabs.push(button(text(format!("{} {} · #{}", if pane.active == i { "●" } else { "" }, tab.title, tab.id))).on_press(Message::SelectTab(id, i))); }
            if let Some(tab) = pane.tabs.get(pane.active)
                && cfg!(target_os = "linux") && tab.provider == Some(Provider::Codex) && tab.recipient.is_some() && self.ledger.is_some() {
                    let terminal_id = tab.id;
                    tabs = tabs.push(checkbox(tab.reminders.consent).label("Allow reminder input + Enter")
                        .on_toggle(move |enabled| Message::Consent(terminal_id, enabled)));
            }
            let body: Element<'_, Message> = match pane.tabs.get(pane.active).and_then(|t| t.terminal.as_ref()) {
                Some(terminal) => {
                    let id = terminal.id;
                    let resize = move |size| Message::Terminal(iced_term::Event::BackendCall(id, iced_term::BackendCommand::Resize(Some(size), None)));
                    iced::widget::Sensor::new(TerminalView::show(terminal).map(Message::Terminal))
                        .key(id).on_show(resize).on_resize(resize).into()
                },
                None => container(text("Process stopped. Launch Shell, Codex or Claude explicitly; previous work is not restarted.")).padding(20).into(),
            };
            let mut header = column![tabs].spacing(4);
            if let Some(tab) = pane.tabs.get(pane.active) && !tab.reminders.status.is_empty() {
                header = header.push(text(&tab.reminders.status).size(11));
            }
            pane_grid::Content::new(body).title_bar(pane_grid::TitleBar::new(header).padding(4))
        }).spacing(5).on_click(Message::Focus).on_resize(8, Message::Resize).width(Length::Fill).height(Length::Fill);
        let tasks = column![
            text("GATEWAY TASKS").size(16),
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
                text(&self.status).size(12)
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
            workspaces,
            active: 0,
            directory: String::new(),
            next_id: 2,
            status: String::new(),
            ledger: None,
            registry: None,
            snapshot_path: None,
            dirty: false,
        }
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
