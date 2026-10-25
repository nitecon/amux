//! Workspace creation window and a portable folder/script browser.
use super::*;
use amux::workspace_launch;
use desktop_design::browser::{BrowserMessage, FileBrowser};
use iced::widget::column;

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub(super) enum Kind {
    #[default]
    General,
    Clone,
    Script,
}
#[derive(Debug, Clone)]
pub(super) enum WorkspaceMessage {
    Tab(Kind),
    Folder(String),
    Name(String),
    Script(String),
    Repository(String),
    Browse(bool),
    Browser(BrowserMessage),
    Pick(PathBuf),
    UseFolder,
    Back,
    Start,
    Cloned(Result<PathBuf, String>),
    Cancel,
}
#[derive(Default)]
pub(super) struct WorkspaceDialog {
    pub open: bool,
    pub last_directory: Option<PathBuf>,
    name: String,
    pub busy: bool,
    tab: Kind,
    folder: String,
    script: String,
    repository: String,
    status: String,
    /// The shared browser; it lists files only while choosing a script.
    browser: Option<FileBrowser>,
    /// The folder browser embedded in the General tab.
    folders: Option<FileBrowser>,
}

impl Desktop {
    // Indices change when a workspace closes. Route delayed responses by their
    // runtime workspace identity before applying the existing revision fences.
    pub(super) fn retarget_workspace_message(&self, message: Message) -> Option<Message> {
        let remap = |mut scope: QueryScope| {
            scope.workspace = self
                .workspaces
                .iter()
                .position(|w| w.id == scope.workspace_id)?;
            Some(scope)
        };
        Some(match message {
            Message::ProjectResolved(scope, result) => {
                Message::ProjectResolved(remap(scope)?, result)
            }
            Message::TasksLoaded(scope, result) => Message::TasksLoaded(remap(scope)?, result),
            Message::RoutingLoaded(scope, result) => Message::RoutingLoaded(remap(scope)?, result),
            Message::TaskDetailLoaded(scope, id, request, result) => {
                Message::TaskDetailLoaded(remap(scope)?, id, request, result)
            }
            Message::TaskChanged(scope, id, request, change, result) => {
                Message::TaskChanged(remap(scope)?, id, request, change, result)
            }
            Message::TaskCreated(scope, request, result) => {
                Message::TaskCreated(remap(scope)?, request, result)
            }
            Message::RecoveryLoaded(mut scope, result) => {
                scope.query = remap(scope.query)?;
                Message::RecoveryLoaded(scope, result)
            }
            Message::ReceiptReported(mut scope, task, result) => {
                scope.query = remap(scope.query)?;
                Message::ReceiptReported(scope, task, result)
            }
            message => message,
        })
    }

    pub(super) fn request_close_workspace(&mut self, id: Uuid) -> Task<Message> {
        let Some(workspace) = self.workspaces.iter().find(|w| w.id == id) else {
            return Task::none();
        };
        if workspace
            .panes
            .iter()
            .any(|(_, pane)| pane.tabs.iter().any(|tab| tab.terminal.is_some()))
        {
            self.closing_workspace = Some(id);
            Task::none()
        } else {
            self.remove_workspace(id)
        }
    }

    pub(super) fn remove_workspace(&mut self, id: Uuid) -> Task<Message> {
        let Some(index) = self.workspaces.iter().position(|w| w.id == id) else {
            return Task::none();
        };
        if self.workspaces[index].gateway.detail.unresolved()
            || self.workspaces[index]
                .gateway
                .request
                .as_ref()
                .is_some_and(|request| {
                    matches!(
                        request.state,
                        TaskRequestState::Sending | TaskRequestState::Unconfirmed(_)
                    )
                })
        {
            self.status = "This workspace has a pending or unconfirmed gateway task action. Inspect its receipt and refresh the task before closing.".into();
            return Task::none();
        }
        let fallback = if self.workspaces.len() == 1 {
            match workspace_launch::home_directory() {
                Ok(home) => Some(home),
                Err(error) => {
                    self.status = format!("Cannot open a fallback Default workspace: {error}");
                    return Task::none();
                }
            }
        } else {
            None
        };
        let terminals: Vec<_> = self.workspaces[index]
            .panes
            .iter()
            .flat_map(|(_, pane)| &pane.tabs)
            .map(|tab| tab.id)
            .collect();
        self.status = "Workspace closed. Project files were kept on disk.".into();
        for terminal in terminals {
            self.withdraw(terminal);
        }
        // Dropping tabs retires their owned jobs and PTYs, using the same path as
        // closing individual tabs. Project directories and files are untouched.
        self.workspaces.remove(index);
        if self.workspaces.is_empty() {
            self.add_workspace(fallback.expect("last workspace fallback validated"));
        } else if index < self.active {
            self.active -= 1;
        } else {
            self.active = self.active.min(self.workspaces.len() - 1);
        }
        self.closing_workspace = None;
        self.show_terminals();
        self.dirty = true;
        self.save();
        self.focus()
    }

    pub(super) fn open_workspace_dialog(&mut self) -> Task<Message> {
        if self.workspace_dialog.open {
            return Task::none();
        }
        self.close_design_popovers();
        self.preferences.open = false;
        let last_directory = self.workspace_dialog.last_directory.clone();
        let directory = last_directory
            .as_deref()
            .filter(|path| path.is_absolute())
            .and_then(|path| path.ancestors().find(|ancestor| ancestor.is_dir()))
            .map(PathBuf::from)
            .or_else(|| workspace_launch::home_directory().ok());
        self.workspace_dialog = WorkspaceDialog {
            open: true,
            last_directory,
            folder: directory
                .as_ref()
                .map(|p| p.display().to_string())
                .unwrap_or_default(),
            folders: directory.map(|d| FileBrowser::new(d, false, None)),
            ..Default::default()
        };
        iced::widget::operation::focus("workspace-name")
    }
    pub(super) fn close_workspace_dialog(&mut self) -> Task<Message> {
        if self.workspace_dialog.busy {
            return Task::none();
        }
        self.workspace_dialog.open = false;
        self.focus()
    }
    pub(super) fn workspace_message(&mut self, message: WorkspaceMessage) -> Task<Message> {
        use WorkspaceMessage::*;
        if !self.workspace_dialog.open {
            return Task::none();
        }
        if self.workspace_dialog.busy && !matches!(message, Cloned(_)) {
            return Task::none();
        }
        let dialog = &mut self.workspace_dialog;
        match message {
            Name(name) => dialog.name = name.chars().filter(|c| !c.is_control()).take(80).collect(),
            Tab(tab) => {
                dialog.tab = tab;
                dialog.status.clear();
                dialog.browser = None;
            }
            Folder(value) => dialog.folder = value,
            Script(value) => dialog.script = value,
            Repository(value) => dialog.repository = value,
            Browse(script) => {
                let path = PathBuf::from(if script {
                    &dialog.script
                } else {
                    &dialog.folder
                });
                let directory = if path.is_dir() {
                    Some(path)
                } else {
                    path.parent().filter(|p| p.is_dir()).map(PathBuf::from)
                };
                match directory.or_else(|| workspace_launch::home_directory().ok()) {
                    Some(directory) => {
                        dialog.browser = Some(FileBrowser::new(directory, script, None))
                    }
                    None => {
                        dialog.status =
                            "Cannot find your home directory; enter a folder path.".into()
                    }
                }
            }
            Browser(message) => {
                let embedded = dialog.browser.is_none() && dialog.tab == Kind::General;
                let browser = if embedded {
                    dialog.folders.as_mut()
                } else {
                    dialog.browser.as_mut()
                };
                if let Some(browser) = browser {
                    let naming = matches!(&message, BrowserMessage::NewFolder(Some(name)) if name.is_empty());
                    browser.update(message);
                    if embedded {
                        dialog.folder = browser.target().display().to_string();
                    }
                    if naming {
                        return iced::widget::operation::focus("browser-new-folder");
                    }
                    // Remember the last folder browsed for a workspace, not for scripts.
                    if !browser.files
                        && browser.error.is_empty()
                        && dialog.last_directory.as_ref() != Some(&browser.directory)
                    {
                        dialog.last_directory = Some(browser.directory.clone());
                        self.dirty = true;
                    }
                }
            }
            Pick(path) => {
                dialog.script = path.display().to_string();
                dialog.browser = None;
            }
            UseFolder => {
                if let Some(browser) = dialog.browser.take() {
                    dialog.folder = browser.target().display().to_string();
                    if !browser.files && browser.error.is_empty() {
                        dialog.last_directory = Some(browser.directory);
                        self.dirty = true;
                    }
                }
            }
            Back => dialog.browser = None,
            Cancel => return self.close_workspace_dialog(),
            Start => {
                let result = (|| -> anyhow::Result<(PathBuf, Option<PathBuf>)> {
                    let directory = if dialog.tab == Kind::Script {
                        workspace_launch::home_directory()?
                    } else {
                        workspace_launch::directory(&dialog.folder)?
                    };
                    let script = if dialog.tab == Kind::Clone || dialog.script.is_empty() {
                        anyhow::ensure!(
                            dialog.tab != Kind::Script,
                            "Choose an initialization script"
                        );
                        None
                    } else {
                        let path = workspace_launch::external_path(
                            PathBuf::from(&dialog.script).canonicalize()?,
                        );
                        workspace_launch::script_command(&path)?;
                        Some(path)
                    };
                    if dialog.tab == Kind::Clone {
                        workspace_launch::clone_name(&dialog.repository)?;
                    }
                    Ok((directory, script))
                })();
                match result {
                    Ok((directory, script)) => {
                        if dialog.tab == Kind::Clone {
                            dialog.busy = true;
                            dialog.status =
                                "Cloning repository… The workspace opens when cloning finishes."
                                    .into();
                            let repository = dialog.repository.clone();
                            return Task::perform(
                                async move {
                                    workspace_launch::clone_repository(directory, repository)
                                        .await
                                        .map_err(|e| format!("{e:#}"))
                                },
                                |result| Message::Workspace(Cloned(result)),
                            );
                        }
                        let name = dialog.name.trim().to_owned();
                        self.add_workspace_with_script(directory, script);
                        self.workspaces[self.active].name = (!name.is_empty()).then_some(name);
                        self.dirty = true;
                        return Task::batch([
                            self.close_workspace_dialog(),
                            self.query(self.active),
                        ]);
                    }
                    Err(error) => dialog.status = format!("{error:#}"),
                }
            }
            Cloned(result) => {
                dialog.busy = false;
                match result {
                    Ok(directory) => {
                        let name = dialog.name.trim().to_owned();
                        self.add_workspace(directory);
                        self.workspaces[self.active].name = (!name.is_empty()).then_some(name);
                        self.dirty = true;
                        return Task::batch([
                            self.close_workspace_dialog(),
                            self.query(self.active),
                        ]);
                    }
                    Err(error) => dialog.status = error,
                }
            }
        }
        Task::none()
    }
    pub(super) fn workspace_dialog_view(&self) -> Element<'_, Message> {
        use WorkspaceMessage::*;
        let dialog = &self.workspace_dialog;
        let msg = Message::Workspace;
        if let Some(browser) = &dialog.browser {
            let target = browser.target();
            // Scripts must be files; workspaces must be folders.
            let accept = if browser.files {
                target.is_file().then(|| msg(Pick(target.to_path_buf())))
            } else {
                browser.error.is_empty().then(|| msg(UseFolder))
            };
            let footer = browser_footer(browser, accept, msg(Back));
            return container(
                column![
                    text(if browser.files {
                        "Choose initialization script"
                    } else {
                        "Choose folder"
                    })
                    .size(22),
                    browser.view(|m| Message::Workspace(Browser(m)), footer),
                ]
                .spacing(12),
            )
            .padding(24)
            .width(Length::Fill)
            .height(Length::Fill)
            .into();
        }
        let tab = |label, kind| {
            button(label)
                .on_press_maybe((!dialog.busy).then_some(msg(Tab(kind))))
                .style(move |theme, status| {
                    desktop_design::button_style(dialog.tab == kind, theme, status)
                })
        };
        let mut body = column![
            row![
                text("New workspace").size(22),
                iced::widget::Space::new().width(Length::Fill),
                button("×").on_press_maybe((!dialog.busy).then_some(msg(Cancel)))
            ]
            .align_y(iced::Alignment::Center),
            text_input("Workspace name (optional)", &dialog.name)
                .id("workspace-name")
                .on_input(|v| Message::Workspace(Name(v)))
                .size(13),
            row![
                tab("General", Kind::General),
                tab("Clone", Kind::Clone),
                tab("Script", Kind::Script)
            ]
            .spacing(8)
        ]
        .spacing(16);
        if let (Kind::General, Some(folders)) = (dialog.tab, &dialog.folders) {
            let accept = (!dialog.busy && folders.error.is_empty()).then(|| msg(Start));
            body = body
                .push(
                    row![
                        text_input(
                            if cfg!(windows) {
                                "Initialization script (optional): PowerShell .ps1 or .cmd/.bat"
                            } else {
                                "Initialization script (optional): POSIX shell script"
                            },
                            &dialog.script
                        )
                        .on_input(|v| Message::Workspace(Script(v))),
                        button("Browse…").on_press(msg(Browse(true)))
                    ]
                    .spacing(8),
                )
                .push(
                    container(folders.view(
                        |m| Message::Workspace(Browser(m)),
                        browser_footer(folders, accept, msg(Cancel)),
                    ))
                    .height(Length::Fill),
                )
                .push(text(&dialog.status).size(13));
            return container(body)
                .padding(24)
                .width(Length::Fill)
                .height(Length::Fill)
                .into();
        }
        body = body.push(text(match dialog.tab {
            Kind::General => "Open an existing folder. New terminals will start there.",
            Kind::Clone => "Clone a Git repository into a new folder inside the selected parent.",
            Kind::Script => "Run an initialization script interactively from your home directory.",
        }));
        if dialog.tab != Kind::Script {
            body = body
                .push(text(if dialog.tab == Kind::Clone {
                    "Clone into parent directory"
                } else {
                    "Workspace directory"
                }))
                .push(
                    row![
                        text_input("Select a directory", &dialog.folder)
                            .on_input(|v| Message::Workspace(Folder(v))),
                        button("Browse…")
                            .on_press_maybe((!dialog.busy).then_some(msg(Browse(false))))
                    ]
                    .spacing(8),
                );
        } else {
            body = body.push(text(format!(
                "Starting directory: {}",
                workspace_launch::home_directory()
                    .map(|p| p.display().to_string())
                    .unwrap_or_default()
            )));
        }
        if dialog.tab == Kind::Clone {
            body = body.push(text("Repository URL")).push(
                text_input(
                    "https://host/owner/repository.git or git@host:owner/repository.git",
                    &dialog.repository,
                )
                .on_input(|v| Message::Workspace(Repository(v))),
            );
            body = body.push(text("Git must be installed and repository credentials already configured. Existing folders are never overwritten.").size(13));
        } else {
            body = body
                .push(text(if dialog.tab == Kind::General {
                    "Initialization script (optional)"
                } else {
                    "Initialization script"
                }))
                .push(
                    row![
                        text_input(
                            if cfg!(windows) {
                                "PowerShell .ps1 or command .cmd/.bat file"
                            } else {
                                "POSIX shell script"
                            },
                            &dialog.script
                        )
                        .on_input(|v| Message::Workspace(Script(v))),
                        button("Browse…").on_press(msg(Browse(true)))
                    ]
                    .spacing(8),
                );
            body = body.push(text("Runs when you explicitly start a terminal. Restoring a layout does not run scripts.").size(13));
        }
        body = body
            .push(
                row![
                    button(if dialog.busy { "Cloning…" } else { "Start" })
                        .on_press_maybe((!dialog.busy).then_some(msg(Start))),
                    button("Cancel").on_press_maybe((!dialog.busy).then_some(msg(Cancel)))
                ]
                .spacing(8),
            )
            .push(text(&dialog.status).size(13));
        container(scrollable(body))
            .padding(24)
            .width(Length::Fill)
            .height(Length::Fill)
            .into()
    }
}

/// New folder, the selected target, Cancel and a bottom-right Accept.
fn browser_footer<'a>(
    browser: &FileBrowser,
    accept: Option<Message>,
    cancel: Message,
) -> Element<'a, Message> {
    let target = browser.target();
    let name = target
        .file_name()
        .unwrap_or(target.as_os_str())
        .to_string_lossy()
        .into_owned();
    let mut footer = row![].spacing(10).align_y(iced::Alignment::Center);
    if !browser.files {
        footer = footer.push(
            button(text("New folder").size(12)).on_press(Message::Workspace(
                WorkspaceMessage::Browser(BrowserMessage::NewFolder(Some(String::new()))),
            )),
        );
    }
    footer
        .push(
            column![
                text(if browser.files {
                    "SELECTED SCRIPT"
                } else {
                    "SELECTED FOLDER TO OPEN"
                })
                .size(10),
                text(target.display().to_string())
                    .font(iced::Font::MONOSPACE)
                    .size(12),
            ]
            .spacing(2)
            .width(Length::Fill),
        )
        .push(button(text("Cancel").size(12)).on_press(cancel))
        .push(
            button(text(format!("Accept \"{name}\"")).size(12))
                .style(desktop_design::primary_action)
                .on_press_maybe(accept),
        )
        .into()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn browsed_folder_survives_cancel_and_saved_session_reload() {
        let root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&root).unwrap();
        let fixture = tempfile::tempdir_in(root).unwrap();
        let folder = fixture.path().join("projects with spaces");
        std::fs::create_dir(&folder).unwrap();
        let expected = workspace_launch::directory(&folder.to_string_lossy()).unwrap();
        let mut app = super::super::tests::desktop();
        let snapshot = fixture.path().join("layout.json");
        app.snapshot_path = Some(snapshot.clone());
        for workspace in &mut app.workspaces {
            workspace.directory = fixture.path().to_path_buf();
        }
        drop(app.open_workspace_dialog());
        drop(app.workspace_message(WorkspaceMessage::Browser(BrowserMessage::Open(folder))));
        drop(app.workspace_message(WorkspaceMessage::Cancel));
        assert!(app.dirty);
        assert!(app.save());
        let saved = Snapshot::load(&snapshot).unwrap().unwrap();
        assert_eq!(saved.workspace_browser_directory.as_ref(), Some(&expected));
        let mut reopened = super::super::tests::desktop();
        reopened.workspace_dialog.last_directory = saved.workspace_browser_directory;
        drop(reopened.open_workspace_dialog());
        assert_eq!(
            reopened
                .workspace_dialog
                .folders
                .as_ref()
                .unwrap()
                .directory,
            expected
        );
        assert_eq!(reopened.workspaces.len(), 2);
    }

    #[test]
    fn bad_navigation_and_script_browsing_keep_workspace_location_and_missing_folder_uses_parent() {
        let root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&root).unwrap();
        let fixture = tempfile::tempdir_in(root).unwrap();
        let parent = workspace_launch::directory(&fixture.path().to_string_lossy()).unwrap();
        let folder = parent.join("project");
        std::fs::create_dir(&folder).unwrap();
        let mut app = super::super::tests::desktop();
        drop(app.open_workspace_dialog());
        drop(
            app.workspace_message(WorkspaceMessage::Browser(BrowserMessage::Open(
                folder.clone(),
            ))),
        );
        drop(
            app.workspace_message(WorkspaceMessage::Browser(BrowserMessage::Open(
                parent.join("missing"),
            ))),
        );
        assert_eq!(app.workspace_dialog.last_directory.as_ref(), Some(&folder));
        drop(app.workspace_message(WorkspaceMessage::Browse(true)));
        drop(
            app.workspace_message(WorkspaceMessage::Browser(BrowserMessage::Open(
                parent.clone(),
            ))),
        );
        assert_eq!(app.workspace_dialog.last_directory.as_ref(), Some(&folder));
        drop(app.close_workspace_dialog());
        std::fs::remove_dir(&folder).unwrap();
        drop(app.open_workspace_dialog());
        assert_eq!(PathBuf::from(&app.workspace_dialog.folder), parent);
    }

    #[test]
    #[cfg(target_os = "linux")]
    fn browse_and_open_workspace_preserves_spaces_in_real_shell_directory() {
        let root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&root).unwrap();
        let fixture = tempfile::tempdir_in(root).unwrap();
        let folder = fixture.path().join("workspace with spaces ' and quotes");
        std::fs::create_dir(&folder).unwrap();
        let expected = folder.canonicalize().unwrap();
        let mut app = super::super::tests::desktop();
        drop(app.open_workspace_dialog());
        drop(app.workspace_message(WorkspaceMessage::Browser(BrowserMessage::Open(folder))));
        assert!(
            app.workspace_dialog
                .folders
                .as_ref()
                .unwrap()
                .error
                .is_empty()
        );
        assert_eq!(PathBuf::from(&app.workspace_dialog.folder), expected);
        drop(app.workspace_message(WorkspaceMessage::Start));
        assert!(!app.workspace_dialog.open);
        let workspace = &app.workspaces[app.active];
        assert_eq!(workspace.directory, expected);
        let terminal = workspace.panes.iter().next().unwrap().1.tabs[0]
            .terminal
            .as_ref()
            .expect("shell must launch");
        let pid = terminal.child_pid().unwrap();
        // Verify the native child actually changed directory, not just the UI label.
        assert_eq!(
            std::fs::read_link(format!("/proc/{pid}/cwd")).unwrap(),
            expected
        );
    }
}
