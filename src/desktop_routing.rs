//! Recipient discovery and ordinary presence share the configured gateway connection.
use super::*;
use amux::task_routing::{Directory, Routing, SessionTarget};
use iced::widget::column;

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub(super) enum Choice {
    #[default]
    Auto,
    Unassigned,
    Broadcast,
    Session(SessionTarget, String),
}

#[cfg(test)]
mod tests {
    use super::*;
    use amux::{delegation::Origin, task_routing::Available};
    fn target() -> SessionTarget {
        SessionTarget {
            origin: Origin {
                session_id: Uuid::new_v4(),
                instance_id: Uuid::new_v4(),
                provider: Provider::Codex,
                os: "windows".into(),
            },
            generation: Uuid::new_v4(),
        }
    }
    fn directory(target: SessionTarget) -> Directory {
        Directory {
            canonical_remote: Some("github.com/nitecon/amux".into()),
            targets: vec![Available {
                target,
                seen_at: unix_ms() as i64,
                expires_at: unix_ms() as i64 + 120_000,
                display_name: None,
                hostname: None,
            }],
        }
    }
    #[test]
    fn external_owner_label_uses_the_exact_live_owner_machine() {
        let mut app = super::super::tests::desktop();
        let owner = target();
        let mut directory = directory(owner.clone());
        directory.targets[0].display_name = Some("Richard Stallman - Claude".into());
        directory.targets[0].hostname = Some("Defcon2".into());
        let task = serde_json::json!({
            "owner_agent_id": "Richard Stallman - Claude",
            "owner_origin": owner.origin,
            "hostname": "Task creation machine"
        });
        let panel = &mut app.workspaces[0].gateway.routing;
        panel.directory = Some(directory);
        panel.checked = Some(Instant::now());
        assert_eq!(
            app.external_task_owner(&task),
            "Richard Stallman - Claude · Defcon2 (external)"
        );

        app.workspaces[0]
            .gateway
            .routing
            .directory
            .as_mut()
            .unwrap()
            .targets[0]
            .target
            .origin
            .instance_id = Uuid::new_v4();
        assert_eq!(
            app.external_task_owner(&task),
            "Richard Stallman - Claude · machine unavailable (external)"
        );
        app.workspaces[0]
            .gateway
            .routing
            .directory
            .as_mut()
            .unwrap()
            .targets[0]
            .target = owner;
        app.workspaces[0].gateway.routing.checked =
            Instant::now().checked_sub(Duration::from_secs(61));
        assert_eq!(
            app.external_task_owner(&task),
            "Richard Stallman - Claude · machine unavailable (external)"
        );
    }

    #[test]
    fn unavailable_selection_never_silently_becomes_automatic() {
        let old = target();
        let mut panel = Panel {
            choice: Choice::Session(old.clone(), "Codex".into()),
            directory: Some(directory(old.clone())),
            checked: Some(Instant::now()),
            ..Default::default()
        };
        assert!(panel.can_submit());
        let replacement = SessionTarget {
            generation: Uuid::new_v4(),
            ..old.clone()
        };
        panel.directory = Some(directory(replacement));
        assert!(!panel.can_submit());
        assert!(matches!(panel.choice, Choice::Session(..)));
        panel.choice = Choice::Broadcast;
        panel.checked = Instant::now().checked_sub(Duration::from_secs(61));
        assert!(!panel.can_submit());
        panel.choice = Choice::Auto;
        assert!(panel.can_submit());
    }
    #[test]
    fn recipient_callbacks_are_fenced_by_workspace_and_connection_revision() {
        let mut app = super::super::tests::desktop();
        let scope = app.scope(0);
        app.bind_project(0, "different".into());
        app.routing_loaded(scope, Ok((directory(target()), None)));
        assert!(app.workspaces[0].gateway.routing.directory.is_none());
        let scope = app.scope(1);
        let wanted = scope.workspace_id;
        let _ = app.remove_workspace(app.workspaces[0].id);
        let Some(Message::RoutingLoaded(scope, result)) = app.retarget_workspace_message(
            Message::RoutingLoaded(scope, Ok((directory(target()), None))),
        ) else {
            panic!("callback lost")
        };
        app.routing_loaded(scope, result);
        assert_eq!(app.workspaces[0].id, wanted);
        assert!(app.workspaces[0].gateway.routing.directory.is_some());
    }
}
impl std::fmt::Display for Choice {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Auto => f.write_str("Automatic project routing"),
            Self::Unassigned => f.write_str("Queue unassigned"),
            Self::Broadcast => f.write_str("Broadcast to workspace agents"),
            Self::Session(_, name) => f.write_str(name),
        }
    }
}
impl Choice {
    pub fn routing(&self) -> Routing {
        match self {
            Self::Auto => Routing::Auto,
            Self::Unassigned => Routing::Unassigned,
            Self::Broadcast => Routing::Broadcast,
            Self::Session(target, _) => Routing::Session {
                target: target.clone(),
            },
        }
    }
}

#[derive(Default)]
pub(super) struct Panel {
    pub choice: Choice,
    pub directory: Option<Directory>,
    pub checked: Option<Instant>,
    pub(super) polled: Option<Instant>,
    pub loading: bool,
    pub status: String,
}
impl Panel {
    pub fn available(&self, target: &SessionTarget) -> bool {
        self.fresh()
            && self
                .directory
                .as_ref()
                .is_some_and(|d| d.targets.iter().any(|entry| &entry.target == target))
    }
    fn fresh(&self) -> bool {
        self.directory.is_some()
            && self
                .checked
                .is_some_and(|at| at.elapsed() < Duration::from_secs(60))
    }
    pub fn can_submit(&self) -> bool {
        self.choice == Choice::Auto
            || self.fresh()
                && match &self.choice {
                    Choice::Session(target, _) => self.available(target),
                    _ => true,
                }
    }
}

impl Desktop {
    pub(super) fn external_task_owner(&self, task: &serde_json::Value) -> String {
        let panel = &self.workspaces[self.active].gateway.routing;
        let origin =
            serde_json::from_value::<amux::delegation::Origin>(task["owner_origin"].clone()).ok();
        let entry = panel
            .directory
            .as_ref()
            .filter(|_| panel.fresh())
            .and_then(|directory| {
                directory.targets.iter().find(|entry| {
                    Some(&entry.target.origin) == origin.as_ref()
                        && entry.expires_at > unix_ms() as i64
                })
            });
        let owner = entry
            .and_then(|entry| entry.display_name.as_deref())
            .filter(|name| !name.trim().is_empty())
            .or_else(|| task["owner_agent_id"].as_str())
            .unwrap_or("Unknown agent");
        let machine = entry
            .and_then(|entry| entry.hostname.as_deref())
            .filter(|name| !name.trim().is_empty())
            .unwrap_or("machine unavailable");
        format!("{owner} · {machine} (external)")
    }

    pub(super) fn refresh_task_targets(&mut self) -> Task<Message> {
        self.workspaces[self.active].gateway.routing.polled = None;
        self.routing_tick()
    }

    pub(super) fn task_agent_choices(&self) -> Vec<Choice> {
        let panel = &self.workspaces[self.active].gateway.routing;
        if !panel.fresh() {
            return Vec::new();
        }
        let mut choices = panel
            .directory
            .as_ref()
            .unwrap()
            .targets
            .iter()
            .map(|entry| {
                let target = &entry.target;
                let local = self.routing_name(target).is_some();
                let name = entry
                    .display_name
                    .clone()
                    .filter(|s| !s.trim().is_empty())
                    .or_else(|| self.routing_name(target))
                    .unwrap_or_else(|| target.origin.provider.command().to_owned());
                let machine = entry
                    .hostname
                    .as_deref()
                    .filter(|s| !s.trim().is_empty())
                    .unwrap_or("machine unavailable");
                Choice::Session(
                    target.clone(),
                    format!(
                        "{} · {name} · {machine} · {} · {}",
                        if local { "Local" } else { "Gateway" },
                        target.origin.os,
                        &target.origin.session_id.to_string()[..8]
                    ),
                )
            })
            .collect::<Vec<_>>();
        choices.sort_by_key(|choice| match choice {
            Choice::Session(target, name) => (self.routing_name(target).is_none(), name.clone()),
            _ => (true, String::new()),
        });
        choices
    }
    pub(super) fn routing_tick(&mut self) -> Task<Message> {
        // Do not spend the 30-second presence poll while a slow native scan
        // temporarily has no fresh actors. Retry on the next UI tick instead.
        if self
            .delegation
            .scan_completed
            .is_none_or(|at| at.elapsed() > Duration::from_secs(2))
        {
            return Task::none();
        }
        let Ok(gateway) = self.preferences.current.gateway() else {
            return Task::none();
        };
        let mut tasks = Vec::new();
        for index in 0..self.workspaces.len() {
            let scope = self.scope(index);
            let panel = &self.workspaces[index].gateway.routing;
            if scope.project.is_empty()
                || panel.loading
                || panel
                    .polled
                    .is_some_and(|at| at.elapsed() < Duration::from_secs(30))
            {
                continue;
            }
            let actors = self.routing_actors(scope.workspace_id);
            let panel = &mut self.workspaces[index].gateway.routing;
            panel.loading = true;
            panel.polled = Some(Instant::now());
            let gateway = gateway.clone();
            tasks.push(Task::perform(
                async move {
                    let result = async {
                        let directory = scope.directory.clone();
                        // A local-only checkout (no origin) is compared by its exact root.
                        let local = tokio::task::spawn_blocking(move || {
                            Ok::<_, anyhow::Error>((
                                amux::gateway_project::configured_origin(&directory)?,
                                amux::project_identity::resolve(&directory)?.directory,
                            ))
                        })
                        .await??;
                        let remote = local.0.as_deref();
                        let results = futures_util::future::join_all(actors.into_iter().map(
                            |(actor, generation, directory)| {
                                let gateway = &gateway;
                                let local = &local;
                                let project = &scope.project;
                                async move {
                                    let actual = tokio::task::spawn_blocking(move || {
                                        Ok::<_, anyhow::Error>((
                                            amux::gateway_project::configured_origin(&directory)?,
                                            amux::project_identity::resolve(&directory)?.directory,
                                        ))
                                    })
                                    .await?;
                                    let same = actual.as_ref().is_ok_and(|actual| {
                                        actual.0 == local.0
                                            && (actual.0.is_some() || actual.1 == local.1)
                                    });
                                    if !same {
                                        return Ok::<_, anyhow::Error>(());
                                    }
                                    let remote = local.0.as_deref();
                                    gateway
                                        .advertise_task_target(project, remote, &actor, generation)
                                        .await
                                }
                            },
                        ))
                        .await;
                        let warning = results
                            .into_iter()
                            .find_map(|result| result.err())
                            .map(|error| format!("Agent availability not confirmed: {error}"));
                        let directory = gateway.task_targets(&scope.project, remote).await?;
                        Ok::<_, anyhow::Error>((directory, warning))
                    }
                    .await
                    .map_err(|error| error.to_string());
                    (scope, result)
                },
                |(scope, result)| Message::RoutingLoaded(scope, result),
            ));
        }
        Task::batch(tasks)
    }

    pub(super) fn routing_loaded(
        &mut self,
        scope: QueryScope,
        result: Result<(Directory, Option<String>), String>,
    ) {
        if !self.scope_current(&scope) {
            return;
        }
        let panel = &mut self.workspaces[scope.workspace].gateway.routing;
        panel.loading = false;
        panel.checked = Some(Instant::now());
        match result {
            Ok((directory, warning)) => {
                panel.status = warning.unwrap_or_else(|| {
                    format!("{} available gateway agents", directory.targets.len())
                });
                panel.directory = Some(directory);
            }
            Err(error) => {
                panel.directory = None;
                panel.status =
                    format!("Recipients unavailable; automatic routing available. {error}");
            }
        }
        let diagnostic = format!("Agent presence: project={} {}", scope.project, panel.status);
        self.queue_diagnostic(diagnostic);
    }

    pub(super) fn routing_form(&self) -> Element<'_, Message> {
        let panel = &self.workspaces[self.active].gateway.routing;
        let mut choices = vec![Choice::Auto];
        if panel.fresh() {
            choices.extend([Choice::Unassigned, Choice::Broadcast]);
            choices.extend(self.task_agent_choices());
        }
        // Keep an unavailable selection visible; never silently retarget a user's draft.
        if !choices.contains(&panel.choice) {
            choices.push(panel.choice.clone());
        }
        column![
            iced::widget::pick_list(choices, Some(panel.choice.clone()), Message::TaskTarget)
                .width(Length::Fill),
            text(if panel.status.is_empty() {
                "Loading recipients…"
            } else {
                &panel.status
            })
            .size(10),
            text(match &panel.choice {
                Choice::Auto => "Gateway project routing; agents claim their own work.",
                Choice::Unassigned =>
                    "Save on the task board without an immediate agent notification.",
                Choice::Broadcast =>
                    "Notify matching workspace agents; one task, one eventual owner.",
                Choice::Session(..) => "Reserve for this exact live agent execution.",
            })
            .size(10),
        ]
        .spacing(5)
        .into()
    }
}
