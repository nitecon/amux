//! User-invoked global harness setup. Discovery never changes provider files.
use anyhow::{Context, Result, bail, ensure};
use clap::ValueEnum;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    ffi::OsString,
    fs,
    io::Write,
    path::{Path, PathBuf},
};

pub const START: &str = "<!-- amux:tasks:start -->";
pub const END: &str = "<!-- amux:tasks:end -->";
pub const RULES: &str = amux_gateway_backend::templates::TASK_HARNESS_RULES;
pub const CLAUDE_DENIES: &[&str] = &[
    "TaskCreate",
    "TaskGet",
    "TaskList",
    "TaskUpdate",
    "TodoWrite",
];

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, ValueEnum)]
#[serde(rename_all = "lowercase")]
pub enum Provider {
    Claude,
    Codex,
    Copilot,
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Deserialize, Serialize)]
pub enum Choice {
    #[default]
    Ask,
    Automatic,
    Manual,
}

#[derive(Debug, Clone, Default, Deserialize, Serialize)]
#[serde(default)]
pub struct Choices {
    claude: Choice,
    codex: Choice,
    copilot: Choice,
}
impl Choices {
    pub fn path() -> Result<PathBuf> {
        Ok(crate::identity::state_directory()?.join("harness-preferences.json"))
    }
    pub fn load(path: &Path) -> Result<Self> {
        let text = read(path)?;
        if text.trim().is_empty() {
            return Ok(Self::default());
        }
        serde_json::from_str(&text)
            .context("Saved agent instruction choices are invalid; original preserved")
    }
    pub fn save(&self, path: &Path) -> Result<()> {
        save(
            path,
            &read(path)?,
            &format!("{}\n", serde_json::to_string_pretty(self)?),
        )
    }
    pub fn get(&self, provider: Provider) -> Choice {
        match provider {
            Provider::Claude => self.claude,
            Provider::Codex => self.codex,
            Provider::Copilot => self.copilot,
        }
    }
    pub fn set(&mut self, provider: Provider, choice: Choice) {
        *match provider {
            Provider::Claude => &mut self.claude,
            Provider::Codex => &mut self.codex,
            Provider::Copilot => &mut self.copilot,
        } = choice;
    }
    pub fn needs_prompt(&self, report: &Report) -> bool {
        report.detected && !report.current() && self.get(report.provider) == Choice::Ask
    }
}
impl Provider {
    pub const ALL: [Self; 3] = [Self::Claude, Self::Codex, Self::Copilot];
    pub fn name(self) -> &'static str {
        match self {
            Self::Claude => "Claude",
            Self::Codex => "Codex",
            Self::Copilot => "Copilot",
        }
    }
    fn command(self) -> &'static str {
        match self {
            Self::Claude => "claude",
            Self::Codex => "codex",
            Self::Copilot => "copilot",
        }
    }
    pub fn enforcement(self) -> &'static str {
        match self {
            Self::Claude => {
                "Blocks Claude's local task tools in permissions.deny; keeps subagent and background tools available."
            }
            Self::Codex => {
                "Sets tools.update_plan.enabled=false in Codex user config. Requires a Codex version supporting this setting; profiles or launch overrides may change it."
            }
            Self::Copilot => {
                "Task tracking uses amux instructions. No supported local task-list disable setting verified for Copilot; its task subagent tool stays available."
            }
        }
    }
}

#[derive(Debug, Clone)]
pub struct Environment {
    pub home: PathBuf,
    pub path: OsString,
    pub codex_home: PathBuf,
    pub claude_home: PathBuf,
    pub copilot_home: PathBuf,
}
impl Environment {
    pub fn discover() -> Result<Self> {
        let home = directories::BaseDirs::new()
            .context("Home directory unavailable")?
            .home_dir()
            .to_path_buf();
        let config = |key, default| {
            std::env::var_os(key)
                .filter(|v| !v.is_empty())
                .map(PathBuf::from)
                .unwrap_or(default)
        };
        Ok(Self {
            codex_home: config("CODEX_HOME", home.join(".codex")),
            claude_home: config("CLAUDE_CONFIG_DIR", home.join(".claude")),
            copilot_home: config("COPILOT_HOME", home.join(".copilot")),
            home,
            path: std::env::var_os("PATH").unwrap_or_default(),
        })
    }
    pub fn provider_home(&self, provider: Provider) -> &Path {
        match provider {
            Provider::Claude => &self.claude_home,
            Provider::Codex => &self.codex_home,
            Provider::Copilot => &self.copilot_home,
        }
    }
    fn rules_path(&self, provider: Provider) -> Result<PathBuf> {
        let root = self.provider_home(provider);
        Ok(match provider {
            Provider::Claude => root.join("CLAUDE.md"),
            Provider::Copilot => root.join("copilot-instructions.md"),
            Provider::Codex => {
                let path = root.join("AGENTS.override.md");
                if read(&path)?.trim().is_empty() {
                    root.join("AGENTS.md")
                } else {
                    path
                }
            }
        })
    }
    fn settings_path(&self, provider: Provider) -> Option<PathBuf> {
        match provider {
            Provider::Claude => Some(self.claude_home.join("settings.json")),
            Provider::Codex => Some(self.codex_home.join("config.toml")),
            Provider::Copilot => None,
        }
    }
    fn executable(&self, provider: Provider) -> Option<PathBuf> {
        for directory in std::env::split_paths(&self.path).filter(|p| !p.as_os_str().is_empty()) {
            for suffix in if cfg!(windows) {
                &[".exe", ".cmd", ".bat"][..]
            } else {
                &[""][..]
            } {
                let path = directory.join(format!("{}{suffix}", provider.command()));
                if path.is_file() {
                    #[cfg(unix)]
                    {
                        use std::os::unix::fs::PermissionsExt;
                        if fs::metadata(&path).ok()?.permissions().mode() & 0o111 == 0 {
                            continue;
                        }
                    }
                    return Some(path);
                }
            }
        }
        None
    }
    pub fn reports(&self) -> Vec<Report> {
        Provider::ALL.into_iter().map(|p| self.report(p)).collect()
    }
    /// Startup maintenance applies only to providers the user opted into.
    pub fn maintain(&self, choices: &Choices) -> Vec<Report> {
        self.reports()
            .into_iter()
            .map(|report| {
                if !report.detected
                    || report.current()
                    || choices.get(report.provider) != Choice::Automatic
                {
                    return report;
                }
                match self.install(report.provider) {
                    Ok(updated) => updated,
                    Err(error) => {
                        let mut updated = self.report(report.provider);
                        updated.error =
                            Some(format!("Automatic rule update incomplete: {error:#}"));
                        updated
                    }
                }
            })
            .collect()
    }
    pub fn manual_instructions(&self, provider: Provider) -> Result<String> {
        let mut output = format!(
            "Add or update this managed block in {}. Preserve your other instructions.\n\n{START}\n{RULES}{END}\n",
            self.rules_path(provider)?.display()
        );
        match provider {
            Provider::Claude => output.push_str(&format!("\nIn {}, merge these tools into permissions.deny, preserving other settings:\n{}\n", self.claude_home.join("settings.json").display(), serde_json::to_string_pretty(&json!({"permissions":{"deny":CLAUDE_DENIES}}))?)),
            Provider::Codex => output.push_str(&format!("\nIn {}, set the following value, preserving other settings:\n[tools.update_plan]\nenabled = false\n", self.codex_home.join("config.toml").display())),
            Provider::Copilot => output.push_str("\nCopilot task tracking is controlled by instructions only; its subagent tools remain available.\n"),
        }
        output.push_str("\nStart a new provider session after editing.\n");
        Ok(output)
    }
    pub fn report(&self, provider: Provider) -> Report {
        let executable = self.executable(provider);
        let mut report = Report {
            provider,
            detected: executable.is_some() || self.provider_home(provider).is_dir(),
            executable,
            instructions: self.provider_home(provider).to_path_buf(),
            settings: self.settings_path(provider),
            rules: State::Missing,
            local_tasks: State::Missing,
            error: None,
        };
        let check = (|| -> Result<()> {
            report.instructions = self.rules_path(provider)?;
            let existing = read(&report.instructions)?;
            report.rules = if existing == merge_rules(&existing)? {
                State::Current
            } else if existing.contains(START) {
                State::Outdated
            } else {
                State::Missing
            };
            report.local_tasks = if let Some(settings) = self.settings_path(provider) {
                let text = read(&settings)?;
                if settings_current(provider, &text)? {
                    State::Current
                } else {
                    State::Missing
                }
            } else {
                State::GuidanceOnly
            };
            Ok(())
        })();
        if let Err(e) = check {
            report.error = Some(format!("{e:#}"));
        }
        report
    }
    /// Validate both files before writing either. Repeated installs are no-ops.
    pub fn install(&self, provider: Provider) -> Result<Report> {
        let instructions = self.rules_path(provider)?;
        let existing = read(&instructions)?;
        let updated = merge_rules(&existing)?;
        let settings = self
            .settings_path(provider)
            .map(|path| -> Result<_> {
                let old = read(&path)?;
                let new = merge_settings(provider, &old)?;
                Ok((path, old, new))
            })
            .transpose()?;
        // Configure task tools first: never report a successful rules-only install
        // if task settings are invalid or unwritable.
        if let Some((path, old, new)) = settings {
            save(&path, &old, &new)?;
        }
        save(&instructions, &existing, &updated)?;
        let report = self.report(provider);
        ensure!(
            report.current(),
            "Installation could not be verified: {:?}",
            report
        );
        Ok(report)
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
pub enum State {
    Missing,
    Outdated,
    Current,
    GuidanceOnly,
}
impl State {
    pub fn label(self) -> &'static str {
        match self {
            Self::Missing => "Missing",
            Self::Outdated => "Update available",
            Self::Current => "Current",
            Self::GuidanceOnly => "Instructions only",
        }
    }
}
#[derive(Debug, Clone, Serialize)]
pub struct Report {
    pub provider: Provider,
    pub detected: bool,
    pub executable: Option<PathBuf>,
    pub instructions: PathBuf,
    pub settings: Option<PathBuf>,
    pub rules: State,
    pub local_tasks: State,
    pub error: Option<String>,
}
impl Report {
    pub fn current(&self) -> bool {
        self.error.is_none()
            && self.rules == State::Current
            && matches!(self.local_tasks, State::Current | State::GuidanceOnly)
    }
}

/// Update only our one delimited block; refuse ambiguous or damaged boundaries.
pub fn merge_rules(existing: &str) -> Result<String> {
    let block = format!("{START}\n{}{END}", RULES);
    let starts: Vec<_> = existing.match_indices(START).map(|(i, _)| i).collect();
    let ends: Vec<_> = existing.match_indices(END).map(|(i, _)| i).collect();
    match (starts.as_slice(), ends.as_slice()) {
        ([], []) => Ok(format!(
            "{existing}{}{block}\n",
            if existing.is_empty() || existing.ends_with("\n\n") {
                ""
            } else if existing.ends_with('\n') {
                "\n"
            } else {
                "\n\n"
            }
        )),
        ([start], [end]) if start < end => {
            for (index, marker) in [(*start, START), (*end, END)] {
                let prefix = &existing[..index];
                let suffix = &existing[index + marker.len()..];
                ensure!(
                    (prefix.is_empty() || prefix.ends_with('\n'))
                        && (suffix.is_empty()
                            || suffix.starts_with('\n')
                            || suffix.starts_with("\r\n")),
                    "Amux task markers must occupy their own lines"
                );
            }
            Ok(format!(
                "{}{block}{}",
                &existing[..*start],
                &existing[*end + END.len()..]
            ))
        }
        _ => bail!(
            "Malformed or duplicate amux task block; repair its start/end markers before updating"
        ),
    }
}

fn read(path: &Path) -> Result<String> {
    match fs::read_to_string(path) {
        Ok(s) => Ok(s),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(String::new()),
        Err(e) => Err(e).with_context(|| format!("Read {}", path.display())),
    }
}

fn claude_settings(text: &str) -> Result<Value> {
    let mut doc: Value = if text.trim().is_empty() {
        json!({})
    } else {
        serde_json::from_str(text).context("Invalid Claude settings JSON")?
    };
    let root = doc
        .as_object_mut()
        .context("Claude settings must be a JSON object")?;
    let permissions = root
        .entry("permissions")
        .or_insert(json!({}))
        .as_object_mut()
        .context("Claude permissions must be an object")?;
    let deny = permissions
        .entry("deny")
        .or_insert(json!([]))
        .as_array_mut()
        .context("Claude permissions.deny must be an array")?;
    ensure!(
        deny.iter().all(Value::is_string),
        "Claude deny entries must be strings"
    );
    for tool in CLAUDE_DENIES {
        if !deny.iter().any(|v| v == tool) {
            deny.push(json!(tool));
        }
    }
    Ok(doc)
}
fn codex_settings(text: &str) -> Result<toml_edit::DocumentMut> {
    use toml_edit::{Item, Table, value};
    let mut doc = text
        .parse::<toml_edit::DocumentMut>()
        .context("Invalid Codex config TOML")?;
    let tools = doc
        .entry("tools")
        .or_insert(Item::Table(Table::new()))
        .as_table_like_mut()
        .context("Codex tools must be a table")?;
    let plan = tools
        .entry("update_plan")
        .or_insert(Item::Table(Table::new()))
        .as_table_like_mut()
        .context("Codex tools.update_plan must be a table")?;
    let mut disabled = value(false);
    if let Some(existing) = plan.get("enabled") {
        let existing = existing
            .as_value()
            .context("Codex tools.update_plan.enabled must be a boolean")?;
        ensure!(
            existing.as_bool().is_some(),
            "Codex tools.update_plan.enabled must be a boolean"
        );
        *disabled.as_value_mut().expect("boolean value").decor_mut() = existing.decor().clone();
    }
    plan.insert("enabled", disabled);
    Ok(doc)
}
fn settings_current(provider: Provider, text: &str) -> Result<bool> {
    Ok(match provider {
        Provider::Claude => {
            let updated = claude_settings(text)?;
            !text.trim().is_empty() && serde_json::from_str::<Value>(text)? == updated
        }
        Provider::Codex => {
            codex_settings(text)?;
            let doc = text.parse::<toml_edit::DocumentMut>()?;
            doc.get("tools")
                .and_then(|v| v.get("update_plan"))
                .and_then(|v| v.get("enabled"))
                .and_then(toml_edit::Item::as_bool)
                == Some(false)
        }
        Provider::Copilot => true,
    })
}
fn merge_settings(provider: Provider, text: &str) -> Result<String> {
    if settings_current(provider, text)? {
        return Ok(text.to_owned());
    }
    match provider {
        Provider::Claude => Ok(format!(
            "{}\n",
            serde_json::to_string_pretty(&claude_settings(text)?)?
        )),
        Provider::Codex => Ok(codex_settings(text)?.to_string()),
        Provider::Copilot => Ok(text.to_owned()),
    }
}

fn save(path: &Path, old: &str, new: &str) -> Result<()> {
    if old == new {
        return Ok(());
    }
    let path = if path.is_symlink() {
        fs::canonicalize(path)?
    } else {
        path.to_owned()
    };
    let parent = path
        .parent()
        .context("Harness file needs a parent directory")?;
    fs::create_dir_all(parent)?;
    ensure!(
        read(&path)? == old,
        "{} changed during setup; check again",
        path.display()
    );
    let mut tmp = tempfile::NamedTempFile::new_in(parent)?;
    if path.exists() {
        tmp.as_file()
            .set_permissions(fs::metadata(&path)?.permissions())?;
        let mut backup = path.as_os_str().to_owned();
        backup.push(".amux.bak");
        fs::copy(&path, PathBuf::from(backup)).context("Back up existing harness file")?;
    }
    tmp.write_all(new.as_bytes())?;
    tmp.as_file().sync_all()?;
    tmp.persist(&path)
        .with_context(|| format!("Save {}", path.display()))?;
    Ok(())
}
