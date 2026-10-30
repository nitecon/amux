//! Native presentation of the owner's TSX concept. Data comes from the app, never screen text.
use super::*;
use iced::widget::{Space, column, mouse_area, opaque, stack};
use iced::{Alignment, Border, Color};

#[path = "desktop_browser.rs"]
pub(super) mod browser;

const BG: Color = Color::from_rgb8(11, 15, 23);
const PANEL: Color = Color::from_rgb8(14, 19, 29);
const CARD: Color = Color::from_rgb8(18, 26, 39);
const LINE: Color = Color::from_rgb8(30, 41, 59);
const MUTED: Color = Color::from_rgb8(148, 163, 184);
const CYAN: Color = Color::from_rgb8(34, 211, 238);
const GREEN: Color = Color::from_rgb8(16, 185, 129);
const AMBER: Color = Color::from_rgb8(251, 191, 36);
const ORANGE: Color = Color::from_rgb8(249, 115, 22);
const RED: Color = Color::from_rgb8(248, 113, 113);

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub(super) enum View {
    #[default]
    Terminals,
    Files,
    Git,
}
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub(super) enum Filter {
    #[default]
    Active,
    UserReview,
    Completed,
}
#[derive(Debug, Clone)]
pub(super) enum Action {
    CopyTemplate(&'static str),
    ToggleTasks,
    DetachTasks,
    DockTasks,
    Cursor(iced::Point),
    ResizeTaskDrawer,
    FinishTaskDrawerResize,
    TabMenu(pane_grid::Pane, u64),
    DismissTabMenu,
    CloseMenuTab,
    RestartMenuTab,
    ProjectMenu(String),
    DismissProjectMenu,
    /// Clone the menu's project: into the Default Projects directory when true,
    /// otherwise into a folder chosen in the file browser.
    CheckoutProject(bool),
    View(View),
    Filter(Filter),
    TaskSearch(String),
    Events,
    Bell,
    Search(String),
    ClearEvents,
    Single,
    Browse(browser::BrowserMessage),
    Loaded(Uuid, u64, PathBuf, Result<ProjectData, String>),
}
#[derive(Default)]
pub(super) struct State {
    pub(super) task_window: Option<iced::window::Id>,
    tasks_collapsed: bool,
    cursor: iced::Point,
    viewport: Option<iced::Size>,
    task_drawer_fraction: Option<f32>,
    resizing_task_drawer: bool,
    tab_menu: Option<(Uuid, pane_grid::Pane, u64, iced::Point)>,
    project_menu: Option<(String, iced::Point)>,
    mode: View,
    compact: bool,
    filter: Filter,
    task_search: String,
    events: bool,
    bell: bool,
    search: String,
    hidden_events: std::collections::HashSet<String>,
    seen_events: std::collections::HashSet<String>,
    project: Option<(Uuid, PathBuf, Result<ProjectData, String>)>,
    files: Option<(Uuid, browser::FileBrowser)>,
    loading: bool,
    revision: u64,
    /// Gateway-wide presence; `None` until loaded or when the gateway lacks it.
    pub(super) gateway_stats: Option<amux::gateway::PresenceSummary>,
    pub(super) gateway_stats_polled: Option<std::time::Instant>,
    /// Gateway projects with the machines that have them checked out.
    pub(super) projects: Vec<amux::gateway::ProjectOverview>,
    pub(super) projects_polled: Option<std::time::Instant>,
}
impl State {
    fn task_drawer_width(&self) -> f32 {
        let window = self.viewport.map_or(1200.0, |size| size.width);
        let available = (window - 24.0).max(1.0);
        (window * self.task_drawer_fraction.unwrap_or(1.0 / 3.0))
            .clamp(available.min(360.0), available)
    }
}
#[derive(Debug, Clone)]
pub(super) struct ProjectData {
    preview: Option<String>,
    branch: String,
    changes: String,
    commits: String,
}
#[derive(Clone)]
pub(super) struct EventCard {
    pub key: String,
    pub title: String,
    pub detail: String,
    pub gateway: Option<Box<amux::delegation::Event>>,
}

pub(super) fn theme() -> Theme {
    Theme::custom(
        "amux",
        iced::theme::Palette {
            background: BG,
            text: Color::from_rgb8(203, 213, 225),
            primary: CYAN,
            success: GREEN,
            warning: AMBER,
            danger: Color::from_rgb8(248, 113, 113),
        },
    )
}
/// Read-only view of every injected instruction template and its variables.
fn templates_view<'a>() -> Element<'a, Message> {
    use amux_gateway_backend::templates::{TEMPLATES, VARIABLES};
    let mut content = column![
        text("Instruction templates").size(20),
        text("Every instruction amux injects into agent sessions. {name} placeholders are filled when the text is sent; ask for a template change to adjust what agents receive.").size(12).color(MUTED),
        text("Template variables").size(15),
    ]
    .spacing(10);
    for variable in VARIABLES {
        content = content.push(
            row![
                text(format!("{{{}}}", variable.name))
                    .font(iced::Font::MONOSPACE)
                    .size(12)
                    .color(CYAN)
                    .width(150),
                text(variable.description).size(12),
            ]
            .spacing(8),
        );
    }
    for template in TEMPLATES {
        content = content
            .push(
                row![
                    text(template.name).size(15),
                    Space::new().width(Length::Fill),
                    button(text("Copy template").size(12))
                        .on_press(action(Action::CopyTemplate(template.text))),
                ]
                .spacing(8)
                .align_y(Alignment::Center),
            )
            .push(
                container(text(template.text).font(iced::Font::MONOSPACE).size(12))
                    .padding(10)
                    .width(Length::Fill)
                    .style(|_| surface(CARD, true)),
            );
    }
    content.into()
}

/// Settings section label, e.g. GATEWAY OPERATING MODE.
pub(super) fn settings_label<'a>(label: &'a str) -> Element<'a, Message> {
    text(label).size(11).color(MUTED).into()
}
/// Amber notice for settings with side effects.
pub(super) fn settings_callout<'a>(note: &'a str) -> Element<'a, Message> {
    container(
        row![text("!").size(13).color(AMBER), text(note).size(13)]
            .spacing(12)
            .align_y(Alignment::Center),
    )
    .padding([14, 18])
    .width(Length::Fill)
    .style(|_| {
        let mut style = surface(Color::from_rgb8(38, 32, 18), true);
        style.border.color = Color::from_rgb8(113, 83, 22);
        style
    })
    .into()
}
/// Groups related settings in one panel.
pub(super) fn settings_card<'a>(content: impl Into<Element<'a, Message>>) -> Element<'a, Message> {
    container(content)
        .padding([20, 24])
        .width(Length::Fill)
        .style(|_| surface(CARD, true))
        .into()
}
/// Radio-style card for picking one option; disabled when `message` is None.
pub(super) fn mode_card<'a>(
    name: &str,
    title: &'a str,
    description: &'a str,
    selected: bool,
    message: Option<Message>,
) -> Element<'a, Message> {
    button(
        column![
            row![
                icon(name),
                text(title).size(15),
                Space::new().width(Length::Fill),
                text(if selected { "◉" } else { "○" })
                    .size(16)
                    .color(if selected { CYAN } else { MUTED })
            ]
            .spacing(10)
            .align_y(Alignment::Center),
            text(description).size(12).color(MUTED)
        ]
        .spacing(8),
    )
    .on_press_maybe(message)
    .padding([14, 16])
    .width(Length::Fill)
    .style(move |theme, status| {
        let mut style = button_style(selected, theme, status);
        style.text_color = if selected { CYAN } else { Color::WHITE };
        style.border.radius = 7.0.into();
        style
    })
    .into()
}
fn surface(color: Color, rounded: bool) -> container::Style {
    container::Style {
        background: Some(color.into()),
        border: Border {
            color: LINE,
            width: 1.0,
            radius: (if rounded { 7.0 } else { 0.0 }).into(),
        },
        ..Default::default()
    }
}
fn accent_gradient() -> iced::Gradient {
    iced::gradient::Linear::new(iced::Radians(std::f32::consts::FRAC_PI_2))
        .add_stop(0.0, CYAN)
        .add_stop(1.0, Color::from_rgb8(20, 184, 166))
        .into()
}
pub(super) fn primary_action(_: &Theme, status: button::Status) -> button::Style {
    button::Style {
        background: Some(
            accent_gradient()
                .scale_alpha(if matches!(status, button::Status::Disabled) {
                    0.4
                } else {
                    1.0
                })
                .into(),
        ),
        text_color: BG,
        border: Border {
            radius: 5.0.into(),
            ..Default::default()
        },
        ..Default::default()
    }
}
/// Kanban order: in-progress work first; the gateway's ranking is kept otherwise.
fn kanban_order(tasks: &[serde_json::Value]) -> Vec<&serde_json::Value> {
    let mut ordered = tasks.iter().collect::<Vec<_>>();
    ordered.sort_by_key(|task| task["status"] != "in_progress");
    ordered
}
fn status_badge(status: &str) -> Element<'static, Message> {
    let (label, color) = match status {
        "done" => ("COMPLETED", GREEN),
        "todo" => ("QUEUED", AMBER),
        "in_progress" => ("IN PROGRESS", CYAN),
        "user_review" => ("USER REVIEW", AMBER),
        "cancelled" => ("CANCELLED", MUTED),
        _ => ("UNKNOWN", MUTED),
    };
    container(text(label).size(9).color(color))
        .padding([3, 5])
        .style(move |_| {
            let mut tint = color;
            tint.a = 0.12;
            let mut border = color;
            border.a = 0.3;
            container::Style {
                background: Some(tint.into()),
                border: Border {
                    color: border,
                    width: 1.0,
                    radius: 3.0.into(),
                },
                ..Default::default()
            }
        })
        .into()
}
fn icon(name: &str) -> iced::widget::Svg<'static> {
    let bytes: &'static [u8] = match name {
        "terminal" => include_bytes!("../assets/icons/design/Terminal.svg"),
        "settings" => include_bytes!("../assets/icons/design/Settings.svg"),
        "bell" => include_bytes!("../assets/icons/design/Bell.svg"),
        "activity" => include_bytes!("../assets/icons/design/Activity.svg"),
        "folder-git" => include_bytes!("../assets/icons/design/FolderGit2.svg"),
        "plus" => include_bytes!("../assets/icons/design/Plus.svg"),
        "x" => include_bytes!("../assets/icons/design/X.svg"),
        "files" => include_bytes!("../assets/icons/design/Files.svg"),
        "git" => include_bytes!("../assets/icons/design/GitBranch.svg"),
        "columns" => include_bytes!("../assets/icons/design/Columns2.svg"),
        "rows" => include_bytes!("../assets/icons/design/Rows2.svg"),
        "single" => include_bytes!("../assets/icons/design/Maximize2.svg"),
        "server" => include_bytes!("../assets/icons/design/Server.svg"),
        "send" => include_bytes!("../assets/icons/design/Send.svg"),
        "refresh" => include_bytes!("../assets/icons/design/RefreshCw.svg"),
        "folder" => include_bytes!("../assets/icons/design/Folder.svg"),
        "file" => include_bytes!("../assets/icons/design/FileText.svg"),
        "back" => include_bytes!("../assets/icons/design/ArrowLeft.svg"),
        "cpu" => include_bytes!("../assets/icons/design/Cpu.svg"),
        "sparkles" => include_bytes!("../assets/icons/design/Sparkles.svg"),
        _ => include_bytes!("../assets/icons/design/Circle.svg"),
    };
    svg(svg::Handle::from_memory(bytes)).width(14).height(14)
}
pub(super) fn button_style(selected: bool, _: &Theme, status: button::Status) -> button::Style {
    let hover = matches!(status, button::Status::Hovered | button::Status::Pressed);
    button::Style {
        background: Some(
            (if selected {
                Color::from_rgb8(8, 51, 68)
            } else if hover {
                CARD
            } else {
                PANEL
            })
            .into(),
        ),
        text_color: if selected { CYAN } else { MUTED },
        border: Border {
            color: if selected {
                Color::from_rgb8(14, 116, 144)
            } else {
                LINE
            },
            width: 1.0,
            radius: 4.0.into(),
        },
        ..Default::default()
    }
}
fn claude_style(theme: &Theme, status: button::Status) -> button::Style {
    let mut style = button_style(true, theme, status);
    style.background = Some(Color::from_rgb8(51, 41, 18).into());
    style.text_color = AMBER;
    style.border.color = Color::from_rgb8(113, 83, 22);
    style
}
fn control<'a>(
    name: &str,
    label: impl Into<String>,
    message: Message,
    selected: bool,
) -> iced::widget::Button<'a, Message> {
    let mut content = row![].spacing(6).align_y(Alignment::Center);
    if !name.is_empty() {
        content = content.push(icon(name));
    }
    let label = label.into();
    if !label.is_empty() {
        content = content.push(text(label).size(12));
    }
    button(content)
        .on_press(message)
        .padding([6, 9])
        .style(move |theme, status| button_style(selected, theme, status))
}
fn tool_control<'a>(
    name: &str,
    label: &'static str,
    message: Message,
    selected: bool,
    compact: bool,
) -> Element<'a, Message> {
    tooltip(
        control(name, if compact { "" } else { label }, message, selected),
        label,
        tooltip::Position::Bottom,
    )
    .into()
}
fn action(value: Action) -> Message {
    Message::Design(value)
}
fn tab_provider(tab: &Tab) -> Option<Provider> {
    tab.delegation.provider.or(tab.provider)
}
fn active(tab: &Tab) -> bool {
    tab.delegation.server_busy.unwrap_or_else(|| {
        amux::session::output_active(tab.terminal.as_ref().and_then(Terminal::activity_elapsed))
    })
}
fn workspace_name(workspace: &Workspace) -> String {
    if let Some(name) = &workspace.name {
        return name.clone();
    }
    if amux::workspace_launch::home_directory().ok().as_ref() == Some(&workspace.directory)
        && workspace.init_script.is_none()
    {
        "Default".into()
    } else {
        workspace
            .init_script
            .as_deref()
            .unwrap_or(&workspace.directory)
            .file_name()
            .unwrap_or(workspace.directory.as_os_str())
            .to_string_lossy()
            .into_owned()
    }
}
fn tabs(workspace: &Workspace) -> impl Iterator<Item = &Tab> {
    workspace.panes.iter().flat_map(|(_, pane)| &pane.tabs)
}
impl Desktop {
    pub(super) fn design_message(&mut self, message: Action) -> Task<Message> {
        match message {
            Action::CopyTemplate(content) => return iced::clipboard::write(content.to_owned()),
            Action::ToggleTasks => self.design.tasks_collapsed = !self.design.tasks_collapsed,
            Action::DetachTasks => {
                if self.design.task_window.is_none() {
                    let (id, open) = iced::window::open(iced::window::Settings {
                        size: iced::Size::new(800.0, 900.0),
                        min_size: Some(iced::Size::new(500.0, 600.0)),
                        exit_on_close_request: false,
                        ..super::desktop_icon::settings()
                    });
                    self.design.task_window = Some(id);
                    return open.discard();
                } else if let Some(id) = self.design.task_window {
                    return iced::window::gain_focus(id);
                }
            }
            Action::DockTasks => {
                self.design.tasks_collapsed = false;
                if let Some(id) = self.design.task_window.take() {
                    return iced::window::close(id);
                }
            }
            Action::Cursor(position) => {
                self.design.cursor = position;
                if self.design.resizing_task_drawer
                    && let Some(viewport) = self.design.viewport
                {
                    let available = (viewport.width - 24.0).max(1.0);
                    let width =
                        (viewport.width - position.x - 12.0).clamp(available.min(360.0), available);
                    self.design.task_drawer_fraction = Some(width / viewport.width.max(1.0));
                }
            }
            Action::ResizeTaskDrawer => self.design.resizing_task_drawer = true,
            Action::FinishTaskDrawerResize => self.design.resizing_task_drawer = false,
            Action::TabMenu(pane, tab) => {
                let workspace = &self.workspaces[self.active];
                if workspace
                    .panes
                    .get(pane)
                    .is_some_and(|pane| pane.tabs.iter().any(|t| t.id == tab))
                {
                    self.design.tab_menu = Some((workspace.id, pane, tab, self.design.cursor));
                }
            }
            Action::DismissTabMenu => self.design.tab_menu = None,
            Action::ProjectMenu(ident) => {
                self.design.project_menu = Some((ident, self.design.cursor));
            }
            Action::DismissProjectMenu => self.design.project_menu = None,
            Action::CheckoutProject(projects_dir) => {
                let Some((ident, _)) = self.design.project_menu.take() else {
                    return Task::none();
                };
                let Some(origin) = self
                    .design
                    .projects
                    .iter()
                    .find(|p| p.ident == ident)
                    .and_then(|p| p.machines.iter().find_map(|m| m.origin.clone()))
                else {
                    self.status = format!("No machine has reported a Git URL for {ident}.");
                    return Task::none();
                };
                let parent = if projects_dir {
                    let dir = self.preferences.current.projects_dir.trim();
                    if dir.is_empty() {
                        self.status =
                            "Set a Default Projects directory in Settings → General first.".into();
                        return Task::none();
                    }
                    Some(PathBuf::from(dir))
                } else {
                    None
                };
                return self.open_clone_dialog(origin, parent);
            }
            Action::CloseMenuTab => {
                if let Some((workspace, pane, tab, _)) = self.design.tab_menu.take()
                    && self.workspaces[self.active].id == workspace
                {
                    return self.update(Message::CloseTab(pane, tab));
                }
            }
            Action::RestartMenuTab => {
                if let Some((workspace, pane, tab, _)) = self.design.tab_menu.take()
                    && self.workspaces[self.active].id == workspace
                {
                    return self.update(Message::RestartTab(pane, tab));
                }
            }
            Action::View(mode) => {
                self.design.tab_menu = None;
                self.design.mode = mode;
                let workspace = &self.workspaces[self.active];
                match mode {
                    View::Files => {
                        // The Files view uses the shared browser, confined to this workspace.
                        self.design.project = None;
                        self.design.files = Some((
                            workspace.id,
                            browser::FileBrowser::new(
                                workspace.directory.clone(),
                                true,
                                Some(workspace.directory.clone()),
                            ),
                        ));
                    }
                    View::Git => return self.load_project(workspace.directory.clone()),
                    View::Terminals => return self.focus(),
                }
            }
            Action::Filter(filter) => self.design.filter = filter,
            Action::TaskSearch(value) => {
                self.design.task_search = value.chars().take(512).collect();
            }
            Action::Events => {
                self.design
                    .seen_events
                    .extend(self.design_events().into_iter().map(|e| e.key));
                self.design.events = !self.design.events;
                self.design.bell = false;
                if self.design.events {
                    self.design
                        .seen_events
                        .extend(self.design_events().into_iter().map(|e| e.key));
                    return iced::widget::operation::focus("events-search");
                }
                return self.focus();
            }
            Action::Bell => {
                self.design.bell = !self.design.bell;
                self.design.events = false;
            }
            Action::Search(value) => self.design.search = value.chars().take(512).collect(),
            Action::ClearEvents => self
                .design
                .hidden_events
                .extend(self.design_events().into_iter().map(|e| e.key)),
            Action::Single => {
                let w = &mut self.workspaces[self.active];
                if w.panes.maximized().is_some() {
                    w.panes.restore();
                } else {
                    w.panes.maximize(w.focused);
                }
            }
            Action::Browse(message) => {
                let id = self.workspaces[self.active].id;
                if let Some((owner, files)) = &mut self.design.files
                    && *owner == id
                {
                    let preview = match &message {
                        browser::BrowserMessage::Select(path) if path.is_file() => {
                            Some(path.clone())
                        }
                        _ => None,
                    };
                    files.update(message);
                    if let Some(path) = preview {
                        return self.load_project(path);
                    }
                    if files.selected.is_none() {
                        self.design.project = None;
                    }
                }
            }
            Action::Loaded(id, revision, path, result) => {
                if self.workspaces[self.active].id == id && self.design.revision == revision {
                    self.design.project = Some((id, path, result));
                    self.design.loading = false;
                }
            }
        }
        Task::none()
    }
    fn load_project(&mut self, path: PathBuf) -> Task<Message> {
        self.design.loading = true;
        self.design.revision = self.design.revision.wrapping_add(1);
        let revision = self.design.revision;
        let id = self.workspaces[self.active].id;
        let root = self.workspaces[self.active].directory.clone();
        let destination = path.clone();
        Task::perform(
            async move {
                tokio::task::spawn_blocking(move || read_project(&root, &destination))
                    .await
                    .map_err(|e| e.to_string())
                    .and_then(|r| r)
            },
            move |result| action(Action::Loaded(id, revision, path.clone(), result)),
        )
    }
    pub(super) fn design_view(&self) -> Element<'_, Message> {
        let workspace = &self.workspaces[self.active];
        let all_tabs: Vec<_> = self.workspaces.iter().flat_map(tabs).collect();
        let agents = all_tabs
            .iter()
            .filter(|t| tab_provider(t).is_some() && t.terminal.is_some())
            .count();
        let busy = all_tabs
            .iter()
            .filter(|t| tab_provider(t).is_some() && active(t))
            .count();
        let events = self.design_events();
        let unread = events
            .iter()
            .filter(|e| !self.design.events && !self.design.seen_events.contains(&e.key))
            .count();
        let header = container(
            row![
                container(icon("terminal").style(|_, _| svg::Style { color: Some(BG) }))
                    .padding(7)
                    .style(|_| {
                        let mut style = surface(CYAN, true);
                        style.background = Some(accent_gradient().into());
                        style
                    }),
                text("amux").size(15),
                text(if self.design.compact {
                    ""
                } else {
                    "— agent workspaces"
                })
                .size(12)
                .color(MUTED),
                container(
                    row![
                        icon("folder-git"),
                        text(workspace.directory.display().to_string())
                            .font(iced::Font::MONOSPACE)
                            .size(11)
                    ]
                    .spacing(8)
                )
                .padding([5, 9])
                .max_width(340)
                .style(|_| surface(PANEL, false)),
                Space::new().width(Length::Fill),
                tooltip(
                    container(
                        column![
                            text(self.design_gateway_line()).size(11),
                            text(format!("This machine: {agents} agents · {busy} active"))
                                .size(11)
                                .color(MUTED)
                        ]
                        .spacing(1)
                    )
                    .padding([3, 10])
                    .style(|_| surface(CARD, true)),
                    "Gateway: agents on every connected machine; active means busy on the \
                     gateway (updated every 15 seconds). This machine: active means terminal \
                     output within the last 5 seconds.",
                    tooltip::Position::Bottom
                ),
                Space::new().width(Length::Fill),
                control(
                    "activity",
                    format!(
                        "Events{}",
                        if unread > 0 {
                            format!("  {unread}")
                        } else {
                            String::new()
                        }
                    ),
                    action(Action::Events),
                    self.design.events
                ),
                tooltip(
                    control(
                        "bell",
                        if self.updates.attention() { "●" } else { "" },
                        action(Action::Bell),
                        self.updates.attention()
                    ),
                    "App updates",
                    tooltip::Position::Bottom
                ),
                control("settings", "", Message::Preferences(true), false),
            ]
            .spacing(8)
            .align_y(Alignment::Center),
        )
        .padding([7, 16])
        .height(48)
        .style(|_| surface(Color::from_rgb8(15, 21, 32), false));
        let mut sidebar = column![
            row![
                text("LOCAL PROJECTS").size(11).color(MUTED),
                Space::new().width(Length::Fill),
                control("plus", "New", Message::NewWorkspace, true)
            ]
            .align_y(Alignment::Center)
        ]
        .spacing(8);
        for (index, w) in self.workspaces.iter().enumerate() {
            let sessions = tabs(w).count();
            let agents = tabs(w)
                .filter(|t| tab_provider(t).is_some() && t.terminal.is_some())
                .count();
            let busy = tabs(w)
                .filter(|t| tab_provider(t).is_some() && active(t))
                .count();
            let badge = if busy > 0 {
                format!("{busy}/{agents} active")
            } else {
                "idle".into()
            };
            let selected = index == self.active;
            let mut diagnostic = column![
                text(workspace_name(w)).size(14),
                text(w.directory.display().to_string())
                    .size(11)
                    .color(MUTED)
            ]
            .spacing(8);
            if !w.gateway.project.is_empty() {
                // The directory is already shown above.
                for line in self.project_lines(&w.gateway.project, None) {
                    diagnostic = diagnostic.push(text(line).size(11).color(MUTED));
                }
            }
            for tab in tabs(w) {
                diagnostic = diagnostic.push(
                    text(format!(
                        "{} · {} · {}",
                        tab.delegation.agent_name.as_deref().unwrap_or("Terminal"),
                        tab_provider(tab).map(Provider::command).unwrap_or("shell"),
                        if tab.terminal.is_none() {
                            "stopped"
                        } else if active(tab) {
                            "recent I/O"
                        } else {
                            "idle"
                        }
                    ))
                    .size(11),
                );
                for task in self.design_owned_tasks(tab.id, &w.gateway.tasks) {
                    diagnostic =
                        diagnostic.push(text(format!("Claimed: {task}")).size(11).color(CYAN));
                }
                if let Some(elapsed) = tab.terminal.as_ref().and_then(Terminal::activity_elapsed) {
                    diagnostic = diagnostic.push(
                        text(format!("Last terminal output: {}s ago", elapsed.as_secs()))
                            .size(10)
                            .color(MUTED),
                    );
                }
            }
            let card = container(
                row![
                    button(
                        row![
                            text("●")
                                .color(if busy > 0 {
                                    let mut pulse = CYAN;
                                    pulse.a = if (unix_ms() / 400).is_multiple_of(2) {
                                        1.0
                                    } else {
                                        0.5
                                    };
                                    pulse
                                } else {
                                    MUTED
                                })
                                .size(12),
                            column![
                                text(workspace_name(w)).size(13),
                                text(format!(
                                    "{sessions} session{} · {agents} agent{}",
                                    if sessions == 1 { "" } else { "s" },
                                    if agents == 1 { "" } else { "s" }
                                ))
                                .size(10)
                                .color(MUTED)
                            ]
                            .spacing(2),
                            Space::new().width(Length::Fill),
                            text(badge)
                                .size(10)
                                .color(if busy > 0 { CYAN } else { MUTED })
                        ]
                        .spacing(8)
                        .align_y(Alignment::Center)
                    )
                    .style(button::text)
                    .on_press(Message::SelectWorkspace(index))
                    .width(Length::Fill)
                    .padding([10, 6]),
                    button(icon("x"))
                        .style(button::text)
                        .padding(3)
                        .on_press(Message::CloseWorkspace(w.id)),
                ]
                .align_y(Alignment::Center),
            )
            .style(move |_| {
                let mut style = surface(
                    if selected {
                        Color::from_rgb8(13, 39, 51)
                    } else {
                        CARD
                    },
                    true,
                );
                if selected {
                    style.border.color = Color::from_rgb8(8, 145, 178);
                }
                style
            });
            sidebar = sidebar.push(
                tooltip(
                    card,
                    container(diagnostic)
                        .padding(14)
                        .width(300)
                        .style(|_| surface(CARD, true)),
                    tooltip::Position::Right,
                )
                .delay(Duration::from_millis(250)),
            );
            if self.closing_workspace == Some(w.id) {
                sidebar = sidebar.push(
                    container(
                        column![
                            text(
                                "Close this workspace and stop its terminals? Files stay on disk."
                            )
                            .size(12),
                            row![
                                button("Close").on_press(Message::RemoveWorkspace(w.id)),
                                button("Cancel").on_press(Message::CancelCloseWorkspace)
                            ]
                            .spacing(5)
                        ]
                        .spacing(8),
                    )
                    .padding(10)
                    .style(|_| surface(CARD, true)),
                );
            }
        }
        let projects = self.sidebar_projects();
        if !projects.is_empty() {
            sidebar = sidebar.push(text("REMOTE PROJECTS").size(11).color(MUTED));
        }
        for project in projects {
            let mut details = column![].spacing(6);
            for line in self.project_lines(&project.ident, None) {
                details = details.push(text(line).size(11));
            }
            let place = match project.machines.len() {
                0 => "Not checked out".to_owned(),
                1 => project.machines[0].hostname.clone(),
                n => format!("{n} machines"),
            };
            let row = mouse_area(
                container(
                    row![
                        text(&project.ident).size(13).color(MUTED),
                        Space::new().width(Length::Fill),
                        text(place).size(10).color(MUTED)
                    ]
                    .spacing(8)
                    .align_y(Alignment::Center),
                )
                .padding([10, 12])
                .width(Length::Fill)
                .style(|_| surface(CARD, true)),
            )
            .on_right_press(action(Action::ProjectMenu(project.ident.clone())));
            sidebar = sidebar.push(
                tooltip(
                    row,
                    container(details)
                        .padding(14)
                        .width(300)
                        .style(|_| surface(CARD, true)),
                    tooltip::Position::Right,
                )
                .delay(Duration::from_millis(250)),
            );
        }
        let sidebar = container(
            column![
                // Right padding keeps the cards clear of the scrollbar.
                scrollable(container(sidebar).padding(iced::Padding {
                    right: 12.0,
                    ..Default::default()
                }))
                .height(Length::Fill),
                button(
                    row![
                        icon("settings"),
                        text("Settings & Gateway").size(11),
                        Space::new().width(Length::Fill),
                        text("Ctrl+,")
                            .font(iced::Font::MONOSPACE)
                            .size(9)
                            .color(MUTED)
                    ]
                    .spacing(6)
                    .align_y(Alignment::Center)
                )
                .on_press(Message::Preferences(true))
                .padding([7, 9])
                .width(Length::Fill)
                .style(|theme, status| button_style(false, theme, status))
            ]
            .spacing(12),
        )
        .padding(12)
        .width(256)
        .height(Length::Fill)
        .style(|_| surface(PANEL, false));
        let toolbar = container(
            row![
                text(if self.design.compact { "" } else { "LAUNCH:" })
                    .size(9)
                    .color(MUTED),
                control("terminal", "+ Terminal", Message::NewTab(None), false),
                control(
                    "cpu",
                    "+ Codex",
                    Message::NewTab(Some(Provider::Codex)),
                    true
                ),
                control(
                    "sparkles",
                    "+ Claude",
                    Message::NewTab(Some(Provider::Claude)),
                    false
                )
                .style(claude_style),
                tool_control(
                    "files",
                    "Files",
                    action(Action::View(View::Files)),
                    self.design.mode == View::Files,
                    self.design.compact
                ),
                tool_control(
                    "git",
                    "Git",
                    action(Action::View(View::Git)),
                    self.design.mode == View::Git,
                    self.design.compact
                ),
                Space::new().width(Length::Fill),
                tool_control(
                    "single",
                    if workspace.panes.maximized().is_some() {
                        "Restore panes"
                    } else {
                        "Single"
                    },
                    action(Action::Single),
                    workspace.panes.maximized().is_some(),
                    self.design.compact
                ),
                tool_control(
                    "columns",
                    "Split right",
                    Message::Split(pane_grid::Axis::Vertical),
                    false,
                    self.design.compact
                ),
                tool_control(
                    "rows",
                    "Split down",
                    Message::Split(pane_grid::Axis::Horizontal),
                    false,
                    self.design.compact
                ),
            ]
            .spacing(5)
            .align_y(Alignment::Center),
        )
        .padding([5, 12])
        .height(44)
        .style(|_| surface(PANEL, false));
        let center: Element<'_, Message> = if self.design.mode == View::Terminals {
            self.design_terminals()
        } else {
            self.project_view()
        };
        let base: Element<'_, Message> = column![
            header,
            row![
                sidebar,
                column![center, toolbar]
                    .width(Length::Fill)
                    .height(Length::Fill),
                self.task_sidebar()
            ]
            .height(Length::Fill)
        ]
        .into();
        // Modal focus operations and input must not reach widgets underneath.
        let task_drawer = self.design.task_window.is_none()
            && self.workspaces[self.active]
                .gateway
                .detail
                .selected
                .is_some();
        let base = if self.preferences.open || self.workspace_dialog.open {
            inert(base)
        } else {
            base
        };
        let mut layers = stack![base];
        if task_drawer && !self.preferences.open && !self.workspace_dialog.open {
            // A detail drawer floats over the terminal without squeezing it.
            let width = self.design.task_drawer_width();
            let progress = self.workspaces[self.active]
                .gateway
                .detail
                .drawer_progress();
            layers = layers.push(
                container(
                    iced::widget::float(opaque(
                        container(row![
                            mouse_area(
                                container(Space::new())
                                    .width(8)
                                    .height(Length::Fill)
                                    .style(|_| surface(LINE, false))
                            )
                            .interaction(iced::mouse::Interaction::ResizingHorizontally)
                            .on_press(action(Action::ResizeTaskDrawer)),
                            self.design_tasks(true),
                        ])
                        .width(width)
                        .height(Length::Fill),
                    ))
                    .translate(move |_, _| iced::Vector::new(width * (1.0 - progress), 0.0)),
                )
                .align_right(Length::Fill)
                .height(Length::Fill)
                .padding(iced::Padding {
                    top: 48.0,
                    right: 12.0,
                    bottom: 12.0,
                    left: 12.0,
                }),
            );
        }
        if self.design.events {
            layers = layers.push(
                container(opaque(self.events_view()))
                    .align_left(Length::Fill)
                    .padding(iced::Padding {
                        top: 48.0,
                        ..Default::default()
                    })
                    .height(Length::Fill),
            );
        }
        if self.design.bell {
            let summary = if self.updates.restart.required {
                "Update installed. Restart to use it."
            } else if self.updates.attention() {
                "An app update is available."
            } else {
                if self.updates.checking {
                    "Checking for updates…"
                } else if !self.updates.error.is_empty() {
                    "Update check failed. Open Updates for details."
                } else if self
                    .updates
                    .report
                    .as_ref()
                    .is_some_and(|report| report.note.is_some())
                {
                    "Update status needs attention. Open Updates for details."
                } else if self.updates.report.is_none() {
                    "Updates have not been checked yet."
                } else {
                    "You are using the latest checked version."
                }
            };
            layers = layers.push(
                container(opaque(
                    container(
                        column![
                            text("App updates").size(14),
                            text(summary).size(12),
                            text(format!("Running v{}", env!("CARGO_PKG_VERSION")))
                                .size(11)
                                .color(MUTED),
                            if self.preferences.current.auto_update
                                && self.updates.restart.required
                                && self.updates.restart.path.is_some()
                            {
                                control("bell", "Restart now", Message::RestartUpdated, true)
                            } else {
                                control("bell", "View updates", Message::Updates(true), true)
                            }
                        ]
                        .spacing(12),
                    )
                    .padding(16)
                    .width(320)
                    .style(|_| surface(CARD, true)),
                ))
                .align_right(Length::Fill)
                .padding(iced::Padding {
                    top: 48.0,
                    right: 16.0,
                    ..Default::default()
                }),
            );
        }
        if let Some((workspace, pane, tab, position)) = self.design.tab_menu
            && self.workspaces[self.active].id == workspace
        {
            let agent = self.workspaces[self.active]
                .panes
                .get(pane)
                .and_then(|pane| pane.tabs.iter().find(|t| t.id == tab))
                .and_then(tab_provider)
                .is_some();
            let viewport = self
                .design
                .viewport
                .unwrap_or(iced::Size::new(1600.0, 1000.0));
            let position = iced::Point::new(
                position.x.clamp(0.0, (viewport.width - 180.0).max(0.0)),
                position.y.clamp(0.0, (viewport.height - 84.0).max(0.0)),
            );
            layers = layers
                .push(
                    mouse_area(Space::new().width(Length::Fill).height(Length::Fill))
                        .on_press(action(Action::DismissTabMenu))
                        .on_right_press(action(Action::DismissTabMenu)),
                )
                .push(
                    container(opaque(
                        container({
                            let item = |glyph, label, message| {
                                button(row![icon(glyph), text(label).size(12)].spacing(8))
                                    .on_press(action(message))
                                    .padding([8, 12])
                                    .width(Length::Fill)
                                    .style(|theme, status| button_style(false, theme, status))
                            };
                            let mut menu = column![];
                            if agent {
                                menu = menu.push(item(
                                    "refresh",
                                    "Restart session",
                                    Action::RestartMenuTab,
                                ));
                            }
                            menu.push(item("x", "Close terminal", Action::CloseMenuTab))
                        })
                        .padding(4)
                        .width(180)
                        .style(|_| surface(CARD, true)),
                    ))
                    .padding(iced::Padding {
                        top: position.y,
                        left: position.x,
                        ..Default::default()
                    }),
                );
        }
        if let Some((_, position)) = &self.design.project_menu {
            let viewport = self
                .design
                .viewport
                .unwrap_or(iced::Size::new(1600.0, 1000.0));
            let position = iced::Point::new(
                position.x.clamp(0.0, (viewport.width - 220.0).max(0.0)),
                position.y.clamp(0.0, (viewport.height - 84.0).max(0.0)),
            );
            let item = |label, message| {
                button(row![icon("folder-git"), text(label).size(12)].spacing(8))
                    .on_press(action(message))
                    .padding([8, 12])
                    .width(Length::Fill)
                    .style(|theme, status| button_style(false, theme, status))
            };
            layers = layers
                .push(
                    mouse_area(Space::new().width(Length::Fill).height(Length::Fill))
                        .on_press(action(Action::DismissProjectMenu))
                        .on_right_press(action(Action::DismissProjectMenu)),
                )
                .push(
                    container(opaque(
                        container(column![
                            item("Check out in project dir", Action::CheckoutProject(true)),
                            item("Check out at location", Action::CheckoutProject(false)),
                        ])
                        .padding(4)
                        .width(220)
                        .style(|_| surface(CARD, true)),
                    ))
                    .padding(iced::Padding {
                        top: position.y,
                        left: position.x,
                        ..Default::default()
                    }),
                );
        }
        if self.workspace_dialog.open {
            let scrim = mouse_area(
                container(Space::new())
                    .width(Length::Fill)
                    .height(Length::Fill)
                    .style(|_| container::Style {
                        background: Some(Color::from_rgba8(0, 0, 0, 0.7).into()),
                        ..Default::default()
                    }),
            )
            .on_press(Message::Workspace(WorkspaceMessage::Cancel));
            layers = layers.push(scrim).push(
                container(opaque(
                    container(self.workspace_dialog_view())
                        .width(680)
                        .height(640)
                        .style(|_| surface(CARD, true)),
                ))
                .center(Length::Fill),
            );
        }
        if self.preferences.open {
            let scrim = mouse_area(
                container(Space::new())
                    .width(Length::Fill)
                    .height(Length::Fill)
                    .style(|_| container::Style {
                        background: Some(Color::from_rgba8(0, 0, 0, 0.7).into()),
                        ..Default::default()
                    }),
            )
            .on_press(Message::Preferences(false));
            layers = layers
                .push(scrim)
                .push(container(opaque(self.settings_modal())).center(Length::Fill));
        }
        let mut notices = column![].spacing(8).width(400);
        let show_harness =
            self.harness.needs_notice() && !self.preferences.open && !self.workspace_dialog.open;
        if show_harness {
            notices = notices.push(
                container(
                    column![
                        row![
                            text("Agent instructions").size(14),
                            Space::new().width(Length::Fill),
                            button("×").on_press(Message::DismissHarnessNotice)
                        ]
                        .align_y(Alignment::Center),
                        self.harness_suggestion(),
                    ]
                    .spacing(6),
                )
                .padding(12)
                .style(|_| surface(CARD, true)),
            );
        }
        for toast in &self.toasts.items {
            notices = notices.push(
                container(
                    column![
                        row![
                            text(&toast.title).size(14).color(CYAN),
                            Space::new().width(Length::Fill),
                            button("×").on_press(Message::DismissToast(toast.id))
                        ]
                        .align_y(Alignment::Center),
                        text(&toast.body).size(12),
                    ]
                    .spacing(6),
                )
                .padding(12)
                .width(Length::Fill)
                .style(|_| surface(CARD, true)),
            );
        }
        if show_harness || !self.toasts.items.is_empty() {
            layers = layers.push(
                container(opaque(notices))
                    .padding(16)
                    .align_right(Length::Fill)
                    .align_bottom(Length::Fill),
            );
        }
        container(layers)
            .width(Length::Fill)
            .height(Length::Fill)
            .style(|_| container::Style {
                background: Some(BG.into()),
                ..Default::default()
            })
            .into()
    }
    fn design_terminals(&self) -> Element<'_, Message> {
        let w = &self.workspaces[self.active];
        pane_grid::PaneGrid::new(&w.panes, |pane_id, pane, _| {
            let mut strip = row![].spacing(3);
            for (index, tab) in pane.tabs.iter().enumerate() {
                let label = match tab_provider(tab) {
                    Some(Provider::Codex) => "Codex",
                    Some(Provider::Claude) => "Claude",
                    None => "Terminal",
                };
                let selected = index == pane.active;
                let subtitle = format!(
                    "{} · {}{}",
                    tab.delegation.agent_name.as_deref().unwrap_or(
                        if tab_provider(tab).is_some() {
                            "Agent"
                        } else {
                            "Local shell"
                        }
                    ),
                    workspace_name(w),
                    if tab.terminal.is_none() {
                        " · stopped"
                    } else {
                        ""
                    }
                );
                strip = strip.push(
                    mouse_area(
                        button(
                            column![
                                row![icon("terminal"), text(label).size(12),]
                                    .spacing(6)
                                    .align_y(Alignment::Center),
                                tooltip(
                                    text(short_title(&subtitle)).size(10).color(MUTED),
                                    container(
                                        text(format!("{subtitle}\n{}", w.directory.display()))
                                            .size(11)
                                    )
                                    .padding(8)
                                    .style(|_| surface(CARD, true)),
                                    tooltip::Position::Bottom
                                ),
                            ]
                            .spacing(2),
                        )
                        .on_press(Message::SelectTab(pane_id, index))
                        .padding([5, 8])
                        .style(move |theme, status| {
                            if selected && tab_provider(tab) == Some(Provider::Claude) {
                                claude_style(theme, status)
                            } else {
                                button_style(selected, theme, status)
                            }
                        }),
                    )
                    .on_right_press(action(Action::TabMenu(pane_id, tab.id))),
                );
            }
            let identity = pane
                .tabs
                .get(pane.active)
                .filter(|tab| tab_provider(tab).is_some())
                .map(|tab| {
                    format!(
                        "ID: {} · {}",
                        tab.delegation.agent_name.as_deref().unwrap_or("Assigning…"),
                        w.gateway.project
                    )
                });
            let mut title = column![scrollable(strip).horizontal().height(55)].spacing(2);
            if let Some(identity) = identity {
                title = title.push(
                    text(identity)
                        .size(10)
                        .font(iced::Font::MONOSPACE)
                        .color(CYAN),
                );
            }
            if let Some(tab) = pane.tabs.get(pane.active) {
                let status = tab.delegation.status();
                title = title.push(
                    row![
                        text(if status.is_empty() {
                            if tab.terminal.is_some() {
                                "Terminal running"
                            } else {
                                "Terminal stopped"
                            }
                        } else {
                            status
                        })
                        .size(10)
                        .color(MUTED),
                        Space::new().width(Length::Fill),
                        text(if active(tab) {
                            "● Recent output"
                        } else {
                            "○ Idle"
                        })
                        .size(10)
                        .color(if active(tab) { CYAN } else { MUTED })
                    ]
                    .padding([3, 8]),
                );
            }
            let body: Element<'_, Message> =
                match pane.tabs.get(pane.active).and_then(|t| t.terminal.as_ref()) {
                    Some(terminal) => {
                        let id = terminal.id;
                        let resize = move |size| {
                            Message::Terminal(iced_term::Event::BackendCall(
                                id,
                                iced_term::BackendCommand::Resize(Some(size), None),
                            ))
                        };
                        iced::widget::Sensor::new(
                            TerminalView::show(terminal).map(Message::Terminal),
                        )
                        .key(id)
                        .on_show(resize)
                        .on_resize(resize)
                        .into()
                    }
                    None => {
                        let mut stopped = column![
                            text("Terminal stopped").size(16),
                            text("Launch a Terminal, Codex or Claude session from the toolbar.")
                                .size(12)
                        ]
                        .spacing(12);
                        if let Some(tab) = pane.tabs.get(pane.active) {
                            stopped = stopped.push(text(&tab.title).size(12).color(AMBER));
                            if let Some(output) = &tab.stopped_output {
                                stopped = stopped.push(
                                    scrollable(text(output).font(iced::Font::MONOSPACE).size(12))
                                        .height(Length::Fill),
                                );
                            }
                        }
                        container(stopped).padding(20).height(Length::Fill).into()
                    }
                };
            pane_grid::Content::new(body)
                .title_bar(pane_grid::TitleBar::new(title).padding(0))
                .style(|_| surface(Color::from_rgb8(7, 10, 15), false))
        })
        .spacing(1)
        .on_click(Message::Focus)
        .on_resize(8, Message::Resize)
        .width(Length::Fill)
        .height(Length::Fill)
        .into()
    }
    fn task_sidebar(&self) -> Element<'_, Message> {
        if self.design.task_window.is_none()
            && self.workspaces[self.active]
                .gateway
                .detail
                .selected
                .is_some()
        {
            return Space::new().width(320).height(Length::Fill).into();
        }
        if self.design.task_window.is_some() || self.design.tasks_collapsed {
            return container(
                column![
                    control("back", "", action(Action::DockTasks), false),
                    control("single", "", action(Action::DetachTasks), false),
                ]
                .spacing(8),
            )
            .padding(5)
            .height(Length::Fill)
            .style(|_| surface(PANEL, false))
            .into();
        }
        self.design_tasks(false)
    }

    pub(super) fn design_tasks(&self, expanded: bool) -> Element<'_, Message> {
        let width = if expanded {
            Length::Fill
        } else {
            Length::Fixed(320.0)
        };
        let collapse = tooltip(
            control(
                "",
                if expanded { "‹" } else { "›" },
                action(if expanded {
                    Action::DockTasks
                } else {
                    Action::ToggleTasks
                }),
                false,
            ),
            text(if expanded {
                "Return tasks to sidebar"
            } else {
                "Collapse task panel"
            }),
            tooltip::Position::Bottom,
        );
        let detached = self.design.task_window.is_some();
        let detach = tooltip(
            control(
                "single",
                "",
                action(if detached {
                    Action::DockTasks
                } else {
                    Action::DetachTasks
                }),
                false,
            ),
            text(if detached {
                "Return tasks to sidebar"
            } else {
                "Open tasks in a separate window"
            }),
            tooltip::Position::Bottom,
        );
        let panel = &self.workspaces[self.active].gateway;
        if panel.detail.selected.is_some() {
            // The detail view's own back button returns to the list, so the collapse arrow is hidden.
            return container(column![row![detach], self.task_detail_view()].spacing(8))
                .padding(20)
                .width(width)
                .height(Length::Fill)
                .style(|_| surface(PANEL, true))
                .into();
        }
        let filter = |label, kind| {
            control(
                "",
                label,
                action(Action::Filter(kind)),
                self.design.filter == kind,
            )
            .width(Length::Fill)
        };
        let searching = !self
            .design
            .task_search
            .trim()
            .trim_start_matches('#')
            .is_empty();
        let states: Element<'_, Message> = if searching {
            text("Search results · all task states")
                .size(11)
                .color(MUTED)
                .into()
        } else {
            row![
                filter("Active", Filter::Active),
                filter("User review", Filter::UserReview),
                filter("Completed", Filter::Completed)
            ]
            .spacing(3)
            .into()
        };
        let search = row![
            text_input("Filter by task ID, title or tag…", &self.design.task_search)
                .on_input(|value| action(Action::TaskSearch(value)))
                .width(Length::Fill),
            button(text("Clear").size(11)).on_press_maybe(
                (!self.design.task_search.is_empty())
                    .then(|| { action(Action::TaskSearch(String::new())) })
            )
        ]
        .spacing(6)
        .align_y(Alignment::Center);
        let header = column![
            row![
                icon("server"),
                text("GATEWAY TASKS").size(12),
                Space::new().width(Length::Fill),
                row![collapse, detach].spacing(4),
                button(text("Raw Events ›").size(10).color(CYAN))
                    .style(button::text)
                    .on_press(action(Action::Events))
            ]
            .spacing(7)
            .align_y(Alignment::Center),
            container(
                row![
                    text(format!(
                        "{} · {} local agents",
                        if panel.loaded_at.is_some() {
                            "Tasks loaded"
                        } else if panel.loading {
                            "Connecting…"
                        } else {
                            "Tasks offline"
                        },
                        tabs(&self.workspaces[self.active])
                            .filter(|t| tab_provider(t).is_some() && t.terminal.is_some())
                            .count()
                    ))
                    .size(10)
                    .color(MUTED),
                    Space::new().width(Length::Fill),
                    control("refresh", "", Message::RefreshTasks, false)
                ]
                .align_y(Alignment::Center)
            )
            .padding(5)
            .style(|_| surface(CARD, true)),
            search,
            states,
        ]
        .spacing(10);
        let mut cards = column![text(&panel.status).size(11).color(MUTED),].spacing(8);
        let mut count = 0;
        for task in kanban_order(&panel.tasks) {
            let status = task["status"].as_str().unwrap_or("unknown");
            if !matches_task_filter(task, self.design.filter, &self.design.task_search) {
                continue;
            }
            count += 1;
            let id = task["id"].as_str().unwrap_or("?");
            let owner = self.design_task_owner(task);
            // Work in progress by an agent outside this app stays visible at a glance.
            let elsewhere = self.task_owned_elsewhere(task);
            let task_id = Uuid::parse_str(id).ok();
            let manage = |action: fn(Uuid) -> super::desktop_tasks::Action| {
                task_id.map(|id| Message::ManageTask(action(id)))
            };
            // The assignee line is the Assign control for work that can still be assigned.
            let assignee = button(
                if elsewhere {
                    text(format!("→ {}", self.external_task_owner(task))).color(RED)
                } else {
                    text(format!("→ {owner}")).color(MUTED)
                }
                .size(10),
            )
            .padding(0)
            .style(button::text)
            .on_press_maybe(
                matches!(status, "todo" | "in_progress")
                    .then(|| manage(super::desktop_tasks::Action::AssignTask))
                    .flatten(),
            );
            let item = column![
                row![
                    text(format!("#{}", id.chars().take(8).collect::<String>()))
                        .size(10)
                        .font(iced::Font::MONOSPACE)
                        .color(CYAN),
                    Space::new().width(Length::Fill),
                    status_badge(status),
                ],
                text(task["title"].as_str().unwrap_or("Untitled")).size(12),
                assignee,
                text(format!(
                    "Project: {}{}",
                    panel.project,
                    task["updated_at"]
                        .as_u64()
                        .map(|ms| format!(
                            " · updated {}s ago",
                            unix_ms().saturating_sub(ms) / 1000
                        ))
                        .unwrap_or_default()
                ))
                .size(10)
                .font(iced::Font::MONOSPACE)
                .color(MUTED),
            ]
            .spacing(9);
            // Clicking anywhere on the card opens the task; hovering outlines it.
            cards = cards.push(
                button(item)
                    .padding(11)
                    .width(Length::Fill)
                    .on_press_maybe(manage(super::desktop_tasks::Action::Open))
                    .style(move |theme: &Theme, state| {
                        let card = surface(CARD, true);
                        let mut border = card.border;
                        if elsewhere {
                            border.color = ORANGE;
                        }
                        if matches!(state, button::Status::Hovered | button::Status::Pressed) {
                            border.color = CYAN;
                        }
                        button::Style {
                            background: card.background,
                            text_color: theme.palette().text,
                            border,
                            ..Default::default()
                        }
                    }),
            );
        }
        if count == 0 {
            cards = cards.push(
                text(if panel.loading {
                    "Loading gateway tasks…"
                } else if searching {
                    "No tasks match this search."
                } else {
                    "No tasks in this view."
                })
                .size(12)
                .color(MUTED),
            );
        }
        let composer = container(
            column![
                row![
                    icon("send"),
                    text("DISPATCH NEW GATEWAY TASK").size(10).color(CYAN),
                    Space::new().width(Length::Fill),
                    tooltip(
                        button(text("New release").size(11))
                            .on_press_maybe(panel.can_prepare_release().then_some(Message::PrepareReleaseTask)),
                        text("Prepare a release request with optional notes. Requires an empty task draft."),
                        tooltip::Position::Top,
                    ),
                ]
                .spacing(6)
                .align_y(Alignment::Center),
                text(format!(
                    "Project: {} · repository matching required",
                    if panel.project.is_empty() {
                        "resolving…"
                    } else {
                        &panel.project
                    }
                ))
                .size(10)
                .color(MUTED),
                self.task_form(),
            ]
            .spacing(9),
        )
        .padding(12)
        .width(Length::Fill)
        .style(|_| surface(PANEL, false));
        container(
            column![
                container(header).padding(12),
                container(scrollable(cards).height(Length::Fill)).padding([0, 12]),
                composer
            ]
            .spacing(4),
        )
        .width(width)
        .height(Length::Fill)
        .style(|_| surface(PANEL, false))
        .into()
    }
    fn events_view(&self) -> Element<'_, Message> {
        let mut entries = column![].spacing(8);
        let search = self.design.search.to_lowercase();
        for event in self.design_events().into_iter().rev().filter(|e| {
            !self.design.hidden_events.contains(&e.key)
                && format!("{} {}", e.title, e.detail)
                    .to_lowercase()
                    .contains(&search)
        }) {
            if let Some(gateway) = &event.gateway {
                let summary = gateway.summary.as_deref();
                let sender = summary
                    .map(|s| s.sender.as_str())
                    .unwrap_or("Unknown sender");
                let title = summary
                    .map(|s| s.title.as_str())
                    .filter(|s| !s.is_empty())
                    .unwrap_or("Title unavailable");
                let preview = summary.map(|s| s.preview.as_str()).unwrap_or("");
                let host = summary
                    .map(|s| s.task_host.as_str())
                    .filter(|s| !s.is_empty())
                    .unwrap_or("unavailable");
                let sender_host = summary
                    .map(|s| s.sender_host.as_str())
                    .filter(|s| !s.is_empty())
                    .unwrap_or("unavailable");
                let from_project = summary
                    .map(|s| s.source_project.as_str())
                    .filter(|s| !s.is_empty())
                    .unwrap_or(&gateway.project);
                let owner_host = summary
                    .map(|s| s.owner_host.as_str())
                    .filter(|s| !s.is_empty())
                    .map(|host| format!(" · Owner host: {host}"))
                    .unwrap_or_default();
                let initials = sender
                    .split_whitespace()
                    .take(2)
                    .filter_map(|s| s.chars().next())
                    .collect::<String>();
                let age = (unix_ms() as i64).saturating_sub(gateway.created_at).max(0) / 60_000;
                let bubble = column![
                    row![
                        text(sender.to_owned()).size(12).color(CYAN),
                        Space::new().width(Length::Fill),
                        text(format!("{age}m ago")).size(10).color(MUTED)
                    ]
                    .spacing(8),
                    row![
                        text(gateway.heading().to_owned()).size(11),
                        Space::new().width(Length::Fill),
                        status_badge(gateway.state())
                    ]
                    .spacing(6),
                    text(title.to_owned()).size(14),
                    text(preview.to_owned()).size(12),
                    text(format!("From {from_project} · Sender host: {sender_host}"))
                        .size(10)
                        .color(MUTED),
                    text(format!(
                        "Task project: {} · Recorded task host: {host}{owner_host}",
                        gateway.project
                    ))
                    .size(10)
                    .color(MUTED),
                    text(format!("TaskID: {}", gateway.task))
                        .font(iced::Font::MONOSPACE)
                        .size(10)
                        .color(MUTED),
                    tooltip(
                        text("Delivery details").size(10).color(MUTED),
                        text(event.detail).size(11),
                        tooltip::Position::Bottom
                    ),
                ]
                .spacing(8);
                entries = entries.push(
                    row![
                        container(text(initials).size(11).color(CYAN))
                            .padding(7)
                            .style(|_| surface(PANEL, true)),
                        container(bubble)
                            .padding(12)
                            .width(Length::Fill)
                            .style(|_| surface(CARD, true)),
                    ]
                    .spacing(8)
                    .align_y(Alignment::Start),
                );
            } else {
                entries = entries.push(
                    container(
                        column![
                            text(event.title).size(11).color(CYAN),
                            text(event.detail).size(11)
                        ]
                        .spacing(8),
                    )
                    .padding(12)
                    .width(Length::Fill)
                    .style(|_| surface(CARD, true)),
                );
            }
        }
        container(
            column![
                row![
                    icon("activity"),
                    text("Gateway messages").size(14),
                    Space::new().width(Length::Fill),
                    control("x", "", action(Action::Events), false)
                ]
                .spacing(8)
                .align_y(Alignment::Center),
                text("Task state and previews from each event; hover for delivery details.")
                    .size(11)
                    .color(MUTED),
                row![
                    text_input("Search agent, UUID or message…", &self.design.search)
                        .id("events-search")
                        .on_input(|s| action(Action::Search(s)))
                        .size(12),
                    button(text("Clear").size(11))
                        .style(button::text)
                        .on_press(action(Action::ClearEvents))
                ]
                .spacing(8),
                scrollable(entries).height(Length::Fill),
                text("Clear hides this view only. Delivery records remain saved.")
                    .size(10)
                    .color(MUTED),
            ]
            .spacing(12),
        )
        .padding(16)
        .width(440)
        .height(Length::Fill)
        .style(|_| surface(Color::from_rgb8(16, 23, 37), false))
        .into()
    }
    fn settings_modal(&self) -> Element<'_, Message> {
        let nav = |name, label, kind| {
            let selected = self.preferences.tab == kind;
            button(
                row![icon(name), text(label).size(13)]
                    .spacing(10)
                    .align_y(Alignment::Center),
            )
            .on_press(Message::PreferencesTab(kind))
            .padding([9, 12])
            .width(Length::Fill)
            .style(move |theme, status| {
                let mut style = button_style(selected, theme, status);
                if !selected {
                    style.border.color = Color::TRANSPARENT;
                }
                style
            })
        };
        let group = |label| {
            container(text(label).size(10).color(MUTED)).padding(iced::Padding {
                top: 12.0,
                left: 12.0,
                bottom: 2.0,
                ..Default::default()
            })
        };
        let body: Element<'_,Message>=match self.preferences.tab {
            PreferencesTab::Gateway=>self.preferences_view(),
            PreferencesTab::Updates=>self.updates_view(),
            PreferencesTab::Harness=>self.harness_view(),
            PreferencesTab::Templates=>templates_view(),
            PreferencesTab::General=>column![
                text("Workspaces & Terminals").size(22),
                settings_card(column![
                checkbox(self.preferences.draft.notify_exits).label("Notify on process exit").on_toggle(Message::ExitNotifications),
                text("Notifications report terminal process exits.").size(12).color(MUTED),
                checkbox(self.preferences.draft.auto_update).label("Install updates automatically").on_toggle(Message::AutoUpdate),
                text("Updates download in the background. Click the update alert to restart amux and resume its sessions.").size(12).color(MUTED),
                text("Default Projects directory").size(13),
                text_input("Folder where projects from other machines are checked out", &self.preferences.draft.projects_dir).on_input(Message::ProjectsDir),
                text("Right-click a grayed-out project in the sidebar and choose Check out in project dir to clone it here.").size(12).color(MUTED),
                checkbox(self.preferences.draft.auto_resume).label("Resume agent sessions on startup").on_toggle(Message::AutoResume),
                text("Saved running terminals reopen on startup. This option also reopens agent tabs with their native conversation resume.").size(12).color(MUTED),
                ].spacing(14)),
                text("Repository matching is required for gateway delivery.").size(12).color(CYAN)
            ].spacing(18).into(),
        };
        let mode = match self.preferences.current.mode {
            GatewayMode::Server => "Server mode",
            GatewayMode::Client => "Client mode",
        };
        let header = container(
            row![
                container(icon("settings").width(18).height(18))
                    .padding(9)
                    .style(|_| surface(Color::from_rgb8(8, 51, 68), true)),
                column![
                    text("amux Settings & Gateway Preferences").size(16),
                    text(format!("v{} · {mode}", env!("CARGO_PKG_VERSION")))
                        .font(iced::Font::MONOSPACE)
                        .size(11)
                        .color(MUTED)
                ]
                .spacing(3),
                Space::new().width(Length::Fill),
                control("x", "", Message::Preferences(false), false)
            ]
            .spacing(12)
            .align_y(Alignment::Center),
        )
        .padding([14, 20])
        .style(|_| surface(Color::from_rgb8(21, 30, 47), false));
        let sidebar = container(
            column![
                group("INFRASTRUCTURE"),
                nav("server", "Gateway Server", PreferencesTab::Gateway),
                nav(
                    "terminal",
                    "Workspaces & Terminals",
                    PreferencesTab::General
                ),
                group("AI & AUTOMATION"),
                nav("cpu", "Agent Instructions", PreferencesTab::Harness),
                nav("file", "Instruction Templates", PreferencesTab::Templates),
                group("SYSTEM"),
                nav("refresh", "Updates", PreferencesTab::Updates),
            ]
            .spacing(4),
        )
        .padding(14)
        .width(230)
        .height(Length::Fill)
        .style(|_| surface(PANEL, false));
        // Right padding keeps content clear of the scrollbar.
        let content = scrollable(container(body).padding(iced::Padding {
            top: 24.0,
            right: 32.0,
            bottom: 24.0,
            left: 28.0,
        }))
        .width(Length::Fill)
        .height(Length::Fill);
        let footer = container(
            row![
                text(&self.preferences.status).size(12).color(
                    if self.preferences.status.starts_with("Not saved") {
                        AMBER
                    } else {
                        MUTED
                    }
                ),
                Space::new().width(Length::Fill),
                control("x", "Cancel", Message::Preferences(false), false),
                control("settings", "Save & Close", Message::SavePreferences, true)
            ]
            .spacing(8)
            .align_y(Alignment::Center),
        )
        .padding([12, 20])
        .style(|_| surface(Color::from_rgb8(21, 30, 47), false));
        container(column![
            header,
            row![sidebar, content].height(Length::Fill),
            footer
        ])
        .width(Length::Fill)
        .max_width(940)
        .height(Length::Fill)
        .max_height(680)
        .style(|_| surface(Color::from_rgb8(17, 24, 38), true))
        .into()
    }
    fn project_view(&self) -> Element<'_, Message> {
        let root = &self.workspaces[self.active];
        let mut content = column![
            row![
                text(if self.design.mode == View::Files {
                    "Workspace Explorer"
                } else {
                    "Git Worktree Status"
                })
                .size(18),
                Space::new().width(Length::Fill),
                control(
                    "back",
                    "Back to terminals",
                    action(Action::View(View::Terminals)),
                    false
                )
            ]
            .align_y(Alignment::Center),
            text(root.directory.display().to_string())
                .font(iced::Font::MONOSPACE)
                .size(11)
                .color(MUTED)
        ]
        .spacing(14);
        if self.design.mode == View::Files {
            return self.files_view(content);
        }
        if self.design.loading {
            content = content.push(text("Loading project…").size(12));
        }
        if let Some((id, _, result)) = &self.design.project
            && *id == root.id
        {
            match result {
                Err(error) => content = content.push(text(error).size(12).color(AMBER)),
                Ok(data) => {
                    if self.design.mode == View::Git {
                        content = content
                            .push(
                                text(format!("Branch: {}", data.branch))
                                    .size(14)
                                    .color(CYAN),
                            )
                            .push(text("Working tree").size(13))
                            .push(
                                text(if data.changes.is_empty() {
                                    "Clean working tree"
                                } else {
                                    &data.changes
                                })
                                .font(iced::Font::MONOSPACE)
                                .size(12),
                            )
                            .push(text("Recent commits").size(13))
                            .push(text(&data.commits).font(iced::Font::MONOSPACE).size(12));
                    }
                }
            }
        }
        container(scrollable(content).height(Length::Fill))
            .padding(24)
            .width(Length::Fill)
            .height(Length::Fill)
            .into()
    }
}
impl Desktop {
    fn files_view<'a>(&'a self, header: iced::widget::Column<'a, Message>) -> Element<'a, Message> {
        let root = &self.workspaces[self.active];
        let Some((_, files)) = self.design.files.as_ref().filter(|(id, _)| *id == root.id) else {
            return container(header).padding(24).into();
        };
        // A selected file shows its bounded preview under the browser.
        let preview: Element<'_, Message> = match &self.design.project {
            Some((id, path, result)) if *id == root.id && files.selected.as_ref() == Some(path) => {
                let body = match result {
                    Ok(data) => text(data.preview.as_deref().unwrap_or(""))
                        .font(iced::Font::MONOSPACE)
                        .size(12),
                    Err(error) => text(error).size(12).color(AMBER),
                };
                container(scrollable(body).height(240))
                    .padding(10)
                    .width(Length::Fill)
                    .style(|_| surface(CARD, true))
                    .into()
            }
            _ if self.design.loading => text("Loading preview…").size(12).into(),
            _ => Space::new().height(0).into(),
        };
        container(
            header
                .push(files.view(|m| action(Action::Browse(m)), preview))
                .height(Length::Fill),
        )
        .padding(24)
        .width(Length::Fill)
        .height(Length::Fill)
        .into()
    }
}
fn matches_task_filter(task: &serde_json::Value, filter: Filter, query: &str) -> bool {
    let query = query.trim().trim_start_matches('#').to_lowercase();
    if query.is_empty() {
        return matches_filter(filter, task["status"].as_str().unwrap_or("unknown"));
    }
    let mut content = format!(
        "{} {}",
        task["id"].as_str().unwrap_or_default(),
        task["title"].as_str().unwrap_or_default()
    );
    if let Some(labels) = task["labels"].as_array() {
        for label in labels.iter().filter_map(serde_json::Value::as_str) {
            content.push(' ');
            content.push_str(label);
        }
    }
    let content = content.to_lowercase();
    query.split_whitespace().all(|term| content.contains(term))
}

fn matches_filter(filter: Filter, status: &str) -> bool {
    match filter {
        Filter::Active => matches!(status, "todo" | "in_progress"),
        Filter::UserReview => status == "user_review",
        Filter::Completed => matches!(status, "done" | "cancelled"),
    }
}
fn read_project(root: &std::path::Path, path: &std::path::Path) -> Result<ProjectData, String> {
    use std::io::Read;
    let root = root.canonicalize().map_err(|e| e.to_string())?;
    let path = path.canonicalize().map_err(|e| e.to_string())?;
    if !path.starts_with(&root) {
        return Err("Choose a path inside this workspace.".into());
    }
    let root = amux::workspace_launch::external_path(root);
    let path = amux::workspace_launch::external_path(path);
    let preview = if path.is_dir() {
        None
    } else {
        let mut bytes = Vec::new();
        std::fs::File::open(&path)
            .map_err(|e| e.to_string())?
            .take(64 * 1024 + 1)
            .read_to_end(&mut bytes)
            .map_err(|e| e.to_string())?;
        let truncated = bytes.len() > 64 * 1024;
        bytes.truncate(64 * 1024);
        Some(if bytes.contains(&0) {
            "Binary file — preview unavailable".into()
        } else {
            format!(
                "{}{}",
                String::from_utf8_lossy(&bytes),
                if truncated {
                    "\n[Preview limited to 64 KiB]"
                } else {
                    ""
                }
            )
        })
    };
    let git = |args: &[&str]| {
        let mut command = std::process::Command::new("git");
        command
            .arg("-C")
            .arg(&root)
            .args(args)
            .env("GIT_OPTIONAL_LOCKS", "0")
            .stdin(std::process::Stdio::null());
        #[cfg(windows)]
        {
            use std::os::windows::process::CommandExt;
            command.creation_flags(windows_sys::Win32::System::Threading::CREATE_NO_WINDOW);
        }
        command
            .output()
            .map(|o| {
                if o.status.success() {
                    String::from_utf8_lossy(&o.stdout).trim_end().to_owned()
                } else {
                    String::from_utf8_lossy(&o.stderr).trim_end().to_owned()
                }
            })
            .unwrap_or_else(|e| format!("Git unavailable: {e}"))
    };
    Ok(ProjectData {
        preview,
        branch: git(&["branch", "--show-current"]),
        changes: git(&["status", "--short"]),
        commits: git(&["log", "-8", "--format=%h  %s"]),
    })
}

impl Desktop {
    pub(super) fn set_compact(&mut self, value: bool) {
        self.design.compact = value;
    }
    pub(super) fn show_terminals(&mut self) {
        self.design.mode = View::Terminals;
    }
    pub(super) fn clear_project_inventory(&mut self) {
        self.design.projects.clear();
        self.design.projects_polled = None;
        self.design.project_menu = None;
    }

    /// Gateway projects not open on this machine: those checked out on other
    /// machines first, then the rest, each alphabetically.
    pub(super) fn sidebar_projects(&self) -> Vec<&amux::gateway::ProjectOverview> {
        let mut projects: Vec<_> = self
            .design
            .projects
            .iter()
            .filter(|p| !self.workspaces.iter().any(|w| w.gateway.project == p.ident))
            .collect();
        projects.sort_by(|a, b| {
            (a.machines.is_empty(), &a.ident).cmp(&(b.machines.is_empty(), &b.ident))
        });
        projects
    }
    /// Hover details for a project; other machines' folders are not shown.
    pub(super) fn project_lines(
        &self,
        ident: &str,
        directory: Option<&std::path::Path>,
    ) -> Vec<String> {
        let mut lines = vec![format!("Project: {ident}")];
        if let Some(directory) = directory {
            lines.push(format!("Checked out at: {}", directory.display()));
        }
        let Some(project) = self.design.projects.iter().find(|p| p.ident == ident) else {
            return lines;
        };
        if let Some(remote) = &project.canonical_remote {
            lines.push(format!("Repository: {remote}"));
        }
        lines.push(if project.machines.is_empty() {
            "Machines: not checked out anywhere".into()
        } else {
            let hosts: Vec<_> = project
                .machines
                .iter()
                .map(|m| m.hostname.as_str())
                .collect();
            format!("Machines: {}", hosts.join(", "))
        });
        lines.push(match project.agents {
            0 => "Agents: none".into(),
            n => format!("Agents: {n} ({} active)", project.active),
        });
        lines
    }
    pub(super) fn close_design_popovers(&mut self) {
        self.design.tab_menu = None;
        self.design.project_menu = None;
        self.design.bell = false;
        self.design.events = false;
    }
    pub(super) fn dismiss_tab_menu(&mut self) {
        self.design.tab_menu = None;
    }
    pub(super) fn design_viewport(&mut self, size: iced::Size) {
        self.design.viewport = Some(size);
    }
}

/// Draw the unchanged underlay while excluding it from input and focus traversal.
fn inert<'a>(content: Element<'a, Message>) -> Element<'a, Message> {
    use iced::advanced::{
        Layout, Widget, layout, renderer,
        widget::{Tree, tree},
    };
    struct Inert<'a>(Element<'a, Message>);
    impl Widget<Message, Theme, iced::Renderer> for Inert<'_> {
        fn tag(&self) -> tree::Tag {
            self.0.as_widget().tag()
        }
        fn state(&self) -> tree::State {
            self.0.as_widget().state()
        }
        fn children(&self) -> Vec<Tree> {
            self.0.as_widget().children()
        }
        fn diff(&self, tree: &mut Tree) {
            self.0.as_widget().diff(tree);
        }
        fn size(&self) -> iced::Size<Length> {
            self.0.as_widget().size()
        }
        fn layout(
            &mut self,
            tree: &mut Tree,
            renderer: &iced::Renderer,
            limits: &layout::Limits,
        ) -> layout::Node {
            self.0.as_widget_mut().layout(tree, renderer, limits)
        }
        fn draw(
            &self,
            tree: &Tree,
            renderer: &mut iced::Renderer,
            theme: &Theme,
            style: &renderer::Style,
            layout: Layout<'_>,
            cursor: iced::mouse::Cursor,
            viewport: &iced::Rectangle,
        ) {
            self.0
                .as_widget()
                .draw(tree, renderer, theme, style, layout, cursor, viewport);
        }
    }
    iced::Element::new(Inert(content))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn task_drawer_starts_at_a_third_and_resizes_within_the_window() {
        let mut app = super::super::tests::desktop();
        app.design_viewport(iced::Size::new(1500.0, 900.0));
        assert_eq!(app.design.task_drawer_width(), 500.0);
        drop(app.design_message(Action::ResizeTaskDrawer));
        drop(app.design_message(Action::Cursor(iced::Point::new(588.0, 200.0))));
        assert!((app.design.task_drawer_width() - 900.0).abs() < 0.01);
        drop(app.design_message(Action::FinishTaskDrawerResize));
        drop(app.design_message(Action::Cursor(iced::Point::ORIGIN)));
        assert!((app.design.task_drawer_width() - 900.0).abs() < 0.01);
        app.design_viewport(iced::Size::new(300.0, 600.0));
        assert_eq!(app.design.task_drawer_width(), 276.0);
    }
    #[test]
    fn task_search_finds_short_ids_titles_and_tags_across_states_without_changing_the_queue() {
        let mut app = super::super::tests::desktop();
        let tasks = vec![
            serde_json::json!({"id":"01a16c87-1234-4567-890a-123456789abc",
                "title":"Windows integration check", "status":"user_review", "labels":["os:windows"]}),
            serde_json::json!({"id":Uuid::new_v4(),"title":"Linux terminal", "status":"todo"}),
            serde_json::json!({"id":Uuid::new_v4(),"title":"Windows check complete", "status":"done", "labels":["bug"]}),
        ];
        app.workspaces[0].gateway.tasks = tasks.clone();
        let found = |query: &str| {
            tasks
                .iter()
                .filter(|task| matches_task_filter(task, Filter::Active, query))
                .map(|task| task["title"].as_str().unwrap())
                .collect::<Vec<_>>()
        };
        assert_eq!(found(" #01A16C87 "), ["Windows integration check"]);
        assert_eq!(
            found("01a16c87-1234-4567-890a-123456789abc"),
            ["Windows integration check"]
        );
        assert_eq!(
            found("check WINDOWS"),
            ["Windows integration check", "Windows check complete"]
        );
        assert_eq!(found("os:WINDOWS"), ["Windows integration check"]);
        assert_eq!(found("bug"), ["Windows check complete"]);
        assert!(found("nonexistent").is_empty());
        assert_eq!(found("   "), ["Linux terminal"]);
        drop(app.design_message(Action::Search("event search".into())));
        drop(app.design_message(Action::TaskSearch("01a16c87".into())));
        assert_eq!(app.design.search, "event search");
        assert_eq!(app.design.filter, Filter::Active);
        assert_eq!(app.workspaces[0].gateway.tasks, tasks);
        drop(app.design_tasks(false));
        drop(app.design_tasks(true));
        drop(app.design_message(Action::TaskSearch(String::new())));
        assert_eq!(app.design.filter, Filter::Active);
        assert_eq!(app.workspaces[0].gateway.tasks, tasks);
    }

    #[test]
    fn active_is_default_and_detached_tasks_redock_without_closing_sessions() {
        let mut app = super::super::tests::desktop();
        assert_eq!(app.design.filter, Filter::Active);
        assert!(matches_filter(Filter::UserReview, "user_review"));
        assert!(!matches_filter(Filter::Active, "user_review"));
        assert!(!matches_filter(Filter::Completed, "user_review"));
        assert!(
            matches_filter(Filter::Active, "todo") && matches_filter(Filter::Active, "in_progress")
        );
        let before = app
            .workspaces
            .iter()
            .map(|w| tabs(w).count())
            .collect::<Vec<_>>();
        app.workspaces[0].gateway.draft_title = "Keep my draft".into();
        drop(app.design_message(Action::ToggleTasks));
        assert!(app.design.tasks_collapsed);
        drop(app.design_message(Action::DetachTasks));
        let window = app.design.task_window.unwrap();
        assert_ne!(window, app.main_window);
        drop(app.close_window(window));
        assert!(app.design.task_window.is_none());
        assert!(!app.design.tasks_collapsed);
        assert_eq!(
            before,
            app.workspaces
                .iter()
                .map(|w| tabs(w).count())
                .collect::<Vec<_>>()
        );
        assert_eq!(app.workspaces[0].gateway.draft_title, "Keep my draft");
        assert!(!app.restarting);
    }
    #[test]
    fn project_views_use_real_files_and_git_and_bound_preview() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("hello.txt"), "hello").unwrap();
        assert!(
            read_project(dir.path(), dir.path())
                .unwrap()
                .preview
                .is_none()
        );
        assert_eq!(
            read_project(dir.path(), &dir.path().join("hello.txt"))
                .unwrap()
                .preview
                .as_deref(),
            Some("hello")
        );
        let nested = dir.path().join("nested");
        std::fs::create_dir(&nested).unwrap();
        assert!(read_project(&nested, &dir.path().join("hello.txt")).is_err());
        std::fs::write(dir.path().join("large"), vec![b'a'; 70_000]).unwrap();
        assert!(
            read_project(dir.path(), &dir.path().join("large"))
                .unwrap()
                .preview
                .unwrap()
                .ends_with("[Preview limited to 64 KiB]")
        );
        assert!(matches_filter(Filter::Completed, "done"));
        assert!(!matches_filter(Filter::Active, "done"));
    }
    #[test]
    fn tab_context_closes_its_exact_target_without_selecting_it() {
        let mut app = super::super::tests::desktop();
        let workspace = &mut app.workspaces[0];
        let original = workspace.focused;
        let (other, _) = workspace
            .panes
            .split(
                pane_grid::Axis::Vertical,
                original,
                Pane {
                    tabs: Vec::new(),
                    active: 0,
                },
            )
            .unwrap();
        let tab = workspace
            .panes
            .get_mut(original)
            .unwrap()
            .tabs
            .pop()
            .unwrap();
        let tab_id = tab.id;
        workspace.panes.get_mut(other).unwrap().tabs.push(tab);
        drop(app.design_message(Action::TabMenu(other, tab_id)));
        assert_eq!(app.workspaces[0].focused, original);
        assert!(app.design.tab_menu.is_some());
        drop(app.design_message(Action::CloseMenuTab));
        assert!(app.workspaces[0].panes.get(other).is_none());
        assert_eq!(app.workspaces[0].focused, original);
        assert!(app.design.tab_menu.is_none());
    }

    #[test]
    fn restart_session_relaunches_only_agent_tabs_in_place_on_the_same_terminal_number() {
        let mut app = super::super::tests::desktop();
        let pane = app.workspaces[0].focused;
        let tab = app.workspaces[0].panes.get(pane).unwrap().tabs[0].id;
        app.workspaces[0].panes.get_mut(pane).unwrap().tabs[0].provider = None;
        drop(app.update(Message::RestartTab(pane, tab)));
        assert!(
            app.status.contains("Only Codex or Claude"),
            "{}",
            app.status
        );
        let first = &mut app.workspaces[0].panes.get_mut(pane).unwrap().tabs[0];
        first.provider = Some(Provider::Claude);
        first.delegation.terminal_number = 42;
        drop(app.design_message(Action::TabMenu(pane, tab)));
        drop(app.design_message(Action::RestartMenuTab));
        assert!(app.design.tab_menu.is_none());
        let p = app.workspaces[0].panes.get(pane).unwrap();
        assert_eq!(p.tabs.len(), 1);
        assert_ne!(p.tabs[0].id, tab, "a restarted PTY gets a fresh runtime ID");
        assert_eq!(p.tabs[0].provider, Some(Provider::Claude));
        assert_eq!(p.tabs[0].delegation.terminal_number, 42);
    }

    #[test]
    fn in_progress_tasks_lead_the_list_and_keep_gateway_order_otherwise() {
        let tasks = ["todo", "in_progress", "user_review", "in_progress", "todo"]
            .iter()
            .enumerate()
            .map(|(i, status)| serde_json::json!({"id": i, "status": status}))
            .collect::<Vec<_>>();
        let order = kanban_order(&tasks)
            .iter()
            .map(|task| task["id"].as_u64().unwrap())
            .collect::<Vec<_>>();
        assert_eq!(order, [1, 3, 0, 2, 4]);
    }

    #[test]
    fn tab_context_is_dismissed_by_activation_escape_and_workspace_switch() {
        let mut app = super::super::tests::desktop();
        let pane = app.workspaces[0].focused;
        let tab = app.workspaces[0].panes.get(pane).unwrap().tabs[0].id;
        for message in [
            Message::SelectTab(pane, 0),
            Message::Escape,
            Message::SelectWorkspace(1),
        ] {
            app.active = 0;
            drop(app.design_message(Action::TabMenu(pane, tab)));
            assert!(app.design.tab_menu.is_some());
            drop(app.update(message));
            assert!(app.design.tab_menu.is_none());
        }
        // A stale action from another workspace cannot close its matching pane id.
        app.active = 0;
        drop(app.design_message(Action::TabMenu(pane, tab)));
        app.active = 1;
        drop(app.design_message(Action::CloseMenuTab));
        assert_eq!(app.workspaces[0].panes.get(pane).unwrap().tabs.len(), 1);
        assert_eq!(app.workspaces[1].panes.len(), 1);
    }

    #[test]
    fn maximizing_preserves_every_pane_and_clear_only_hides_events() {
        let mut app = super::super::tests::desktop();
        let w = &mut app.workspaces[0];
        let before: Vec<_> = tabs(w).map(|t| t.id).collect();
        w.panes
            .split(
                pane_grid::Axis::Vertical,
                w.focused,
                Pane {
                    tabs: Vec::new(),
                    active: 0,
                },
            )
            .unwrap();
        drop(app.design_message(Action::Single));
        assert!(app.workspaces[0].panes.maximized().is_some());
        drop(app.design_message(Action::Single));
        assert!(app.workspaces[0].panes.maximized().is_none());
        assert_eq!(app.workspaces[0].panes.len(), 2);
        assert_eq!(
            tabs(&app.workspaces[0]).map(|t| t.id).collect::<Vec<_>>(),
            before
        );
        app.status = "Saved receipt".into();
        let events = app.design_events();
        drop(app.design_message(Action::ClearEvents));
        assert_eq!(app.design_events().len(), events.len());
        assert!(app.design.hidden_events.contains(&events[0].key));
    }
    #[test]
    fn general_settings_save_without_gateway_and_keep_uncertain_submission() {
        let dir = tempfile::tempdir().unwrap();
        let mut app = super::super::tests::desktop();
        app.preferences.current.mode = GatewayMode::Client;
        app.preferences.draft = app.preferences.current.clone();
        app.preferences.path = Some(dir.path().join("settings.json"));
        app.preferences.open = true;
        app.preferences.draft.auto_resume = false;
        app.workspaces[0].gateway.request = Some(TaskRequest {
            id: Uuid::new_v4(),
            project: "project-0".into(),
            title: "Uncertain".into(),
            state: TaskRequestState::Unconfirmed("No receipt".into()),
        });
        drop(app.update(Message::SavePreferences));
        assert!(!app.preferences.open);
        assert!(app.workspaces[0].gateway.request.is_some());
        assert!(
            !GatewayPreferences::load(app.preferences.path.as_deref().unwrap())
                .unwrap()
                .unwrap()
                .auto_resume
        );
    }
    #[test]
    fn stale_project_preview_cannot_replace_the_latest_navigation() {
        let mut app = super::super::tests::desktop();
        let id = app.workspaces[0].id;
        app.design.revision = 2;
        app.design.loading = true;
        drop(app.design_message(Action::Loaded(
            id,
            1,
            PathBuf::from("old"),
            Err("old".into()),
        )));
        assert!(app.design.project.is_none());
        assert!(app.design.loading);
    }
}
