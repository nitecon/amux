use super::*;
use amux::harness::{Environment, Provider, Report};
use iced::widget::column;

#[derive(Default)]
pub(super) struct State {
    pub reports: Vec<Report>,
    pub busy: bool,
    pub status: String,
}

impl Desktop {
    pub(super) fn check_harness(&mut self, install: Option<Provider>) -> Task<Message> {
        if self.harness.busy {
            return Task::none();
        }
        self.harness.busy = true;
        self.harness.status = if install.is_some() {
            "Installing instructions…"
        } else {
            "Checking installed agents…"
        }
        .into();
        Task::perform(
            async move {
                tokio::task::spawn_blocking(move || -> Result<_, String> {
                let environment = Environment::discover().map_err(|e| e.to_string())?;
                let status = match install {
                    Some(provider) => match environment.install(provider) {
                        Ok(_) => format!("{} instructions installed. Start a new provider session to load the changes. Changes apply immediately; existing files have .amux.bak backups.", provider.name()),
                        Err(e) => format!("Installation incomplete: {e:#}. Check the reported files and retry."),
                    },
                    None => "Check complete. Install or update instructions for the agents you use.".into(),
                };
                Ok((environment.reports(), status))
            }).await.map_err(|e| e.to_string())?
            },
            Message::HarnessChecked,
        )
    }

    pub(super) fn harness_suggestion(&self) -> Element<'_, Message> {
        if !self
            .harness
            .reports
            .iter()
            .any(|r| r.detected && !r.current())
        {
            return column![].into();
        }
        container(
            row![
                text("Your coding agents need amux task instructions. Review them in Settings.")
                    .size(12),
                button("Agent Instructions")
                    .on_press(Message::PreferencesTab(PreferencesTab::Harness))
            ]
            .spacing(12),
        )
        .padding([6, 12])
        .into()
    }

    pub(super) fn harness_view(&self) -> Element<'_, Message> {
        let mut content = column![
            text("Agent Instructions").size(20),
            text("Teach installed coding agents to use amux for shared tasks, ownership and delegation. amux maintains one marked block in each provider’s user instructions and preserves your other text. Check again after updating amux."),
            text("Install/update applies immediately to this machine, independently of Save & Close. Restart provider sessions to load changes. Claude local task tools and Codex update_plan are disabled through their user settings; Copilot currently receives task-tracking instructions only.").size(12),
            button(if self.harness.busy { "Checking…" } else { "Check installation and updates" })
                .on_press_maybe((!self.harness.busy).then_some(Message::CheckHarness)),
            text(&self.harness.status).size(12),
        ].spacing(12);
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
                button(if report.current() {
                    "Verify / update"
                } else {
                    "Install / update instructions"
                })
                .on_press_maybe(
                    (!self.harness.busy).then_some(Message::InstallHarness(report.provider)),
                ),
            );
            content = content.push(container(entry).padding(12));
        }
        content.into()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn suggestion_opens_instruction_settings_and_check_results_do_not_save_preferences() {
        let mut app = super::super::tests::desktop();
        let preferences = serde_json::to_value(&app.preferences.current).unwrap();
        let _ = app.update(Message::PreferencesTab(PreferencesTab::Harness));
        assert!(app.preferences.open);
        assert_eq!(app.preferences.tab, PreferencesTab::Harness);
        assert!(app.harness.busy);
        // Dropping the returned task means no user files or provider are touched.
        let _ = app.update(Message::HarnessChecked(Ok((
            Vec::new(),
            "Verified fixture".into(),
        ))));
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
}
