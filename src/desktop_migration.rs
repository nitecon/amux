use super::*;
use amux::migration::{self, Installed, Preview};
use iced::widget::column;

#[derive(Default)]
pub(super) struct State {
    pub open: bool,
    pub busy: bool,
    source: String,
    remote: bool,
    preview: Option<Preview>,
    installed: bool,
    status: String,
}

#[derive(Debug, Clone)]
pub(super) enum Message {
    Open,
    Close,
    Source(String),
    Remote(bool),
    Prepare,
    Prepared(Result<Preview, String>),
    Import,
    Imported(Result<Installed, String>),
    Activate,
}

impl Desktop {
    pub(super) fn migration_message(&mut self, message: Message) -> Task<super::Message> {
        if self.migration.busy && !matches!(message, Message::Prepared(_) | Message::Imported(_)) {
            return Task::none();
        }
        match message {
            Message::Open => {
                let source = amux_gateway_backend::db::DEFAULT_SQLITE_DATABASE_PATH;
                self.migration = State {
                    open: true,
                    source: if std::path::Path::new(source).is_file() {
                        source.into()
                    } else {
                        String::new()
                    },
                    remote: !std::path::Path::new(source).is_file(),
                    ..State::default()
                };
            }
            Message::Close => self.migration = State::default(),
            Message::Source(value) if value.len() <= 4096 => {
                self.migration.source = value;
                self.migration.preview = None;
                self.migration.installed = false;
            }
            Message::Remote(remote) => {
                self.migration.remote = remote;
                self.migration.preview = None;
                self.migration.installed = false;
            }
            Message::Prepare => {
                let Some(directory) = self.registry.clone() else {
                    return Task::none();
                };
                self.migration.busy = true;
                self.migration.preview = None;
                self.migration.installed = false;
                self.migration.status = "Preparing a consistent snapshot and checking data…".into();
                let remote = self.migration.remote;
                let source = self.migration.source.clone();
                let url = self.preferences.draft.url.clone();
                let key = self.preferences.draft.api_key.clone();
                return Task::perform(
                    async move {
                        if remote {
                            migration::prepare_remote(&url, &key, &directory)
                                .await
                                .map_err(|error| error.to_string())
                        } else {
                            tokio::task::spawn_blocking(move || {
                                migration::prepare_local(std::path::Path::new(&source), &directory)
                            })
                            .await
                            .map_err(|error| error.to_string())?
                            .map_err(|error| error.to_string())
                        }
                    },
                    |result| super::Message::Migration(Message::Prepared(result)),
                );
            }
            Message::Prepared(result) => {
                self.migration.busy = false;
                match result {
                    Ok(preview) => {
                        self.migration.status = preview.summary();
                        self.migration.preview = Some(preview);
                    }
                    Err(error) => self.migration.status = error,
                }
            }
            Message::Import
                if self.preferences.current.mode == GatewayMode::Legacy
                    && self.embedded.local.is_none() =>
            {
                let (Some(preview), Some(directory)) =
                    (self.migration.preview.clone(), self.registry.clone())
                else {
                    return Task::none();
                };
                self.migration.busy = true;
                self.migration.status =
                    "Installing the verified snapshot into this machine's database…".into();
                return Task::perform(
                    async move {
                        tokio::task::spawn_blocking(move || {
                            migration::install(&preview, &directory)
                        })
                        .await
                        .map_err(|error| error.to_string())?
                        .map_err(|error| error.to_string())
                    },
                    |result| super::Message::Migration(Message::Imported(result)),
                );
            }
            Message::Imported(result) => {
                self.migration.busy = false;
                match result {
                    Ok(installed) => {
                        self.migration.installed = true;
                        self.migration.status = format!(
                            "Imported into {}. Legacy connection is still active. Switch when you are ready; this snapshot does not include later gateway writes.",
                            installed.database.display()
                        );
                        self.toasts.show("migration", "Gateway data imported", "Verified database installed. Choose when to switch to the local server in Settings.");
                    }
                    Err(error) => self.migration.status = error,
                }
            }
            Message::Activate
                if self.migration.installed
                    && self.preferences.current.mode == GatewayMode::Legacy =>
            {
                let Some(directory) = self.registry.as_ref() else {
                    return Task::none();
                };
                let backup = directory.join(format!("legacy-connection-{}.json", Uuid::new_v4()));
                let result = serde_json::to_vec_pretty(&self.preferences.current)
                    .map_err(anyhow::Error::from)
                    .and_then(|bytes| {
                        amux_gateway_backend::security::write_private(&backup, &bytes)
                    });
                if let Err(error) = result {
                    self.migration.status =
                        format!("Connection backup failed; not switched: {error}");
                    return Task::none();
                }
                self.preferences.draft.mode = GatewayMode::Server;
                self.preferences.draft.enrolled = None;
                self.migration.open = false;
                return self.save_preferences();
            }
            _ => {}
        }
        Task::none()
    }

    pub(super) fn migration_view(&self) -> Element<'_, super::Message> {
        let action = |message| super::Message::Migration(message);
        if !self.migration.open {
            return button("Migrate gateway data to this machine")
                .on_press_maybe(
                    (!self.embedded.busy && !self.embedded.stopping)
                        .then_some(action(Message::Open)),
                )
                .into();
        }
        let available = !self.migration.busy;
        let mut content = column![
            text("Migrate to the local server").size(18),
            text("1. Choose source → 2. Review snapshot → 3. Import → 4. Switch connection"),
            text("Imports the complete gateway SQLite database, including tasks, comments, messages, memories, patterns, skills, artifacts and history. The source stays unchanged. Use an unused local server; existing local data cannot be merged."),
            text("Plugin channel/room records are preserved. Configure the same channel plugins on this host before switching; plugin tokens and environment files are not copied."),
            row![button("Local database / snapshot").on_press_maybe(available.then_some(action(Message::Remote(false)))), button("Download from gateway").on_press_maybe(available.then_some(action(Message::Remote(true))))].spacing(6),
        ].spacing(10);
        if self.migration.remote {
            content = content.push(text("Uses the gateway address and operator API key above. Requires a gateway version with migration exports; older gateways must provide a consistent SQLite backup file."));
        } else {
            let input = text_input(
                "Path to gateway SQLite database or exported backup",
                &self.migration.source,
            );
            content = content.push(if available {
                input.on_input(|value| super::Message::Migration(Message::Source(value)))
            } else {
                input
            });
        }
        if let Some(preview) = &self.migration.preview {
            content = content
                .push(text(format!("Source: {}", preview.inventory.source)).size(12))
                .push(text(preview.summary()))
                .push(text(format!("SHA-256: {}", preview.inventory.sha256)).size(12));
            let counts = preview
                .inventory
                .tables
                .iter()
                .map(|(name, count)| format!("{name}: {count}"))
                .collect::<Vec<_>>()
                .join(" · ");
            content = content.push(text(counts).size(12));
            for note in &preview.inventory.schema_notes {
                content = content.push(text(note).size(12));
            }
        }
        content = content.push(text(&self.migration.status).size(12));
        if self.migration.installed {
            content = content.push(text("Before switching, stop writes to the legacy gateway and ensure the final snapshot includes them. Other machines keep their existing connections until you pair them with this server."))
                .push(button("Switch this machine to local server").on_press_maybe(available.then_some(action(Message::Activate))));
        } else {
            content = content.push(
                row![
                    button("Prepare and review snapshot")
                        .on_press_maybe(available.then_some(action(Message::Prepare))),
                    button("Import verified snapshot").on_press_maybe(
                        (available && self.migration.preview.is_some())
                            .then_some(action(Message::Import))
                    ),
                ]
                .spacing(8),
            );
        }
        content
            .push(
                button("Close wizard").on_press_maybe(available.then_some(action(Message::Close))),
            )
            .into()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn opening_reviewing_and_importing_do_not_switch_connections_or_choose_consent() {
        let mut app = super::super::tests::desktop();
        app.preferences.current.mode = GatewayMode::Legacy;
        app.preferences.current.url = "https://legacy.example".into();
        let original = serde_json::to_value(&app.preferences.current).unwrap();
        drop(app.migration_message(Message::Open));
        assert!(app.migration.open);
        drop(app.migration_message(Message::Activate));
        assert_eq!(
            serde_json::to_value(&app.preferences.current).unwrap(),
            original
        );
        // No file or process action is performed by these completion messages.
        drop(app.migration_message(Message::Imported(Ok(Installed {
            database: PathBuf::from("fixture/application.sqlite"),
            receipt: PathBuf::from("fixture/migration.json"),
        }))));
        assert!(app.migration.installed);
        assert_eq!(
            serde_json::to_value(&app.preferences.current).unwrap(),
            original
        );
        assert!(app.embedded.local.is_none());
        drop(app.migration_message(Message::Source("different-snapshot.sqlite".into())));
        assert!(!app.migration.installed && app.migration.preview.is_none());
        drop(app.migration_message(Message::Close));
        assert!(!app.migration.open);
    }

    #[test]
    fn migration_in_progress_blocks_saving_or_switching_server_mode() {
        let mut app = super::super::tests::desktop();
        app.migration.busy = true;
        app.preferences.current.mode = GatewayMode::Legacy;
        app.preferences.draft.mode = GatewayMode::Legacy;
        drop(app.update(super::super::Message::GatewayMode(GatewayMode::Server)));
        assert_eq!(app.preferences.draft.mode, GatewayMode::Legacy);
        drop(app.update(super::super::Message::SavePreferences));
        assert!(!app.embedded.busy);
        drop(app.migration_message(Message::Close));
        assert!(app.migration.busy);
    }
}
