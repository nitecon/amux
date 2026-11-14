//! Settings use one catalog for launch profiles and per-task delegation guidance.
use super::*;
use amux::model_profiles::{Catalog, Profile};
use iced::widget::{column, pick_list, row, text};

#[derive(Debug, Clone, Copy)]
pub(super) enum Target {
    Launch,
    Code,
    Basic,
    Complex,
    Terminal(u64),
}
#[derive(Debug, Clone)]
pub(super) enum Action {
    Model(Provider, Target, String),
    Thinking(Provider, Target, String),
    Inherit(Provider, u64),
}
fn profile_mut(
    settings: &mut amux::model_profiles::Preferences,
    provider: Provider,
    target: Target,
) -> &mut Profile {
    if let Target::Terminal(number) = target {
        let default = settings.provider(provider).launch.clone();
        return settings
            .terminals
            .entry(format!("{}:{number}", provider.command()))
            .or_insert(default);
    }
    let profiles = settings.provider_mut(provider);
    match target {
        Target::Launch => &mut profiles.launch,
        Target::Code => &mut profiles.code,
        Target::Basic => &mut profiles.basic,
        Target::Complex => &mut profiles.complex,
        Target::Terminal(_) => unreachable!(),
    }
}
impl Desktop {
    pub(super) fn models_message(&mut self, action: Action) {
        let catalog = match Catalog::load() {
            Ok(c) => c,
            Err(e) => {
                self.preferences.status = format!("Not saved: {e:#}");
                return;
            }
        };
        match action {
            Action::Inherit(provider, number) => {
                self.preferences
                    .draft
                    .models
                    .terminals
                    .remove(&format!("{}:{number}", provider.command()));
            }
            Action::Model(provider, target, value) => {
                let profile = profile_mut(&mut self.preferences.draft.models, provider, target);
                profile.model = if value == "Provider default" {
                    String::new()
                } else {
                    value
                };
                if catalog.validate(provider, profile).is_err() {
                    profile.thinking.clear();
                }
            }
            Action::Thinking(provider, target, value) => {
                profile_mut(&mut self.preferences.draft.models, provider, target).thinking =
                    if value == "Default" {
                        String::new()
                    } else {
                        value
                    };
            }
        }
    }
    pub(super) fn models_view(&self) -> Element<'_, Message> {
        let catalog = match Catalog::load() {
            Ok(c) => c,
            Err(e) => {
                return column![
                    text("Models & Routing").size(22),
                    text(format!(
                        "{e:#}. Restore models.json in the installation directory."
                    ))
                ]
                .spacing(14)
                .into();
            }
        };
        let mut content = column![text("Models & Routing").size(22),
            text("Native subagents first. Current routing choices are included with every task delivery. The lead keeps ownership, reviews and submits. Complex work takes precedence over routine defaults.").size(13),
            text("Launch profiles apply on the next launch/restart. Restart a terminal explicitly to apply a changed profile; its recorded conversation is retained. Provider default leaves the provider model/effort unchanged.").size(12),
            text(format!("Model catalog: {}", Catalog::path().map(|p| p.display().to_string()).unwrap_or_default())).size(12)
        ].spacing(14);
        for provider in [Provider::Codex, Provider::Claude] {
            let profiles = self.preferences.draft.models.provider(provider);
            content = content.push(text(provider.command()).size(18));
            for (label, target, profile) in [
                ("Terminal launch default", Target::Launch, &profiles.launch),
                ("Routine code", Target::Code, &profiles.code),
                ("Research / basic", Target::Basic, &profiles.basic),
                ("Complex work", Target::Complex, &profiles.complex),
            ] {
                content = content.push(profile_row(
                    &catalog,
                    provider,
                    target,
                    label.to_owned(),
                    profile,
                ));
            }
        }
        content = content.push(text("Persona / terminal overrides").size(18));
        content = content.push(text("Terminal numbers retain their assigned persona across restarts. Unlisted/new terminals use the provider launch default.").size(12));
        for workspace in &self.workspaces {
            for (_, pane) in workspace.panes.iter() {
                for tab in &pane.tabs {
                    let Some(provider) = tab.delegation.provider.or(tab.provider) else {
                        continue;
                    };
                    let number = tab.delegation.terminal_number;
                    if number == 0 {
                        continue;
                    }
                    let profile = self.preferences.draft.models.launch(provider, number);
                    let label = format!(
                        "{} · {} #{}",
                        workspace.directory.display(),
                        tab.delegation
                            .agent_name
                            .as_deref()
                            .unwrap_or(provider.command()),
                        number
                    );
                    content = content.push(profile_row(
                        &catalog,
                        provider,
                        Target::Terminal(number),
                        label,
                        profile,
                    ));
                    if self
                        .preferences
                        .draft
                        .models
                        .terminals
                        .contains_key(&format!("{}:{number}", provider.command()))
                    {
                        content = content.push(
                            button("Use provider launch default")
                                .on_press(Message::Models(Action::Inherit(provider, number))),
                        );
                    }
                }
            }
        }
        content.into()
    }
}
fn profile_row(
    catalog: &Catalog,
    provider: Provider,
    target: Target,
    label: String,
    profile: &Profile,
) -> Element<'static, Message> {
    let mut models: Vec<String> = catalog
        .models
        .iter()
        .filter(|m| m.provider == provider)
        .map(|m| m.id.clone())
        .collect();
    if matches!(target, Target::Launch | Target::Terminal(_)) {
        models.insert(0, "Provider default".into());
    }
    // Retain obsolete selections visibly so a catalog update never silently changes a profile.
    if !profile.model.is_empty() && !models.contains(&profile.model) {
        models.push(profile.model.clone());
    }
    let selected = if profile.model.is_empty() {
        "Provider default".into()
    } else {
        profile.model.clone()
    };
    let mut thinking = vec!["Default".to_owned()];
    if let Some(model) = catalog
        .models
        .iter()
        .find(|m| m.provider == provider && m.id == profile.model)
    {
        thinking.extend(model.thinking.clone());
    }
    if !profile.thinking.is_empty() && !thinking.contains(&profile.thinking) {
        thinking.push(profile.thinking.clone());
    }
    let effort = if profile.thinking.is_empty() {
        "Default".into()
    } else {
        profile.thinking.clone()
    };
    column![
        text(label).size(13),
        row![
            pick_list(models, Some(selected), move |v| Message::Models(
                Action::Model(provider, target, v)
            ))
            .width(250),
            pick_list(thinking, Some(effort), move |v| Message::Models(
                Action::Thinking(provider, target, v)
            ))
            .width(140),
        ]
        .spacing(10)
    ]
    .spacing(5)
    .into()
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn settings_cancel_and_save_keep_launch_overrides_separate_from_routing() {
        let (_root, mut app) = super::super::tests::preferences_fixture();
        app.models_message(Action::Model(
            Provider::Codex,
            Target::Terminal(7),
            "gpt-6-luna".into(),
        ));
        app.models_message(Action::Thinking(
            Provider::Codex,
            Target::Terminal(7),
            "medium".into(),
        ));
        app.models_message(Action::Model(
            Provider::Claude,
            Target::Basic,
            "sonnet".into(),
        ));
        assert_eq!(app.preferences.current.models.claude.basic.model, "haiku");
        drop(app.close_preferences());
        drop(app.open_preferences(PreferencesTab::Models));
        assert_eq!(app.preferences.draft.models.claude.basic.model, "haiku");
        app.models_message(Action::Model(
            Provider::Codex,
            Target::Terminal(7),
            "gpt-6-luna".into(),
        ));
        app.models_message(Action::Thinking(
            Provider::Codex,
            Target::Terminal(7),
            "medium".into(),
        ));
        drop(app.save_preferences());
        let restored = GatewayPreferences::load(app.preferences.path.as_deref().unwrap())
            .unwrap()
            .unwrap();
        assert_eq!(
            restored.models.launch(Provider::Codex, 7),
            &Profile::new("gpt-6-luna", "medium")
        );
        assert!(restored.models.launch(Provider::Codex, 8).model.is_empty());
        assert_eq!(restored.models.claude.basic.model, "haiku");
    }
}
