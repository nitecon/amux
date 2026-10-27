use super::*;
use amux::harness::{Choice, Choices, Environment, Provider, Report};
use iced::widget::column;

#[derive(Default)]
pub(super) struct State {
    pub reports: Vec<Report>,
    pub busy: bool,
    pub status: String,
    pub choices: Choices,
    pub manual: Option<String>,
    pub notice_dismissed: bool,
}

#[derive(Clone, Copy)]
pub(super) enum Action {
    Startup,
    Check,
    Automatic(Provider),
    Manual(Provider),
}

#[derive(Debug, Clone)]
pub(super) struct Checked {
    pub reports: Vec<Report>,
    pub choices: Choices,
    pub status: String,
    pub manual: Option<String>,
}

impl State {
    pub fn needs_notice(&self) -> bool {
        !self.notice_dismissed
            && !self.busy
            && ((self.reports.is_empty() && !self.status.is_empty())
                || self.reports.iter().any(|report| {
                    self.choices.needs_prompt(report)
                        || (report.error.is_some()
                            && self.choices.get(report.provider) == Choice::Automatic)
                }))
    }
    // Complete opted-in file maintenance before restoring provider processes, so
    // restored sessions load the updated instructions rather than yesterday's block.
    pub fn startup() -> Self {
        let mut state = Self::default();
        match run(Action::Startup) {
            Ok(checked) => state.apply(checked),
            Err(error) => state.status = error,
        }
        state
    }
    pub fn apply(&mut self, checked: Checked) {
        self.reports = checked.reports;
        self.choices = checked.choices;
        self.status = checked.status;
        self.manual = checked.manual;
    }
}

fn run(action: Action) -> Result<Checked, String> {
    let environment = Environment::discover().map_err(|e| e.to_string())?;
    let path = Choices::path().map_err(|e| e.to_string())?;
    run_with(&environment, &path, action).map_err(|e| format!("{e:#}"))
}

fn run_with(
    environment: &Environment,
    path: &std::path::Path,
    action: Action,
) -> anyhow::Result<Checked> {
    let mut choices = Choices::load(path)?;
    let mut manual = None;
    let status = match action {
        Action::Automatic(provider) => {
            choices.set(provider, Choice::Automatic);
            choices.save(path)?;
            match environment.install(provider) {
                Ok(_) => format!("{} rules updated. Automatic startup maintenance enabled. Start a new provider session to load changes.", provider.name()),
                Err(e) => format!("Rule update incomplete: {e:#}. Automatic maintenance is enabled; review the file error."),
            }
        },
        Action::Manual(provider) => {
            choices.set(provider, Choice::Manual);
            choices.save(path)?;
            manual = Some(environment.manual_instructions(provider)?);
            format!("{} manual setup selected. amux will not edit these files or ask again. Use Update rules in Settings to enable automatic maintenance later.", provider.name())
        },
        Action::Startup => "Startup rules checked. Automatically maintained agents are verified; any file errors are shown below.".into(),
        Action::Check => "Check complete. Update rules or view manual instructions for the agents you use.".into(),
    };
    let reports = if matches!(action, Action::Startup) {
        environment.maintain(&choices)
    } else {
        environment.reports()
    };
    Ok(Checked {
        reports,
        choices,
        status,
        manual,
    })
}

impl Desktop {
    pub(super) fn check_harness(&mut self, action: Action) -> Task<Message> {
        if self.harness.busy {
            return Task::none();
        }
        self.harness.busy = true;
        self.harness.status = if matches!(action, Action::Automatic(_)) {
            "Installing instructions…"
        } else {
            "Checking installed agents…"
        }
        .into();
        Task::perform(
            async move {
                tokio::task::spawn_blocking(move || run(action))
                    .await
                    .map_err(|e| e.to_string())?
            },
            Message::HarnessChecked,
        )
    }

    pub(super) fn harness_suggestion(&self) -> Element<'_, Message> {
        let mut prompts = column![].spacing(6);
        if self.harness.reports.is_empty() && !self.harness.status.is_empty() && !self.harness.busy
        {
            prompts = prompts.push(row![
                text("Agent instruction settings could not be loaded. Review Agent Instructions in Settings.").size(12),
                button("Agent Instructions").on_press(Message::PreferencesTab(PreferencesTab::Harness)),
            ].spacing(8));
        }
        for report in &self.harness.reports {
            if self.harness.choices.needs_prompt(report) {
                prompts = prompts.push(container(column![
                    text(format!("{}: amux task rules need updating in {}. May amux maintain them automatically?", report.provider.name(), report.instructions.display())).size(12),
                    column![
                        button("Update rules automatically").on_press_maybe((!self.harness.busy).then_some(Message::InstallHarness(report.provider))),
                        button("Show rules; don’t ask again").on_press_maybe((!self.harness.busy).then_some(Message::ManualHarness(report.provider))),
                    ].spacing(8)
                ].spacing(4)).padding([6, 12]));
            } else if report.error.is_some()
                && self.harness.choices.get(report.provider) == Choice::Automatic
            {
                prompts = prompts.push(
                    container(
                        row![
                            text(format!(
                                "{} rules could not be updated. Review the file error in Settings.",
                                report.provider.name()
                            ))
                            .size(12),
                            button("Agent Instructions")
                                .on_press(Message::PreferencesTab(PreferencesTab::Harness)),
                        ]
                        .spacing(8),
                    )
                    .padding([6, 12]),
                );
            }
        }
        prompts.into()
    }

    pub(super) fn harness_view(&self) -> Element<'_, Message> {
        let mut content = column![
            text("Agent Instructions").size(20),
            text("Teach your coding agents to use amux for shared tasks. Choose automatic rule updates at each startup, or view the instructions for manual editing without future prompts. Your other instructions are preserved."),
            text("Update rules enables automatic startup maintenance immediately, independently of Save & Close. Restart provider sessions to load changes. Claude local tasks and Codex update_plan are disabled through their user settings; Copilot receives instructions only.").size(12),
            button(if self.harness.busy { "Checking…" } else { "Check installation and updates" })
                .on_press_maybe((!self.harness.busy).then_some(Message::CheckHarness)),
            text(&self.harness.status).size(12),
        ].spacing(12);
        if let Some(manual) = &self.harness.manual {
            content = content.push(
                column![
                    button("Copy manual instructions").on_press(Message::CopyHarnessRules),
                    text(manual).size(12),
                ]
                .spacing(8),
            );
        }
        for report in &self.harness.reports {
            let mut entry = column![
                text(format!(
                    "{} · {}",
                    report.provider.name(),
                    if report.detected {
                        "Detected"
                    } else {
                        "Not detected"
                    }
                ))
                .size(17),
                text(format!(
                    "Instructions: {} · Local task settings: {}",
                    report.rules.label(),
                    report.local_tasks.label()
                ))
                .size(12),
                text(report.instructions.display().to_string()).size(12),
                text(report.provider.enforcement()).size(12),
            ]
            .spacing(6);
            if let Some(path) = &report.settings {
                entry = entry.push(text(path.display().to_string()).size(12));
            }
            if let Some(error) = &report.error {
                entry = entry.push(text(error).size(12));
            }
            entry = entry.push(
                text(match self.harness.choices.get(report.provider) {
                    Choice::Ask => "Startup choice: not selected",
                    Choice::Automatic => "Startup choice: maintain rules automatically",
                    Choice::Manual => "Startup choice: manual editing; prompts disabled",
                })
                .size(12),
            );
            entry = entry.push(button("Update rules").on_press_maybe(
                (!self.harness.busy).then_some(Message::InstallHarness(report.provider)),
            ));
            entry = entry.push(button("Show rules; don’t ask again").on_press_maybe(
                (!self.harness.busy).then_some(Message::ManualHarness(report.provider)),
            ));
            content = content.push(container(entry).padding(12));
        }
        content.into()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dismissing_the_notice_does_not_choose_consent_or_open_settings() {
        let mut app = super::super::tests::desktop();
        app.harness.status = "Instructions need attention".into();
        assert!(app.harness.needs_notice());
        let _ = app.update(Message::DismissHarnessNotice);
        assert!(!app.harness.needs_notice());
        assert_eq!(app.harness.choices.get(Provider::Claude), Choice::Ask);
        let _ = app.update(Message::HarnessChecked(Err("Check failed".into())));
        assert!(!app.preferences.open);
        assert!(!app.harness.needs_notice());
        assert_eq!(app.toasts.items.len(), 1);
        let id = app.toasts.items[0].id;
        let _ = app.update(Message::DismissToast(id));
        assert!(app.toasts.items.is_empty());
    }

    #[test]
    fn suggestion_opens_instruction_settings_and_check_results_do_not_save_preferences() {
        let mut app = super::super::tests::desktop();
        let preferences = serde_json::to_value(&app.preferences.current).unwrap();
        let _ = app.update(Message::PreferencesTab(PreferencesTab::Harness));
        assert!(app.preferences.open);
        assert_eq!(app.preferences.tab, PreferencesTab::Harness);
        assert!(app.harness.busy);
        // Dropping the returned task means no user files or provider are touched.
        let _ = app.update(Message::HarnessChecked(Ok(Checked {
            reports: Vec::new(),
            choices: Choices::default(),
            status: "Verified fixture".into(),
            manual: None,
        })));
        assert!(!app.harness.busy);
        assert_eq!(app.harness.status, "Verified fixture");
        assert_eq!(
            serde_json::to_value(&app.preferences.current).unwrap(),
            preferences
        );
        let _ = app.update(Message::HarnessChecked(Err(
            "Unreadable instructions".into()
        )));
        assert_eq!(app.harness.status, "Unreadable instructions");
        assert_eq!(
            serde_json::to_value(&app.preferences.current).unwrap(),
            preferences
        );
    }

    #[test]
    fn manual_selection_survives_restart_and_manual_update_reenables_maintenance() {
        let target = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&target).unwrap();
        let temp = tempfile::tempdir_in(target).unwrap();
        let root = temp.path();
        let env = Environment {
            home: root.into(),
            path: Default::default(),
            claude_home: root.join("claude"),
            codex_home: root.join("codex"),
            copilot_home: root.join("copilot"),
        };
        std::fs::create_dir(&env.claude_home).unwrap();
        let path = root.join("harness-preferences.json");
        let manual = run_with(&env, &path, Action::Manual(Provider::Claude)).unwrap();
        assert!(manual.manual.unwrap().contains("CLAUDE.md"));
        assert_eq!(manual.choices.get(Provider::Claude), Choice::Manual);
        assert!(!env.claude_home.join("CLAUDE.md").exists());
        let startup = run_with(&env, &path, Action::Startup).unwrap();
        assert!(!startup.choices.needs_prompt(&startup.reports[0]));
        assert!(!env.claude_home.join("CLAUDE.md").exists());
        let automatic = run_with(&env, &path, Action::Automatic(Provider::Claude)).unwrap();
        assert_eq!(automatic.choices.get(Provider::Claude), Choice::Automatic);
        assert!(automatic.reports[0].current());
        std::fs::remove_file(env.claude_home.join("CLAUDE.md")).unwrap();
        let checked = run_with(&env, &path, Action::Check).unwrap();
        assert!(
            !checked.reports[0].current(),
            "manual check must not write files"
        );
        let startup = run_with(&env, &path, Action::Startup).unwrap();
        assert!(startup.reports[0].current());
    }
}
