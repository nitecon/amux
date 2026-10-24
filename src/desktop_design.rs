//! Native presentation of the owner's TSX concept. Data comes from the app, never screen text.
use super::*;
use iced::widget::{Space, column, mouse_area, opaque, stack};
use iced::{Alignment, Border, Color};

const BG: Color = Color::from_rgb8(11, 15, 23);
const PANEL: Color = Color::from_rgb8(14, 19, 29);
const CARD: Color = Color::from_rgb8(18, 26, 39);
const LINE: Color = Color::from_rgb8(30, 41, 59);
const MUTED: Color = Color::from_rgb8(148, 163, 184);
const CYAN: Color = Color::from_rgb8(34, 211, 238);
const GREEN: Color = Color::from_rgb8(16, 185, 129);
const AMBER: Color = Color::from_rgb8(251, 191, 36);

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
    All,
    Active,
    Completed,
}
#[derive(Debug, Clone)]
pub(super) enum Action {
    View(View),
    Filter(Filter),
    Events,
    Bell,
    Search(String),
    ClearEvents,
    Single,
    Inspect(PathBuf),
    Loaded(Uuid, u64, PathBuf, Result<ProjectData, String>),
}
#[derive(Default)]
pub(super) struct State {
    mode: View,
    compact: bool,
    filter: Filter,
    events: bool,
    bell: bool,
    search: String,
    hidden_events: std::collections::HashSet<String>,
    seen_events: std::collections::HashSet<String>,
    project: Option<(Uuid, PathBuf, Result<ProjectData, String>)>,
    loading: bool,
    revision: u64,
}
#[derive(Debug, Clone)]
pub(super) struct ProjectData {
    entries: Vec<(PathBuf, bool)>,
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
fn status_badge(status: &str) -> Element<'static, Message> {
    let (label, color) = match status {
        "done" => ("COMPLETED", GREEN),
        "todo" => ("QUEUED", AMBER),
        "in_progress" => ("IN PROGRESS", CYAN),
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
    tab.terminal
        .as_ref()
        .and_then(Terminal::activity_elapsed)
        .is_some_and(|elapsed| elapsed < Duration::from_secs(5))
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
            Action::View(mode) => {
                self.design.mode = mode;
                if mode != View::Terminals {
                    return self.load_project(self.workspaces[self.active].directory.clone());
                }
                return self.focus();
            }
            Action::Filter(filter) => self.design.filter = filter,
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
            Action::Inspect(path) => return self.load_project(path),
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
                        text(format!(
                            "{} · {agents} agents · {busy} active",
                            self.design_connection()
                        ))
                        .size(11)
                    )
                    .padding([5, 10])
                    .style(|_| surface(CARD, true)),
                    "Active means terminal input/output within the last 5 seconds.",
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
                text("WORKSPACES").size(11).color(MUTED),
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
                        text(format!("Last terminal I/O: {}s ago", elapsed.as_secs()))
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
            sidebar = sidebar.push(tooltip(
                card,
                container(diagnostic)
                    .padding(14)
                    .width(300)
                    .style(|_| surface(CARD, true)),
                tooltip::Position::Right,
            ));
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
        let sidebar = container(
            column![
                scrollable(sidebar).height(Length::Fill),
                control(
                    "settings",
                    "Settings & Gateway",
                    Message::Preferences(true),
                    false
                )
                .width(Length::Fill)
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
                column![toolbar, center]
                    .width(Length::Fill)
                    .height(Length::Fill),
                self.design_tasks()
            ]
            .height(Length::Fill)
        ]
        .into();
        // Modal focus operations and input must not reach widgets underneath.
        let base = if self.preferences.open || self.workspace_dialog.open {
            inert(base)
        } else {
            base
        };
        let mut layers = stack![base];
        if self.design.events {
            layers = layers.push(
                container(opaque(self.events_view()))
                    .align_right(Length::Fill)
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
                            control("bell", "View updates", Message::Updates(true), true)
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
                    container(
                        column![
                            row![
                                control(
                                    "terminal",
                                    label,
                                    Message::SelectTab(pane_id, index),
                                    selected
                                )
                                .style(move |theme, status| {
                                    if selected && tab_provider(tab) == Some(Provider::Claude) {
                                        claude_style(theme, status)
                                    } else {
                                        button_style(selected, theme, status)
                                    }
                                }),
                                button(icon("x"))
                                    .on_press(Message::CloseTab(pane_id, tab.id))
                                    .style(button::text)
                                    .padding(4)
                            ]
                            .align_y(Alignment::Center),
                            tooltip(
                                text(short_title(&subtitle)).size(10).color(MUTED),
                                container(
                                    text(format!("{subtitle}\n{}", w.directory.display())).size(11)
                                )
                                .padding(8)
                                .style(|_| surface(CARD, true)),
                                tooltip::Position::Bottom
                            ),
                        ]
                        .spacing(2),
                    )
                    .padding([3, 4])
                    .style(move |_| {
                        surface(
                            if selected {
                                Color::from_rgb8(9, 42, 56)
                            } else {
                                PANEL
                            },
                            false,
                        )
                    }),
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
                let status = if self.preferences.current.automatic_notifications {
                    tab.delegation.status()
                } else {
                    &tab.reminders.status
                };
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
                            "● Recent I/O"
                        } else {
                            "○ Idle"
                        })
                        .size(10)
                        .color(if active(tab) { CYAN } else { MUTED })
                    ]
                    .padding([3, 8]),
                );
                if !self.preferences.current.automatic_notifications
                    && cfg!(target_os = "linux")
                    && tab_provider(tab) == Some(Provider::Codex)
                    && tab.recipient.is_some()
                    && self.ledger.is_some()
                {
                    let id = tab.id;
                    title = title.push(
                        checkbox(tab.reminders.consent)
                            .label("Allow gateway reminders + Enter")
                            .on_toggle(move |v| Message::Consent(id, v))
                            .size(12)
                            .text_size(10),
                    );
                }
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
    fn design_tasks(&self) -> Element<'_, Message> {
        let panel = &self.workspaces[self.active].gateway;
        let filter = |label, kind| {
            control(
                "",
                label,
                action(Action::Filter(kind)),
                self.design.filter == kind,
            )
            .width(Length::Fill)
        };
        let header = column![
            row![
                icon("server"),
                text("GATEWAY TASKS").size(12),
                Space::new().width(Length::Fill),
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
            row![
                filter("All", Filter::All),
                filter("Active", Filter::Active),
                filter("Completed", Filter::Completed)
            ]
            .spacing(3),
        ]
        .spacing(10);
        let mut cards = column![text(&panel.status).size(11).color(MUTED)].spacing(8);
        let mut count = 0;
        for task in &panel.tasks {
            let status = task["status"].as_str().unwrap_or("unknown");
            if !matches_filter(self.design.filter, status) {
                continue;
            }
            count += 1;
            let id = task["id"].as_str().unwrap_or("?");
            let owner = self.design_task_owner(task);
            let mut item = column![
                row![
                    text(format!("#{}", id.chars().take(8).collect::<String>()))
                        .size(10)
                        .font(iced::Font::MONOSPACE)
                        .color(CYAN),
                    Space::new().width(Length::Fill),
                    status_badge(status)
                ],
                text(task["title"].as_str().unwrap_or("Untitled")).size(12),
                text(format!("→ {owner}")).size(10).color(MUTED),
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
            let w = &self.workspaces[self.active];
            if let Some(tab) = w
                .panes
                .get(w.focused)
                .and_then(|p| p.tabs.get(p.active))
                .filter(|t| t.reminders.consent && t.terminal.is_some())
                && let Ok(id) = Uuid::parse_str(id)
                && panel.pending_task(id)
            {
                item = item.push(control(
                    "send",
                    "Remind agent",
                    Message::Remind(self.scope(self.active), id, tab.id),
                    false,
                ));
            }
            cards = cards.push(
                container(item)
                    .padding(11)
                    .width(Length::Fill)
                    .style(|_| surface(CARD, true)),
            );
        }
        if count == 0 {
            cards = cards.push(
                text(if panel.loading {
                    "Loading gateway tasks…"
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
                    text("DISPATCH NEW GATEWAY TASK").size(10).color(CYAN)
                ]
                .spacing(6),
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
        .width(320)
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
            entries = entries.push(
                container(
                    column![
                        text(event.title).size(11).color(CYAN),
                        text(event.detail).font(iced::Font::MONOSPACE).size(11)
                    ]
                    .spacing(8),
                )
                .padding(12)
                .width(Length::Fill)
                .style(|_| surface(CARD, true)),
            );
        }
        container(
            column![
                row![
                    icon("activity"),
                    text("Gateway Events Telemetry").size(14),
                    Space::new().width(Length::Fill),
                    control("x", "", action(Action::Events), false)
                ]
                .spacing(8)
                .align_y(Alignment::Center),
                text("Delivery receipts and connection status")
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
        let tab = |label, kind| {
            control(
                "settings",
                label,
                Message::PreferencesTab(kind),
                self.preferences.tab == kind,
            )
        };
        let body: Element<'_,Message>=match self.preferences.tab {
            PreferencesTab::Gateway=>self.preferences_view(),
            PreferencesTab::Updates=>self.updates_view(),
            PreferencesTab::General=>column![
                text("Workspaces & Terminals").size(18),
                checkbox(self.preferences.draft.notify_exits).label("Notify on process exit").on_toggle(Message::ExitNotifications),
                text("Notifications report terminal process exits.").size(12).color(MUTED),
                checkbox(self.preferences.draft.auto_resume).label("Resume agent sessions on startup").on_toggle(Message::AutoResume),
                text("Saved running terminals reopen on startup. This option also reopens agent tabs with their native conversation resume.").size(12),
                text("Repository matching is required for gateway delivery.").size(12).color(CYAN)
            ].spacing(18).into(),
        };
        container(
            column![
                container(
                    row![
                        icon("settings"),
                        text("amux Settings & Gateway Preferences").size(16),
                        Space::new().width(Length::Fill),
                        control("x", "", Message::Preferences(false), false)
                    ]
                    .spacing(10)
                    .align_y(Alignment::Center)
                )
                .padding(18)
                .style(|_| surface(Color::from_rgb8(21, 30, 47), false)),
                container(
                    row![
                        tab("Gateway Server", PreferencesTab::Gateway),
                        tab("Workspaces & Terminals", PreferencesTab::General),
                        tab("Updates", PreferencesTab::Updates)
                    ]
                    .spacing(6)
                )
                .padding([0, 16]),
                container(scrollable(body).height(Length::Fill)).padding(20),
                container(
                    column![
                        text(&self.preferences.status).size(11).color(
                            if self.preferences.status.starts_with("Not saved") {
                                AMBER
                            } else {
                                MUTED
                            }
                        ),
                        row![
                            Space::new().width(Length::Fill),
                            control("x", "Cancel", Message::Preferences(false), false),
                            control("settings", "Save & Close", Message::SavePreferences, true)
                        ]
                        .spacing(8)
                    ]
                    .spacing(8)
                )
                .padding(16),
            ]
            .spacing(12),
        )
        .width(620)
        .height(650)
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
        if self.design.loading {
            content = content.push(text("Loading project…").size(12));
        }
        if let Some((id, path, result)) = &self.design.project
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
                    } else {
                        content = content
                            .push(
                                row![
                                    control(
                                        "folder",
                                        "Project root",
                                        action(Action::Inspect(root.directory.clone())),
                                        false
                                    ),
                                    control(
                                        "back",
                                        "Parent",
                                        action(Action::Inspect(
                                            path.parent()
                                                .filter(|p| p.starts_with(&root.directory))
                                                .unwrap_or(&root.directory)
                                                .to_path_buf()
                                        )),
                                        false
                                    ),
                                    control(
                                        "refresh",
                                        "Refresh",
                                        action(Action::Inspect(path.clone())),
                                        false
                                    )
                                ]
                                .spacing(6),
                            )
                            .push(text(path.display().to_string()).size(12).color(MUTED));
                        if let Some(preview) = &data.preview {
                            content =
                                content.push(text(preview).font(iced::Font::MONOSPACE).size(12));
                        }
                        for (path, directory) in &data.entries {
                            content = content.push(
                                control(
                                    if *directory { "folder" } else { "file" },
                                    path.file_name()
                                        .unwrap_or(path.as_os_str())
                                        .to_string_lossy()
                                        .into_owned(),
                                    action(Action::Inspect(path.clone())),
                                    false,
                                )
                                .width(Length::Fill),
                            );
                        }
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
fn matches_filter(filter: Filter, status: &str) -> bool {
    match filter {
        Filter::All => true,
        Filter::Active => matches!(status, "todo" | "in_progress"),
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
    let mut entries = Vec::new();
    let preview = if path.is_dir() {
        for entry in std::fs::read_dir(&path)
            .map_err(|e| e.to_string())?
            .take(1000)
            .flatten()
        {
            entries.push((entry.path(), entry.path().is_dir()));
        }
        entries.sort_by(|a, b| b.1.cmp(&a.1).then_with(|| a.0.cmp(&b.0)));
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
        entries,
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
    pub(super) fn close_design_popovers(&mut self) {
        self.design.bell = false;
        self.design.events = false;
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
    fn project_views_use_real_files_and_git_and_bound_preview() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("hello.txt"), "hello").unwrap();
        let data = read_project(dir.path(), dir.path()).unwrap();
        assert_eq!(data.entries.len(), 1);
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
