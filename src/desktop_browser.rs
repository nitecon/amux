//! One folder/file browser shared by the workspace picker and the Files view:
//! back/forward history, an editable path, Places, a Git summary, a name filter
//! and a hidden-files toggle. A confined browser never leaves its root.
use super::*;
use amux::workspace_launch;
use iced::widget::column;
use std::{path::Path, time::SystemTime};

const MAX_ENTRIES: usize = 2000;

#[derive(Debug, Clone)]
pub(crate) enum BrowserMessage {
    Open(PathBuf),
    Select(PathBuf),
    Back,
    Forward,
    Up,
    Location(String),
    Go,
    Filter(String),
    ToggleHidden,
    NewFolder(Option<String>),
    CreateFolder,
}

struct Entry {
    path: PathBuf,
    name: String,
    directory: bool,
    hidden: bool,
    repository: bool,
    modified: Option<SystemTime>,
}

struct Git {
    branch: String,
    root: bool,
}

pub(crate) struct FileBrowser {
    pub(crate) directory: PathBuf,
    pub(crate) selected: Option<PathBuf>,
    pub(crate) error: String,
    pub(crate) files: bool,
    root: Option<PathBuf>,
    location: String,
    entries: Vec<Entry>,
    history: Vec<PathBuf>,
    index: usize,
    filter: String,
    show_hidden: bool,
    git: Option<Git>,
    new_folder: Option<String>,
}

impl FileBrowser {
    /// `files` also lists regular files; `root` confines navigation to that folder.
    pub(crate) fn new(directory: PathBuf, files: bool, root: Option<PathBuf>) -> Self {
        let root =
            root.map(|root| workspace_launch::directory(&root.to_string_lossy()).unwrap_or(root));
        let mut browser = Self {
            location: directory.display().to_string(),
            directory: directory.clone(),
            selected: None,
            error: String::new(),
            files,
            root,
            entries: Vec::new(),
            history: Vec::new(),
            index: 0,
            filter: String::new(),
            show_hidden: true,
            git: None,
            new_folder: None,
        };
        browser.navigate(directory, true);
        browser
    }

    /// The selected entry, or the current folder when nothing is selected.
    pub(crate) fn target(&self) -> &Path {
        self.selected.as_deref().unwrap_or(&self.directory)
    }

    pub(crate) fn update(&mut self, message: BrowserMessage) {
        use BrowserMessage::*;
        match message {
            Open(path) => {
                self.navigate(path, true);
            }
            Select(path) => self.selected = Some(path),
            Back | Forward => {
                let index = if matches!(message, Back) {
                    self.index.checked_sub(1)
                } else {
                    Some(self.index + 1).filter(|i| *i < self.history.len())
                };
                if let Some(index) = index
                    && self.navigate(self.history[index].clone(), false)
                {
                    self.index = index;
                }
            }
            Up => {
                if self.can_go_up()
                    && let Some(parent) = self.directory.parent()
                {
                    self.navigate(parent.to_path_buf(), true);
                }
            }
            Location(value) => self.location = value,
            Go => {
                let typed = self.location.trim();
                let path = match typed.strip_prefix('~') {
                    Some(rest) => workspace_launch::home_directory()
                        .map(|home| home.join(rest.trim_start_matches(['/', '\\'])))
                        .unwrap_or_else(|_| PathBuf::from(typed)),
                    None => PathBuf::from(typed),
                };
                self.navigate(path, true);
            }
            Filter(value) => self.filter = value.chars().take(256).collect(),
            ToggleHidden => self.show_hidden = !self.show_hidden,
            NewFolder(name) => self.new_folder = name,
            CreateFolder => {
                if let Some(name) = self.new_folder.take() {
                    match create_folder(&self.directory, &name) {
                        Ok(path) => {
                            self.navigate(self.directory.clone(), false);
                            self.selected = Some(path);
                        }
                        Err(error) => {
                            self.error = error;
                            self.new_folder = Some(name);
                        }
                    }
                }
            }
        }
    }

    fn can_go_up(&self) -> bool {
        self.directory.parent().is_some() && self.root.as_ref() != Some(&self.directory)
    }

    /// A failed navigation keeps the current listing and shows why.
    fn navigate(&mut self, path: PathBuf, record: bool) -> bool {
        let resolved = match workspace_launch::directory(&path.to_string_lossy()) {
            Ok(resolved) => resolved,
            Err(error) => {
                self.error = format!("{error:#}");
                return false;
            }
        };
        if let Some(root) = &self.root
            && !resolved.starts_with(root)
        {
            self.error = "Stay inside this workspace.".into();
            return false;
        }
        match list(&resolved) {
            Ok(entries) => self.entries = entries,
            Err(error) => {
                self.error = format!("Cannot browse {}: {error}", resolved.display());
                return false;
            }
        }
        self.git = git(&resolved);
        self.location = resolved.display().to_string();
        self.selected = None;
        self.error.clear();
        self.filter.clear();
        self.new_folder = None;
        if record {
            self.history.truncate(self.index + 1);
            if self.history.last() != Some(&resolved) {
                self.history.push(resolved.clone());
            }
            self.index = self.history.len() - 1;
        }
        self.directory = resolved;
        true
    }

    fn visible(&self) -> Vec<&Entry> {
        let filter = self.filter.trim().to_lowercase();
        self.entries
            .iter()
            .filter(|e| {
                (e.directory || self.files)
                    && (self.show_hidden || !e.hidden)
                    && (filter.is_empty() || e.name.to_lowercase().contains(&filter))
            })
            .collect()
    }

    fn places(&self) -> Vec<(&'static str, PathBuf)> {
        if let Some(root) = &self.root {
            return vec![("Project root", root.clone())];
        }
        let Ok(home) = workspace_launch::home_directory() else {
            return Vec::new();
        };
        let mut places = vec![
            (
                "Root",
                home.ancestors().last().unwrap_or(&home).to_path_buf(),
            ),
            ("Home", home.clone()),
        ];
        if let Some(documents) = directories::UserDirs::new()
            .and_then(|dirs| dirs.document_dir().map(PathBuf::from))
            .filter(|path| path.is_dir())
        {
            places.push(("Documents", documents));
        }
        places
    }

    pub(crate) fn view<'a>(
        &'a self,
        on: fn(BrowserMessage) -> Message,
        footer: Element<'a, Message>,
    ) -> Element<'a, Message> {
        use BrowserMessage::*;
        let nav = move |label: &'static str, message: Option<BrowserMessage>| {
            button(text(label).size(12))
                .padding([5, 9])
                .style(|theme, status| button_style(false, theme, status))
                .on_press_maybe(message.map(on))
        };
        let home = self
            .places()
            .into_iter()
            .find_map(|(label, path)| matches!(label, "Home" | "Project root").then_some(path));
        let toolbar = row![
            nav("←", (self.index > 0).then_some(Back)),
            nav(
                "→",
                (self.index + 1 < self.history.len()).then_some(Forward)
            ),
            nav("↑ Up", self.can_go_up().then_some(Up)),
            nav("⌂ Home", home.map(Open)),
            text_input("Type a path and press Enter", &self.location)
                .on_input(move |value| on(Location(value)))
                .on_submit(on(Go))
                .font(iced::Font::MONOSPACE)
                .size(12),
            nav("↵", Some(Go)),
        ]
        .spacing(4)
        .align_y(Alignment::Center);

        let mut places = column![text("PLACES").size(10).color(MUTED)].spacing(4);
        for (label, path) in self.places() {
            let current = self.directory == path;
            places = places.push(
                button(text(label).size(12))
                    .width(Length::Fill)
                    .style(move |theme, status| button_style(current, theme, status))
                    .on_press(on(Open(path))),
            );
        }
        places = places.push(Space::new().height(Length::Fill));
        if let Some(git) = &self.git {
            places = places.push(
                container(
                    column![
                        row![icon("git"), text(&git.branch).size(11).color(CYAN)]
                            .spacing(5)
                            .align_y(Alignment::Center),
                        text(if git.root {
                            "Repository root"
                        } else {
                            "Inside a Git repository"
                        })
                        .size(10)
                        .color(if git.root { GREEN } else { MUTED }),
                    ]
                    .spacing(4),
                )
                .padding(8)
                .width(Length::Fill)
                .style(|_| {
                    let mut style = surface(CARD, true);
                    style.border.color = Color::from_rgb8(14, 116, 144);
                    style
                }),
            );
        }
        let places = container(places)
            .width(170)
            .height(Length::Fill)
            .padding(10)
            .style(|_| surface(PANEL, true));

        let visible = self.visible();
        let mut list = column![].spacing(3);
        if let Some(name) = &self.new_folder {
            list = list.push(
                row![
                    icon("folder"),
                    text_input("New folder name", name)
                        .id("browser-new-folder")
                        .on_input(move |value| on(NewFolder(Some(value))))
                        .on_submit(on(CreateFolder))
                        .size(12),
                    nav("Create", Some(CreateFolder)),
                    nav("Cancel", Some(NewFolder(None))),
                ]
                .spacing(6)
                .align_y(Alignment::Center),
            );
        }
        for entry in &visible {
            let selected = self.selected.as_ref() == Some(&entry.path);
            let name = text(&entry.name).size(12).font(iced::Font::MONOSPACE);
            let enter: Element<'a, Message> = if entry.directory {
                button(text("›").size(13))
                    .padding([0, 6])
                    .style(button::text)
                    .on_press(on(Open(entry.path.clone())))
                    .into()
            } else {
                Space::new().width(22).into()
            };
            let content = row![
                row![
                    icon(if entry.repository {
                        "folder-git"
                    } else if entry.directory {
                        "folder"
                    } else {
                        "file"
                    }),
                    if entry.hidden {
                        name.color(MUTED)
                    } else {
                        name
                    },
                ]
                .spacing(8)
                .align_y(Alignment::Center)
                .width(Length::FillPortion(6)),
                text(if entry.repository {
                    "Git repository"
                } else if entry.directory {
                    "Folder"
                } else {
                    "File"
                })
                .size(11)
                .color(if entry.repository { GREEN } else { MUTED })
                .width(Length::FillPortion(3)),
                text(entry.modified.map(age).unwrap_or_else(|| "—".into()))
                    .size(11)
                    .color(MUTED)
                    .width(Length::FillPortion(2)),
                enter,
            ]
            .align_y(Alignment::Center);
            let card = container(content)
                .padding([6, 10])
                .width(Length::Fill)
                .style(move |_| {
                    let mut style = surface(
                        if selected {
                            Color::from_rgb8(8, 51, 68)
                        } else {
                            CARD
                        },
                        true,
                    );
                    if selected {
                        style.border.color = CYAN;
                    }
                    style
                });
            let mut area = mouse_area(card)
                .on_press(on(Select(entry.path.clone())))
                .interaction(iced::mouse::Interaction::Pointer);
            if entry.directory {
                area = area.on_double_click(on(Open(entry.path.clone())));
            }
            list = list.push(area);
        }
        if visible.is_empty() {
            list = list.push(
                text("Nothing here matches this view.")
                    .size(12)
                    .color(MUTED),
            );
        }
        let header = |label: &'static str, portion| {
            text(label)
                .size(10)
                .color(MUTED)
                .width(Length::FillPortion(portion))
        };
        let main = column![
            row![
                text_input("Filter this folder…", &self.filter)
                    .on_input(move |value| on(Filter(value)))
                    .size(12),
                button(
                    text(if self.show_hidden {
                        "Hidden files: shown"
                    } else {
                        "Hidden files: hidden"
                    })
                    .size(11)
                )
                .style(|theme, status| button_style(self.show_hidden, theme, status))
                .on_press(on(ToggleHidden)),
            ]
            .spacing(8)
            .align_y(Alignment::Center),
            row![
                header("NAME", 6),
                header("TYPE", 3),
                header("MODIFIED", 2),
                Space::new().width(22)
            ]
            .padding([0, 10]),
            scrollable(list).height(Length::Fill),
            row![
                text("Click to select · double-click a folder to open it")
                    .size(10)
                    .color(MUTED),
                Space::new().width(Length::Fill),
                text(format!("{} items", visible.len()))
                    .size(10)
                    .color(MUTED),
            ],
        ]
        .spacing(8)
        .width(Length::Fill);

        let mut body = column![toolbar].spacing(10);
        if !self.error.is_empty() {
            body = body.push(text(&self.error).size(11).color(RED));
        }
        body.push(row![places, main].spacing(10).height(Length::Fill))
            .push(footer)
            .into()
    }
}

fn list(directory: &Path) -> std::io::Result<Vec<Entry>> {
    let mut entries = Vec::new();
    for entry in std::fs::read_dir(directory)?.take(MAX_ENTRIES).flatten() {
        let path = entry.path();
        let name = entry.file_name().to_string_lossy().into_owned();
        // Follows symbolic links, like opening the entry would.
        let metadata = std::fs::metadata(&path).ok();
        let directory = metadata.as_ref().is_some_and(|m| m.is_dir());
        entries.push(Entry {
            hidden: name.starts_with('.') || hidden_attribute(&entry),
            repository: directory && path.join(".git").exists(),
            modified: metadata.and_then(|m| m.modified().ok()),
            path,
            name,
            directory,
        });
    }
    entries.sort_by(|a, b| {
        b.directory
            .cmp(&a.directory)
            .then_with(|| a.name.to_lowercase().cmp(&b.name.to_lowercase()))
    });
    Ok(entries)
}

#[cfg(windows)]
fn hidden_attribute(entry: &std::fs::DirEntry) -> bool {
    use std::os::windows::fs::MetadataExt;
    const FILE_ATTRIBUTE_HIDDEN: u32 = 0x2;
    entry
        .metadata()
        .is_ok_and(|m| m.file_attributes() & FILE_ATTRIBUTE_HIDDEN != 0)
}

#[cfg(not(windows))]
fn hidden_attribute(_: &std::fs::DirEntry) -> bool {
    false
}

fn create_folder(parent: &Path, name: &str) -> Result<PathBuf, String> {
    let name = name.trim();
    if name.is_empty()
        || matches!(name, "." | "..")
        || Path::new(name).file_name() != Some(std::ffi::OsStr::new(name))
    {
        return Err("Enter a folder name without slashes.".into());
    }
    let path = parent.join(name);
    std::fs::create_dir(&path)
        .map_err(|error| format!("Cannot create {}: {error}", path.display()))?;
    Ok(path)
}

fn git(directory: &Path) -> Option<Git> {
    let output = |args: &[&str]| {
        let mut command = std::process::Command::new("git");
        command
            .arg("-C")
            .arg(directory)
            .args(args)
            .stdin(std::process::Stdio::null())
            .stderr(std::process::Stdio::null());
        for (key, _) in std::env::vars_os() {
            if key.to_string_lossy().starts_with("GIT_") {
                command.env_remove(key);
            }
        }
        #[cfg(windows)]
        {
            use std::os::windows::process::CommandExt;
            command.creation_flags(windows_sys::Win32::System::Threading::CREATE_NO_WINDOW);
        }
        let output = command.output().ok()?;
        output
            .status
            .success()
            .then(|| String::from_utf8_lossy(&output.stdout).trim().to_owned())
    };
    let top = output(&["rev-parse", "--show-toplevel"])?;
    let root = PathBuf::from(&top)
        .canonicalize()
        .map(workspace_launch::external_path)
        .is_ok_and(|top| top == directory);
    let branch = output(&["symbolic-ref", "--short", "-q", "HEAD"])
        .filter(|branch| !branch.is_empty())
        .unwrap_or_else(|| "detached HEAD".into());
    Some(Git { branch, root })
}

fn age(time: SystemTime) -> String {
    let seconds = SystemTime::now()
        .duration_since(time)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    match seconds {
        0..60 => "just now".into(),
        60..3_600 => format!("{}m ago", seconds / 60),
        3_600..86_400 => format!("{}h ago", seconds / 3_600),
        86_400..2_592_000 => format!("{}d ago", seconds / 86_400),
        2_592_000..31_536_000 => format!("{}mo ago", seconds / 2_592_000),
        _ => format!("{}y ago", seconds / 31_536_000),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture() -> (tempfile::TempDir, PathBuf) {
        let root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&root).unwrap();
        let fixture = tempfile::tempdir_in(root).unwrap();
        let base = workspace_launch::directory(&fixture.path().to_string_lossy()).unwrap();
        for dir in ["Alpha project", ".hidden", "beta"] {
            std::fs::create_dir(base.join(dir)).unwrap();
        }
        std::fs::create_dir(base.join("beta/.git")).unwrap();
        std::fs::write(base.join("notes.txt"), "x").unwrap();
        (fixture, base)
    }

    fn names(browser: &FileBrowser) -> Vec<&str> {
        browser.visible().iter().map(|e| e.name.as_str()).collect()
    }

    #[test]
    fn folders_only_by_default_with_filter_hidden_toggle_and_repository_marker() {
        let (_fixture, base) = fixture();
        let mut browser = FileBrowser::new(base.clone(), false, None);
        assert_eq!(names(&browser), [".hidden", "Alpha project", "beta"]);
        assert!(
            browser
                .entries
                .iter()
                .any(|e| e.name == "beta" && e.repository)
        );
        browser.update(BrowserMessage::ToggleHidden);
        assert_eq!(names(&browser), ["Alpha project", "beta"]);
        browser.update(BrowserMessage::Filter("ALP".into()));
        assert_eq!(names(&browser), ["Alpha project"]);
        let files = FileBrowser::new(base, true, None);
        assert_eq!(
            names(&files),
            [".hidden", "Alpha project", "beta", "notes.txt"]
        );
    }

    #[test]
    fn history_up_typed_paths_and_failed_navigation_keep_the_current_folder() {
        let (_fixture, base) = fixture();
        let mut browser = FileBrowser::new(base.clone(), false, None);
        browser.update(BrowserMessage::Open(base.join("Alpha project")));
        assert_eq!(browser.directory, base.join("Alpha project"));
        browser.update(BrowserMessage::Up);
        assert_eq!(browser.directory, base);
        browser.update(BrowserMessage::Back);
        assert_eq!(browser.directory, base.join("Alpha project"));
        browser.update(BrowserMessage::Forward);
        assert_eq!(browser.directory, base);
        browser.update(BrowserMessage::Location(format!(
            "{}",
            base.join("beta").display()
        )));
        browser.update(BrowserMessage::Go);
        assert_eq!(browser.directory, base.join("beta"));
        browser.update(BrowserMessage::Open(base.join("missing")));
        assert_eq!(browser.directory, base.join("beta"));
        assert!(!browser.error.is_empty());
        browser.update(BrowserMessage::Select(base.join("beta/.git")));
        assert_eq!(browser.target(), base.join("beta/.git"));
    }

    #[test]
    fn confined_browser_stays_inside_its_root_and_new_folders_are_plain_names() {
        let (_fixture, base) = fixture();
        let root = base.join("beta");
        let mut browser = FileBrowser::new(root.clone(), true, Some(root.clone()));
        assert!(!browser.can_go_up());
        browser.update(BrowserMessage::Open(base.clone()));
        assert_eq!(browser.directory, root);
        assert_eq!(browser.places(), [("Project root", root.clone())]);
        for bad in ["", "..", "a/b"] {
            browser.update(BrowserMessage::NewFolder(Some(bad.into())));
            browser.update(BrowserMessage::CreateFolder);
            assert!(!browser.error.is_empty(), "{bad:?} accepted");
        }
        browser.update(BrowserMessage::NewFolder(Some("New one".into())));
        browser.update(BrowserMessage::CreateFolder);
        assert!(root.join("New one").is_dir());
        assert_eq!(
            browser.selected.as_deref(),
            Some(root.join("New one").as_path())
        );
    }
}
