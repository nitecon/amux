//! Device-local project favorites, display ordering and exact-first filtering.
use super::*;
use iced::widget::column;

#[derive(Clone, Copy)]
enum ProjectRow<'a> {
    Workspace(usize),
    Checkout(&'a desktop_workspace::LocalProject),
    Remote(&'a amux::gateway::ProjectOverview),
}

struct ProjectEntry<'a> {
    row: ProjectRow<'a>,
    name: String,
    names: Vec<String>,
    favorite: bool,
}

impl Desktop {
    fn gateway_favorite_key(&self, project: &str) -> String {
        let cluster = self
            .preferences
            .current
            .enrolled
            .as_ref()
            .map(|connection| connection.cluster_id.as_str())
            .unwrap_or_default();
        format!("gateway:{cluster}:{project}")
    }

    fn project_keys(&self, project: ProjectRow<'_>) -> Vec<String> {
        let mut keys = Vec::new();
        let ident = match project {
            ProjectRow::Workspace(index) => {
                let workspace = &self.workspaces[index];
                keys.push(format!("local:{}", workspace.directory.display()));
                Some(workspace.gateway.project.as_str())
            }
            ProjectRow::Checkout(checkout) => {
                keys.push(format!("local:{}", checkout.directory.display()));
                checkout
                    .canonical_remote
                    .as_ref()
                    .and_then(|remote| {
                        self.design
                            .projects
                            .iter()
                            .find(|p| p.canonical_remote.as_ref() == Some(remote))
                    })
                    .map(|project| project.ident.as_str())
            }
            ProjectRow::Remote(project) => Some(project.ident.as_str()),
        };
        if let Some(ident) = ident.filter(|ident| !ident.is_empty()) {
            keys.push(self.gateway_favorite_key(ident));
        }
        keys
    }

    pub(super) fn toggle_project_favorite(&mut self, keys: Vec<String>) {
        let pinned = keys
            .iter()
            .any(|key| self.design.project_favorites.contains(key));
        for key in &keys {
            self.design.project_favorites.remove(key);
        }
        if !pinned && let Some(key) = keys.last() {
            self.design.project_favorites.insert(key.clone());
        }
        self.dirty = true;
        self.save();
    }

    pub(in super::super) fn reconcile_project_favorites(&mut self) {
        // A workspace pinned before project resolution follows that same project
        // when it later appears as a remote row, without keeping duplicate aliases.
        for index in 0..self.workspaces.len() {
            let keys = self.project_keys(ProjectRow::Workspace(index));
            if keys.len() == 2 && self.design.project_favorites.remove(&keys[0]) {
                self.design.project_favorites.insert(keys[1].clone());
                self.dirty = true;
            }
        }
    }

    fn project_favorite_button(&self, project: ProjectRow<'_>) -> Element<'static, Message> {
        let keys = self.project_keys(project);
        let pinned = keys
            .iter()
            .any(|key| self.design.project_favorites.contains(key));
        tooltip(
            button(
                text(if pinned { "★" } else { "☆" })
                    .size(16)
                    .color(if pinned { CYAN } else { MUTED }),
            )
            .on_press(action(Action::ProjectFavorite(keys)))
            .style(button::text)
            .padding(3),
            text(if pinned {
                "Unpin project"
            } else {
                "Pin project"
            }),
            tooltip::Position::Bottom,
        )
        .into()
    }

    fn project_entries(&self) -> Vec<ProjectEntry<'_>> {
        let rows = (0..self.workspaces.len())
            .map(ProjectRow::Workspace)
            .chain(
                self.unadded_local_projects()
                    .into_iter()
                    .map(ProjectRow::Checkout),
            )
            .chain(self.sidebar_projects().into_iter().map(ProjectRow::Remote));
        let mut entries: Vec<_> = rows
            .map(|row| {
                let (name, ident) = match row {
                    ProjectRow::Workspace(index) => (
                        workspace_name(&self.workspaces[index]),
                        Some(self.workspaces[index].gateway.project.as_str()),
                    ),
                    ProjectRow::Checkout(checkout) => (
                        checkout
                            .directory
                            .file_name()
                            .unwrap_or(checkout.directory.as_os_str())
                            .to_string_lossy()
                            .into_owned(),
                        checkout
                            .canonical_remote
                            .as_ref()
                            .and_then(|remote| {
                                self.design.projects.iter().find(|project| {
                                    project.canonical_remote.as_ref() == Some(remote)
                                })
                            })
                            .map(|project| project.ident.as_str()),
                    ),
                    ProjectRow::Remote(project) => (project.ident.clone(), None),
                };
                let mut names = vec![name.to_lowercase()];
                if let Some(ident) = ident.filter(|ident| !ident.is_empty()) {
                    names.push(ident.to_lowercase());
                }
                ProjectEntry {
                    row,
                    name,
                    names,
                    favorite: self
                        .project_keys(row)
                        .iter()
                        .any(|key| self.design.project_favorites.contains(key)),
                }
            })
            .collect();
        let query = self.design.project_search.trim().to_lowercase();
        if !query.is_empty() {
            let exact = entries.iter().any(|entry| entry.names.contains(&query));
            entries.retain(|entry| {
                entry.names.iter().any(|name| {
                    if exact {
                        name == &query
                    } else {
                        name.contains(&query)
                    }
                })
            });
        }
        entries.sort_by_cached_key(|entry| {
            let kind = match entry.row {
                ProjectRow::Workspace(_) => 0,
                ProjectRow::Checkout(_) => 1,
                ProjectRow::Remote(project) => {
                    if project.agents > 0 {
                        2
                    } else {
                        3
                    }
                }
            };
            (
                !entry.favorite,
                kind,
                entry.name.to_lowercase(),
                entry.name.clone(),
            )
        });
        entries
    }

    pub(super) fn project_sidebar(&self) -> Element<'_, Message> {
        let mut sidebar = column![].spacing(8);
        let entries = self.project_entries();
        let mut previous = None;
        for entry in &entries {
            let section = if entry.favorite {
                "FAVORITES"
            } else {
                match entry.row {
                    ProjectRow::Remote(_) => "REMOTE PROJECTS",
                    _ => "LOCAL PROJECTS",
                }
            };
            if previous != Some(section) {
                sidebar = sidebar.push(text(section).size(11).color(MUTED));
                previous = Some(section);
            }
            sidebar = sidebar.push(match entry.row {
                ProjectRow::Workspace(index) => self.workspace_project_card(index),
                ProjectRow::Checkout(project) => self.checkout_project_card(project),
                ProjectRow::Remote(project) => self.remote_project_card(project),
            });
        }
        if entries.is_empty() {
            sidebar = sidebar.push(text("No projects match.").size(12).color(MUTED));
        }
        container(
            column![
                row![
                    text("PROJECTS").size(11).color(MUTED),
                    Space::new().width(Length::Fill),
                    control("plus", "New", Message::NewWorkspace, true)
                ]
                .align_y(Alignment::Center),
                text_input("Filter projects…", &self.design.project_search)
                    .on_input(|value| action(Action::ProjectSearch(value)))
                    .size(13),
                scrollable(sidebar).spacing(8).height(Length::Fill),
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
        .style(|_| surface(PANEL, false))
        .into()
    }
    fn workspace_project_card(&self, index: usize) -> Element<'_, Message> {
        let mut sidebar = column![].spacing(8);
        let w = &self.workspaces[index];
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
        let selected = self.remote_active.is_none() && index == self.active;
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
                diagnostic = diagnostic.push(text(format!("Claimed: {task}")).size(11).color(CYAN));
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
                self.project_favorite_button(ProjectRow::Workspace(index)),
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
                mouse_area(card)
                    .on_right_press(action(Action::LocalProjectMenu(w.directory.clone()))),
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
                        text("Close this workspace and stop its terminals? Files stay on disk.")
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

        sidebar.into()
    }
    fn checkout_project_card<'a>(
        &'a self,
        project: &'a desktop_workspace::LocalProject,
    ) -> Element<'a, Message> {
        let mut sidebar = column![].spacing(8);
        let name = project
            .directory
            .file_name()
            .unwrap_or(project.directory.as_os_str());
        let task_project = project.canonical_remote.as_ref().and_then(|remote| {
            self.design
                .projects
                .iter()
                .find(|item| item.canonical_remote.as_ref() == Some(remote))
        });
        let title: Element<'_, Message> = if let Some(task_project) = task_project {
            button(
                text(name.to_string_lossy().into_owned())
                    .size(13)
                    .color(MUTED),
            )
            .on_press(action(Action::RemoteProject(task_project.ident.clone())))
            .padding(0)
            .style(button::text)
            .into()
        } else {
            text(name.to_string_lossy().into_owned())
                .size(13)
                .color(MUTED)
                .into()
        };
        sidebar = sidebar.push(
            tooltip(
                mouse_area(
                    container(
                        row![
                            text("●").size(12).color(MUTED),
                            column![title, text("Not added").size(10).color(MUTED)]
                                .spacing(2)
                                .width(Length::Fill),
                            self.project_favorite_button(ProjectRow::Checkout(project)),
                            button(text("Add").size(11).color(CYAN))
                                .on_press(action(Action::AddLocalProject(
                                    project.directory.clone()
                                )))
                                .padding([5, 8])
                                .style(button::text)
                        ]
                        .spacing(8)
                        .align_y(Alignment::Center),
                    )
                    .padding([10, 6])
                    .width(Length::Fill)
                    .style(|_| surface(CARD, true)),
                )
                .on_right_press(action(Action::LocalProjectMenu(project.directory.clone()))),
                text(format!(
                    "{}\nAdd this existing checkout to amux",
                    project.directory.display()
                )),
                tooltip::Position::Right,
            )
            .delay(Duration::from_millis(250)),
        );

        sidebar.into()
    }
    fn remote_project_card<'a>(
        &'a self,
        project: &'a amux::gateway::ProjectOverview,
    ) -> Element<'a, Message> {
        let mut sidebar = column![].spacing(8);
        let selected = self.remote_active.is_some()
            && self.task_panel(self.task_workspace()).project == project.ident;
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
            .style(move |_| {
                surface(
                    if selected {
                        Color::from_rgb8(13, 39, 51)
                    } else {
                        CARD
                    },
                    true,
                )
            }),
        )
        .on_press(action(Action::RemoteProject(project.ident.clone())))
        .on_right_press(action(Action::ProjectMenu(project.ident.clone())));
        let row = row![
            row,
            self.project_favorite_button(ProjectRow::Remote(project))
        ]
        .align_y(Alignment::Center);
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

        sidebar.into()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::Path;

    fn desktop() -> Desktop {
        super::super::super::tests::desktop()
    }
    fn project(name: &str, agents: u64) -> amux::gateway::ProjectOverview {
        amux::gateway::ProjectOverview {
            ident: name.into(),
            agents,
            ..Default::default()
        }
    }
    fn names(app: &Desktop) -> Vec<String> {
        app.project_entries()
            .into_iter()
            .map(|entry| entry.name)
            .collect()
    }

    #[test]
    fn right_click_opens_checkout_menu_for_added_and_discovered_local_projects() {
        use iced::advanced::{Shell, layout, renderer::Headless, widget::Tree};
        let renderer = iced::futures::executor::block_on(iced::Renderer::new(
            iced::Font::DEFAULT,
            iced::Pixels(14.0),
            Some("tiny-skia"),
        ))
        .unwrap();
        let app = desktop();
        let project = desktop_workspace::LocalProject {
            directory: PathBuf::from("discovered checkout"),
            canonical_remote: None,
        };
        for (mut view, expected) in [
            (app.workspace_project_card(0), &app.workspaces[0].directory),
            (app.checkout_project_card(&project), &project.directory),
        ] {
            let mut tree = Tree::new(view.as_widget());
            let node = view.as_widget_mut().layout(
                &mut tree,
                &renderer,
                &layout::Limits::new(iced::Size::ZERO, iced::Size::new(240.0, 400.0)),
            );
            let mut messages = Vec::new();
            view.as_widget_mut().update(
                &mut tree,
                &iced::Event::Mouse(iced::mouse::Event::ButtonPressed(
                    iced::mouse::Button::Right,
                )),
                iced::advanced::Layout::new(&node),
                iced::mouse::Cursor::Available(iced::Point::new(20.0, 20.0)),
                &renderer,
                &mut iced::advanced::clipboard::Null,
                &mut Shell::new(&mut messages),
                &iced::Rectangle::with_size(node.size()),
            );
            assert!(
                matches!(messages.as_slice(), [Message::Design(Action::LocalProjectMenu(directory))] if directory == expected)
            );
        }
    }

    #[test]
    fn favorites_lead_local_alpha_and_remote_agent_priority_without_reordering_sessions() {
        let mut app = desktop();
        app.workspaces[0].name = Some("zLocal".into());
        app.workspaces[1].name = Some("aLocal".into());
        let identities: Vec<_> = app.workspaces.iter().map(|w| w.id).collect();
        app.preferences.current.projects_dir = "/fixture/checkouts".into();
        app.design.local_projects_polled =
            Some((PathBuf::from("/fixture/checkouts"), Instant::now()));
        app.design.local_projects = vec![desktop_workspace::LocalProject {
            directory: PathBuf::from("/fixture/checkouts/middle"),
            canonical_remote: None,
        }];
        app.design.projects = vec![
            project("X", 0),
            project("Zeta", 1),
            project("Alpha", 0),
            project("beta", 2),
        ];
        assert_eq!(
            names(&app),
            ["aLocal", "zLocal", "middle", "beta", "Zeta", "Alpha", "X"]
        );
        let keys = app.project_keys(ProjectRow::Remote(&app.design.projects[0]));
        drop(app.design_message(Action::ProjectFavorite(keys)));
        assert_eq!(
            names(&app),
            ["X", "aLocal", "zLocal", "middle", "beta", "Zeta", "Alpha"]
        );
        assert_eq!(
            app.workspaces.iter().map(|w| w.id).collect::<Vec<_>>(),
            identities
        );
        assert_eq!(app.active, 0);
        assert!(
            app.remote_active.is_none(),
            "pinning does not select or clone a project"
        );
        drop(app.design_message(Action::ProjectSearch("middle".into())));
        assert_eq!(
            names(&app),
            ["middle"],
            "unadded checkouts participate in the same filter"
        );
        drop(app.design_message(Action::ProjectSearch(String::new())));
        drop(app.design_view());
    }

    #[test]
    fn project_filter_uses_case_insensitive_exact_matches_before_substrings_in_every_group() {
        let mut app = desktop();
        app.workspaces[0].name = Some("X".into());
        app.design.task_search = "keep task search".into();
        app.workspaces[1].name = Some("local".into());
        app.design.projects = vec![
            project("starxapi", 1),
            project("starX", 0),
            project("Star", 0),
        ];
        for (query, expected) in [
            (" x ", vec!["X"]),
            ("STARX", vec!["starX"]),
            ("starxa", vec!["starxapi"]),
            ("star", vec!["Star"]),
            ("missing", vec![]),
        ] {
            drop(app.design_message(Action::ProjectSearch(query.into())));
            assert_eq!(names(&app), expected);
        }
        app.design.projects.pop();
        drop(app.design_message(Action::ProjectSearch("star".into())));
        assert_eq!(names(&app), ["starxapi", "starX"]);
        drop(app.design_message(Action::ProjectSearch(String::new())));
        assert_eq!(names(&app), ["local", "X", "starxapi", "starX"]);
        assert_eq!(app.active, 0);
        assert_eq!(app.design.task_search, "keep task search");
    }

    #[test]
    fn remote_favorite_survives_save_reload_and_local_opening_without_crossing_gateways() {
        let mut app = desktop();
        let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&root).unwrap();
        let fixture = tempfile::tempdir_in(root).unwrap();
        app.snapshot_path = Some(fixture.path().join("layout.json"));
        for (index, workspace) in app.workspaces.iter_mut().enumerate() {
            workspace.directory = fixture.path().join(format!("workspace-{index}"));
        }
        app.design.projects = vec![project("X", 0)];
        let keys = app.project_keys(ProjectRow::Remote(&app.design.projects[0]));
        drop(app.design_message(Action::ProjectFavorite(keys)));
        let saved = Snapshot::load(app.snapshot_path.as_ref().unwrap())
            .unwrap()
            .unwrap();
        let mut reopened = desktop();
        reopened.design.project_favorites = saved.project_favorites;
        reopened.design.projects = app.design.projects.clone();
        assert_eq!(names(&reopened)[0], "X");
        assert!(reopened.project_entries()[0].favorite);
        reopened.bind_project(0, "X".into());
        reopened.workspaces[0].name = Some("My game".into());
        assert_eq!(names(&reopened)[0], "My game");
        drop(reopened.design_message(Action::ProjectSearch("x".into())));
        assert_eq!(
            names(&reopened),
            ["My game"],
            "gateway project names remain searchable under a local alias"
        );
        let keys = reopened.project_keys(ProjectRow::Workspace(0));
        drop(reopened.design_message(Action::ProjectFavorite(keys)));
        assert!(reopened.design.project_favorites.is_empty());
        let keys = reopened.project_keys(ProjectRow::Remote(&reopened.design.projects[0]));
        reopened.toggle_project_favorite(keys);
        reopened.preferences.current.enrolled = Some(EnrolledConnection {
            cluster_id: "other-gateway".into(),
            node_id: "other".into(),
            url: "https://localhost:24182".into(),
            profile_path: PathBuf::from("/fixture/profile"),
        });
        assert!(
            reopened
                .project_entries()
                .iter()
                .all(|entry| !entry.favorite)
        );
    }

    #[test]
    fn a_local_favorite_acquires_the_resolved_project_identity_without_duplicate_aliases() {
        let mut app = desktop();
        app.workspaces[0].gateway.project.clear();
        let keys = app.project_keys(ProjectRow::Workspace(0));
        app.toggle_project_favorite(keys);
        assert_eq!(app.design.project_favorites.len(), 1);
        app.bind_project(0, "X".into());
        assert_eq!(app.design.project_favorites.len(), 1);
        assert!(
            app.design
                .project_favorites
                .contains(&app.gateway_favorite_key("X"))
        );
        let keys = app.project_keys(ProjectRow::Workspace(0));
        app.toggle_project_favorite(keys);
        assert!(app.design.project_favorites.is_empty());
    }
}
