//! Provisional native terminal/UI candidate. Agent work is never restarted implicitly.
use amux::recipient::TerminalRecipient;
use amux::session::{Layout, SavedTab, SavedWorkspace, Snapshot};
use amux::{gateway::Gateway, identity::Provider};
use iced::widget::{button, column, container, pane_grid, row, scrollable, text, text_input};
use iced::{Element, Length, Subscription, Task, Theme};
use iced_term::{Terminal, TerminalView};
use std::path::PathBuf;

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
    TasksLoaded(Result<serde_json::Value, String>),
    Save,
    Close(iced::window::Id),
}

struct Tab {
    id: u64,
    title: String,
    terminal: Option<Terminal>,
    provider: Option<Provider>,
    recipient: Option<TerminalRecipient>,
}
struct Pane {
    tabs: Vec<Tab>,
    active: usize,
}
struct Workspace {
    directory: PathBuf,
    panes: pane_grid::State<Pane>,
    focused: pane_grid::Pane,
}
struct Desktop {
    workspaces: Vec<Workspace>,
    active: usize,
    directory: String,
    next_id: u64,
    status: String,
    gateway_project: String,
    tasks: String,
    loading: bool,
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
        let mut app = Self {
            workspaces: Vec::new(), active: 0, directory: directory.display().to_string(), next_id: 0,
            status: "Local terminals · gateway optional · automatic reminders not enabled in this candidate".into(),
            gateway_project: std::env::var("AMUX_GATEWAY_PROJECT").unwrap_or_default(),
            tasks: "Set AMUX_GATEWAY_URL and AMUX_GATEWAY_KEY, enter project, then Refresh. Task claims belong to individual agents using `amux tasks`.".into(), loading: false,
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
            app.add_workspace(directory);
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
                            terminal: None,
                            recipient: None,
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
        // A shell is not registered as an agent. Explicit launches go through the
        // same wrapper as the CLI, which allocates a distinct execution identity.
        if let Some(provider) = provider {
            match std::env::current_exe() {
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
                        terminal: None,
                        provider: Some(provider),
                        recipient: None,
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
                let recipient = provider.and_then(|provider| {
                    TerminalRecipient::new(terminal.child_pid()?, provider, directory).ok()
                });
                Tab {
                    id,
                    title,
                    terminal: Some(terminal),
                    provider,
                    recipient,
                }
            }
            Err(error) => {
                self.status = format!("Terminal launch failed: {error}");
                Tab {
                    id,
                    title: format!("{title} — launch failed"),
                    terminal: None,
                    provider,
                    recipient: None,
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
                                        tab.terminal = None;
                                        tab.recipient = None;
                                        tab.title.push_str(" · exited");
                                    }
                                    _ => {}
                                }
                            }
                            return Task::none();
                        }
                    }
                }
            }
            Message::GatewayProject(project) => self.gateway_project = project,
            Message::RefreshTasks if !self.loading => {
                let project = self.gateway_project.trim().to_owned();
                if project.is_empty() {
                    self.tasks = "Enter the gateway project identifier".into();
                    return Task::none();
                }
                let gateway = std::env::var("AMUX_GATEWAY_URL")
                    .ok()
                    .zip(std::env::var("AMUX_GATEWAY_KEY").ok())
                    .ok_or_else(|| anyhow::anyhow!("Set AMUX_GATEWAY_URL and AMUX_GATEWAY_KEY"))
                    .and_then(|(url, key)| Gateway::new(&url, key));
                match gateway {
                    Ok(gateway) => {
                        self.loading = true;
                        return Task::perform(
                            async move { gateway.list(&project).await.map_err(|e| e.to_string()) },
                            Message::TasksLoaded,
                        );
                    }
                    Err(error) => self.tasks = error.to_string(),
                }
            }
            Message::TasksLoaded(result) => {
                self.loading = false;
                self.tasks = match result {
                    Ok(value) => match value.as_array() {
                        Some(tasks) => tasks
                            .iter()
                            .map(|task| {
                                format!(
                                    "{}\n{} · {}\nOwner: {}\n",
                                    task["title"].as_str().unwrap_or("Untitled"),
                                    task["id"].as_str().unwrap_or("?"),
                                    task["status"].as_str().unwrap_or("unknown"),
                                    task["owner_origin"]["session_id"]
                                        .as_str()
                                        .or(task["owner_agent_id"].as_str())
                                        .unwrap_or("unclaimed")
                                )
                            })
                            .collect::<Vec<_>>()
                            .join("\n"),
                        None => "Unexpected gateway task-list shape".into(),
                    },
                    Err(error) => {
                        format!("Gateway unavailable: {error}\nLocal terminals remain available.")
                    }
                };
            }
            Message::RefreshTasks => {}
        }
        Task::none()
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
            for (i, tab) in pane.tabs.iter().enumerate() { tabs = tabs.push(button(text(format!("{} {}", if pane.active == i { "●" } else { "" }, tab.title))).on_press(Message::SelectTab(id, i))); }
            let body: Element<'_, Message> = match pane.tabs.get(pane.active).and_then(|t| t.terminal.as_ref()) {
                Some(terminal) => TerminalView::show(terminal).map(Message::Terminal),
                None => container(text("Process stopped. Launch Shell, Codex or Claude explicitly; previous work is not restarted.")).padding(20).into(),
            };
            pane_grid::Content::new(body).title_bar(pane_grid::TitleBar::new(tabs).padding(4))
        }).spacing(5).on_click(Message::Focus).on_resize(8, Message::Resize).width(Length::Fill).height(Length::Fill);
        let tasks = column![
            text("GATEWAY TASKS").size(16),
            text_input("Gateway project", &self.gateway_project).on_input(Message::GatewayProject),
            button(if self.loading {
                "Loading…"
            } else {
                "Refresh"
            })
            .on_press(Message::RefreshTasks),
            scrollable(text(&self.tasks).size(13))
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
