//! Existing-task actions stay in the gateway sidebar and are attributed to the user.
use super::*;
use amux::task_management::TaskEdits;
use desktop_routing::Choice;
use iced::widget::column;
use reqwest::Method;
use serde_json::Value;

#[derive(Debug, Clone)]
pub(super) enum Action {
    Open(Uuid),
    AssignTask(Uuid),
    Back,
    Refresh,
    Edit,
    Cancel,
    Title(String),
    Description(text_editor::Action),
    Specification(text_editor::Action),
    Comment(text_editor::Action),
    ShowAgents,
    Target(Choice),
    Save,
    PostComment,
    Assign,
    Close,
    Resume,
}

#[derive(Debug, Clone, Copy)]
pub(super) enum Change {
    Edit,
    Comment,
    Assign,
    Close,
    Resume,
}

#[derive(Default, PartialEq)]
enum Mode {
    #[default]
    View,
    Edit,
    Assign,
}

#[derive(Default)]
pub(super) struct Panel {
    pub selected: Option<Uuid>,
    task: Option<Value>,
    loading: Option<Uuid>,
    pending: Option<Uuid>,
    uncertain: bool,
    mode: Mode,
    title: String,
    description: text_editor::Content,
    specification: text_editor::Content,
    comment: text_editor::Content,
    recipient: Option<Choice>,
    status: String,
}

impl Panel {
    pub fn refresh_with_list(&self) -> bool {
        self.selected.is_some()
            && self.mode == Mode::View
            && self.loading.is_none()
            && !self.unresolved()
    }
    pub fn unresolved(&self) -> bool {
        self.pending.is_some() || self.uncertain
    }
    fn ready(&self) -> bool {
        self.task.is_some() && self.loading.is_none() && !self.unresolved()
    }
}

enum Mutation {
    Edit(TaskEdits),
    Comment(String),
    Assign(amux::task_routing::SessionTarget),
    Close,
    Resume,
}

fn message(action: Action) -> Message {
    Message::ManageTask(action)
}

impl Desktop {
    pub(super) fn refresh_open_task(&mut self, index: usize) -> Task<Message> {
        self.load_task_detail(index)
    }
    pub(super) fn manage_task(&mut self, action: Action) -> Task<Message> {
        let panel = &mut self.workspaces[self.active].gateway.detail;
        match action {
            Action::Open(id) | Action::AssignTask(id) => {
                if panel.unresolved() {
                    panel.status =
                        "Refresh and inspect the current operation before leaving this task."
                            .into();
                    return Task::none();
                }
                *panel = Panel {
                    selected: Some(id),
                    mode: if matches!(action, Action::AssignTask(_)) {
                        Mode::Assign
                    } else {
                        Mode::View
                    },
                    ..Default::default()
                };
                let detail = self.load_task_detail(self.active);
                return if matches!(action, Action::AssignTask(_)) {
                    Task::batch([detail, self.refresh_task_targets()])
                } else {
                    detail
                };
            }
            Action::Back => {
                if !panel.unresolved() {
                    *panel = Panel::default();
                }
            }
            Action::Refresh => {
                if panel.pending.is_none() {
                    return self.load_task_detail(self.active);
                }
            }
            Action::Edit if panel.ready() => {
                let task = panel.task.as_ref().unwrap();
                panel.title = task["title"].as_str().unwrap_or_default().into();
                panel.description = text_editor::Content::with_text(
                    task["description"].as_str().unwrap_or_default(),
                );
                panel.specification = text_editor::Content::with_text(
                    task["specification"]
                        .as_str()
                        .or(task["details"].as_str())
                        .unwrap_or_default(),
                );
                panel.mode = Mode::Edit;
            }
            Action::Cancel if !panel.unresolved() => panel.mode = Mode::View,
            Action::Title(title) if panel.ready() && panel.mode == Mode::Edit => {
                if title.len() <= 1024 {
                    panel.title = title;
                }
            }
            Action::Description(action) if panel.ready() && panel.mode == Mode::Edit => {
                edit_text(&mut panel.description, action)
            }
            Action::Specification(action) if panel.ready() && panel.mode == Mode::Edit => {
                edit_text(&mut panel.specification, action)
            }
            Action::Comment(action) if panel.ready() => edit_text(&mut panel.comment, action),
            Action::Target(choice @ Choice::Session(..)) if panel.ready() => {
                panel.recipient = Some(choice)
            }
            Action::ShowAgents if panel.ready() => {
                panel.mode = Mode::Assign;
                return self.refresh_task_targets();
            }
            Action::Save
            | Action::PostComment
            | Action::Assign
            | Action::Close
            | Action::Resume => {
                let change = match action {
                    Action::Save => Change::Edit,
                    Action::PostComment => Change::Comment,
                    Action::Assign => Change::Assign,
                    Action::Resume => Change::Resume,
                    _ => Change::Close,
                };
                return self.change_task(change);
            }
            _ => {}
        }
        Task::none()
    }

    fn load_task_detail(&mut self, index: usize) -> Task<Message> {
        let scope = self.scope(index);
        let panel = &mut self.workspaces[index].gateway.detail;
        let Some(id) = panel.selected else {
            return Task::none();
        };
        if panel.loading.is_some() || panel.pending.is_some() || scope.project.is_empty() {
            return Task::none();
        }
        let gateway = match self.preferences.current.gateway() {
            Ok(gateway) => gateway,
            Err(error) => {
                panel.status = error.to_string();
                return Task::none();
            }
        };
        let request = Uuid::new_v4();
        panel.loading = Some(request);
        Task::perform(
            async move {
                let result = gateway
                    .request(Method::GET, &scope.project, &[&id.to_string()], None, None)
                    .await
                    .map_err(|e| e.to_string());
                (scope, id, request, result)
            },
            |(scope, id, request, result)| Message::TaskDetailLoaded(scope, id, request, result),
        )
    }

    pub(super) fn task_detail_loaded(
        &mut self,
        scope: QueryScope,
        id: Uuid,
        request: Uuid,
        result: Result<Value, String>,
    ) {
        if !self.scope_current(&scope) {
            return;
        }
        let panel = &mut self.workspaces[scope.workspace].gateway.detail;
        if panel.selected != Some(id) || panel.loading != Some(request) {
            return;
        }
        panel.loading = None;
        match result {
            Ok(task) => {
                panel.task = Some(task);
                if panel.uncertain {
                    panel.status = "Task refreshed. Inspect its fields, owner and comments before repeating the operation.".into();
                }
                panel.uncertain = false;
            }
            Err(error) => panel.status = format!("Could not refresh task: {error}"),
        }
    }

    fn change_task(&mut self, change: Change) -> Task<Message> {
        let scope = self.scope(self.active);
        let panel = &self.workspaces[self.active].gateway.detail;
        let Some(id) = panel.selected else {
            return Task::none();
        };
        if !panel.ready() || scope.project.is_empty() {
            return Task::none();
        }
        let mutation = match change {
            Change::Edit if panel.mode == Mode::Edit => Mutation::Edit(TaskEdits {
                title: panel.title.clone(),
                description: panel.description.text(),
                specification: panel.specification.text(),
            }),
            Change::Comment => Mutation::Comment(panel.comment.text()),
            Change::Assign
                if panel.mode == Mode::Assign
                    && matches!(
                        panel.task.as_ref().unwrap()["status"].as_str(),
                        Some("todo" | "in_progress")
                    ) =>
            {
                let Some(Choice::Session(target, _)) = &panel.recipient else {
                    return Task::none();
                };
                if !self.workspaces[self.active]
                    .gateway
                    .routing
                    .available(target)
                {
                    self.workspaces[self.active].gateway.detail.status = "Selected agent is unavailable. Refresh agents and select a live execution.".into();
                    return Task::none();
                }
                if !amux::task_platform::matches(
                    panel.task.as_ref().unwrap(),
                    Some(&target.origin.os),
                ) {
                    self.workspaces[self.active].gateway.detail.status =
                        "Selected agent does not match this task's OS requirement.".into();
                    return Task::none();
                }
                Mutation::Assign(target.clone())
            }
            Change::Close if panel.task.as_ref().unwrap()["status"] != "done" => Mutation::Close,
            Change::Resume if panel.task.as_ref().unwrap()["status"] == "user_review" => {
                Mutation::Resume
            }
            _ => return Task::none(),
        };
        let validation = match &mutation {
            Mutation::Edit(edits) => edits.validate(),
            Mutation::Comment(content) => {
                if content.trim().is_empty() || content.len() > 64 * 1024 {
                    Err(anyhow::anyhow!("Enter a comment of at most 64 KiB"))
                } else {
                    Ok(())
                }
            }
            _ => Ok(()),
        };
        let gateway = validation.and_then(|_| self.preferences.current.gateway());
        let gateway = match gateway {
            Ok(gateway) => gateway,
            Err(error) => {
                self.workspaces[self.active].gateway.detail.status = error.to_string();
                return Task::none();
            }
        };
        let request = Uuid::new_v4();
        let panel = &mut self.workspaces[self.active].gateway.detail;
        panel.pending = Some(request);
        panel.status = "Saving…".into();
        Task::perform(
            async move {
                // Resolve the workspace repository again before applying a user mutation.
                let result = async {
                    anyhow::ensure!(
                        gateway.resolve_project(scope.directory.clone()).await? == scope.project,
                        "Workspace repository changed; refresh the task before continuing"
                    );
                    match mutation {
                        Mutation::Edit(edits) => {
                            gateway.edit_desktop_task(&scope.project, id, &edits).await
                        }
                        Mutation::Comment(content) => {
                            gateway.comment_on_task(&scope.project, id, &content).await
                        }
                        Mutation::Assign(target) => {
                            gateway
                                .assign_desktop_task(&scope.project, id, &target)
                                .await
                        }
                        Mutation::Close => gateway.close_desktop_task(&scope.project, id).await,
                        Mutation::Resume => gateway.resume_desktop_task(&scope.project, id).await,
                    }
                }
                .await
                .map_err(|e| e.to_string());
                (scope, id, request, change, result)
            },
            |(scope, id, request, change, result)| {
                Message::TaskChanged(scope, id, request, change, result)
            },
        )
    }

    pub(super) fn task_changed(
        &mut self,
        scope: QueryScope,
        id: Uuid,
        request: Uuid,
        change: Change,
        result: Result<Value, String>,
    ) -> Task<Message> {
        if !self.scope_current(&scope) {
            return Task::none();
        }
        let panel = &mut self.workspaces[scope.workspace].gateway.detail;
        if panel.selected != Some(id) || panel.pending != Some(request) {
            return Task::none();
        }
        panel.pending = None;
        match result {
            Ok(mut value) => {
                if matches!(change, Change::Comment) {
                    panel.comment = text_editor::Content::default();
                } else {
                    if value.get("comments").is_none()
                        && let Some(comments) =
                            panel.task.as_ref().and_then(|task| task.get("comments"))
                    {
                        value["comments"] = comments.clone();
                    }
                    panel.task = Some(value);
                }
                panel.mode = Mode::View;
                panel.status = match change {
                    Change::Edit => "Task saved",
                    Change::Comment => "Comment posted",
                    Change::Assign => "Task assigned",
                    Change::Close => "Task closed",
                    Change::Resume => "Task returned to queue",
                }
                .into();
                Task::batch([
                    self.load_task_detail(scope.workspace),
                    self.query(scope.workspace),
                ])
            }
            Err(error) => {
                panel.uncertain = true;
                panel.status = format!(
                    "Result not confirmed: {error}. Refresh and inspect the task before trying again."
                );
                Task::none()
            }
        }
    }

    pub(super) fn task_detail_view(&self) -> Element<'_, Message> {
        let panel = &self.workspaces[self.active].gateway.detail;
        let ready = panel.ready();
        let control = |label: &'static str, action, enabled: bool| {
            button(text(label).size(11)).on_press_maybe(enabled.then(|| message(action)))
        };
        let mut body = column![
            row![
                control("‹ Go back", Action::Back, !panel.unresolved()),
                control(
                    "Refresh",
                    Action::Refresh,
                    panel.loading.is_none() && panel.pending.is_none()
                )
            ]
            .spacing(6),
            text(&panel.status).size(11),
        ]
        .spacing(10);
        let Some(task) = &panel.task else {
            return body
                .push(text(if panel.loading.is_some() {
                    "Loading task…"
                } else {
                    "Task unavailable"
                }))
                .into();
        };
        body = body
            .push(text(task["title"].as_str().unwrap_or("Untitled")).size(15))
            .push(
                text(format!(
                    "{} · {}",
                    if task["status"] == "user_review" {
                        "User review"
                    } else {
                        task["status"].as_str().unwrap_or("unknown")
                    },
                    self.design_task_owner(task)
                ))
                .size(11),
            )
            .push(text(format!("#{}", panel.selected.unwrap())).size(10));
        match amux::task_platform::required_os(task) {
            Ok(Some(os)) => body = body.push(text(format!("Requires {}", os.as_str())).size(11)),
            Err(_) => {
                body = body.push(
                    text("Invalid OS requirement; correct the task labels before assignment.")
                        .size(11),
                )
            }
            _ => {}
        }
        if panel.mode == Mode::Edit {
            body = body
                .push(text_input("Title", &panel.title).on_input(|s| message(Action::Title(s))))
                .push(text("Description").size(11))
                .push(
                    text_editor(&panel.description)
                        .height(95)
                        .on_action(|a| message(Action::Description(a))),
                )
                .push(text("Specification / handoff").size(11))
                .push(
                    text_editor(&panel.specification)
                        .height(140)
                        .on_action(|a| message(Action::Specification(a))),
                )
                .push(
                    row![
                        control("Save", Action::Save, ready),
                        control("Cancel", Action::Cancel, !panel.unresolved())
                    ]
                    .spacing(6),
                );
        } else if panel.mode == Mode::Assign {
            let choices: Vec<_> = self.task_agent_choices().into_iter().filter(|choice| matches!(choice, Choice::Session(target, _) if amux::task_platform::matches(task, Some(&target.origin.os)))).collect();
            let current = panel.recipient.clone();
            let available = current.as_ref().is_some_and(|c| matches!(c, Choice::Session(target, _) if self.workspaces[self.active].gateway.routing.available(target) && amux::task_platform::matches(task, Some(&target.origin.os))));
            body = body
                .push(text("Assign to agent").size(13))
                .push(text("Local and gateway agents for this repository:").size(11))
                .push(text(&self.workspaces[self.active].gateway.routing.status).size(10));
            for choice in &choices {
                let selected = current.as_ref() == Some(choice);
                body = body.push(
                    button(text(choice.to_string()).size(11))
                        .width(Length::Fill)
                        .style(move |theme, status| {
                            if selected {
                                button::primary(theme, status)
                            } else {
                                button::secondary(theme, status)
                            }
                        })
                        .on_press_maybe(ready.then(|| message(Action::Target(choice.clone())))),
                );
            }
            if choices.is_empty() {
                body = body.push(text("No live agents available for this repository.").size(11));
            }
            if let Some(choice) = current.filter(|_| !available) {
                body = body.push(
                    text(format!(
                        "Unavailable selection: {choice}. Refresh and select a live agent."
                    ))
                    .size(11),
                );
            }
            body = body
                .push(
                    row![
                        control("Assign", Action::Assign, ready && available),
                        control("Refresh agents", Action::ShowAgents, ready)
                    ]
                    .spacing(5),
                )
                .push(control("Cancel", Action::Cancel, !panel.unresolved()))
                .push(
                    text("Assignments expire through the gateway's inactivity timeout.").size(10),
                );
        } else {
            if task["status"] == "user_review" {
                body = body
                    .push(
                        text("Waiting for your decision. Agents will not pick up this task.")
                            .size(11),
                    )
                    .push(control("Return to queue", Action::Resume, ready));
            }
            body = body.push(
                row![
                    control("Edit", Action::Edit, ready),
                    control(
                        "Assign",
                        Action::ShowAgents,
                        ready && matches!(task["status"].as_str(), Some("todo" | "in_progress"))
                    ),
                    control(
                        "Close task",
                        Action::Close,
                        ready && task["status"] != "done"
                    )
                ]
                .spacing(5),
            );
            for (label, field) in [
                ("Description", "description"),
                ("Specification", "specification"),
            ] {
                let content = task[field]
                    .as_str()
                    .or_else(|| {
                        (field == "specification")
                            .then(|| task["details"].as_str())
                            .flatten()
                    })
                    .unwrap_or_default();
                if !content.is_empty() {
                    body = body.push(text(label).size(11)).push(text(content).size(12));
                }
            }
        }
        body = body.push(text("Comments").size(13));
        let comments = task["comments"].as_array();
        if comments.is_none_or(|c| c.is_empty()) {
            body = body.push(text("No comments yet.").size(11));
        }
        if let Some(comments) = comments {
            for comment in comments {
                body = body
                    .push(
                        text(format!(
                            "{} · {}",
                            comment["author"].as_str().unwrap_or("unknown"),
                            comment["author_type"].as_str().unwrap_or("")
                        ))
                        .size(10),
                    )
                    .push(text(comment["content"].as_str().unwrap_or_default()).size(12));
            }
        }
        body.push(
            text_editor(&panel.comment)
                .placeholder("Add a comment…")
                .height(100)
                .on_action(|a| message(Action::Comment(a))),
        )
        .push(control(
            "Post comment",
            Action::PostComment,
            ready && !panel.comment.text().trim().is_empty(),
        ))
        .into()
    }
}

fn edit_text(content: &mut text_editor::Content, action: text_editor::Action) {
    let before = content.text();
    content.perform(action);
    if content.text().len() > 64 * 1024 {
        *content = text_editor::Content::with_text(&before);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn app() -> Desktop {
        let mut app = super::super::tests::desktop();
        app.preferences.current.url = "http://127.0.0.1:9".into();
        app.preferences.current.api_key = "fixture-only".into();
        app
    }
    fn task(id: Uuid, project: &str) -> Value {
        json!({"id":id,"project_ident":project,"status":"todo","title":"Original", "description":"Description", "specification":"Handoff", "comments":[]})
    }
    fn open(app: &mut Desktop, id: Uuid) {
        let _ = app.manage_task(Action::Open(id));
        let request = app.workspaces[app.active].gateway.detail.loading.unwrap();
        let scope = app.scope(app.active);
        let project = scope.project.clone();
        app.task_detail_loaded(scope, id, request, Ok(task(id, &project)));
    }

    #[test]
    fn task_loads_and_mutations_stay_with_their_workspace_and_request() {
        let mut app = app();
        let id = Uuid::new_v4();
        open(&mut app, id);
        let _ = app.manage_task(Action::Edit);
        let _ = app.manage_task(Action::Title("Edited".into()));
        let _ = app.manage_task(Action::Save);
        let pending = app.workspaces[0].gateway.detail.pending.unwrap();
        let _ = app.manage_task(Action::Save);
        assert_eq!(
            app.workspaces[0].gateway.detail.pending,
            Some(pending),
            "double click must not submit twice"
        );
        let scope = app.scope(0);
        app.active = 1;
        let other = Uuid::new_v4();
        open(&mut app, other);
        let _ = app.task_changed(scope, id, pending, Change::Edit, Ok(task(id, "project-0")));
        assert_eq!(app.workspaces[1].gateway.detail.selected, Some(other));
        assert_eq!(
            app.workspaces[0].gateway.detail.task.as_ref().unwrap()["id"],
            id.to_string()
        );
    }

    #[test]
    fn uncertain_comments_keep_the_draft_and_block_repeats_until_refreshed() {
        let mut app = app();
        let id = Uuid::new_v4();
        open(&mut app, id);
        app.workspaces[0].gateway.detail.comment =
            text_editor::Content::with_text("Keep this draft");
        let draft = app.workspaces[0].gateway.detail.comment.text();
        let _ = app.manage_task(Action::PostComment);
        let pending = app.workspaces[0].gateway.detail.pending.unwrap();
        let scope = app.scope(0);
        let _ = app.task_changed(
            scope.clone(),
            id,
            pending,
            Change::Comment,
            Err("connection lost".into()),
        );
        assert!(app.workspaces[0].gateway.detail.unresolved());
        let _ = app.manage_task(Action::PostComment);
        assert!(app.workspaces[0].gateway.detail.pending.is_none());
        assert_eq!(app.workspaces[0].gateway.detail.comment.text(), draft);
        let workspace = app.workspaces[0].id;
        let _ = app.remove_workspace(workspace);
        assert_eq!(
            app.workspaces[0].id, workspace,
            "keep an unresolved operation visible"
        );
        let _ = app.manage_task(Action::Refresh);
        let reload = app.workspaces[0].gateway.detail.loading.unwrap();
        app.task_detail_loaded(scope, id, reload, Ok(task(id, "project-0")));
        assert!(!app.workspaces[0].gateway.detail.unresolved());
        assert_eq!(app.workspaces[0].gateway.detail.comment.text(), draft);
    }

    #[test]
    fn stale_loads_cannot_overwrite_another_task_or_a_rebound_project() {
        let mut app = app();
        let old = Uuid::new_v4();
        let _ = app.manage_task(Action::Open(old));
        let scope = app.scope(0);
        let request = app.workspaces[0].gateway.detail.loading.unwrap();
        let next = Uuid::new_v4();
        open(&mut app, next);
        app.task_detail_loaded(scope, old, request, Ok(task(old, "project-0")));
        assert_eq!(
            app.workspaces[0].gateway.detail.task.as_ref().unwrap()["id"],
            next.to_string()
        );
        let scope = app.scope(0);
        app.bind_project(0, "other-project".into());
        app.task_detail_loaded(scope, next, request, Ok(task(next, "project-0")));
        assert!(app.workspaces[0].gateway.detail.task.is_none());
    }

    #[test]
    fn assignment_lists_machine_names_and_refuses_expired_or_replaced_executions() {
        use amux::{
            delegation::Origin,
            task_routing::{Available, Directory, SessionTarget},
        };
        let mut app = app();
        let id = Uuid::new_v4();
        open(&mut app, id);
        let target = SessionTarget {
            origin: Origin {
                session_id: Uuid::new_v4(),
                instance_id: Uuid::new_v4(),
                provider: Provider::Codex,
                os: "linux".into(),
            },
            generation: Uuid::new_v4(),
        };
        app.workspaces[0].gateway.routing.directory = Some(Directory {
            canonical_remote: Some("github.com/nitecon/amux".into()),
            targets: vec![Available {
                target: target.clone(),
                seen_at: unix_ms() as i64,
                expires_at: unix_ms() as i64 + 120_000,
                display_name: Some("Ada Lovelace - Codex".into()),
                hostname: Some("Defcon2".into()),
            }],
        });
        app.workspaces[0].gateway.routing.checked = Some(Instant::now());
        let choices = app.task_agent_choices();
        assert_eq!(choices.len(), 1);
        assert!(choices[0].to_string().contains("Ada Lovelace"));
        assert!(choices[0].to_string().contains("Defcon2"));
        app.workspaces[0].gateway.detail.mode = Mode::Assign;
        app.workspaces[0].gateway.detail.recipient = Some(choices[0].clone());
        app.workspaces[0]
            .gateway
            .routing
            .directory
            .as_mut()
            .unwrap()
            .targets[0]
            .target
            .generation = Uuid::new_v4();
        let _ = app.manage_task(Action::Assign);
        assert!(app.workspaces[0].gateway.detail.pending.is_none());
        app.workspaces[0].gateway.routing.checked =
            Instant::now().checked_sub(Duration::from_secs(61));
        assert!(app.task_agent_choices().is_empty());
        assert!(!app.workspaces[0].gateway.routing.available(&target));
    }

    #[test]
    fn detail_callbacks_follow_workspace_identity_after_peer_removal() {
        let mut app = app();
        app.active = 1;
        let id = Uuid::new_v4();
        let _ = app.manage_task(Action::Open(id));
        let scope = app.scope(1);
        let request = app.workspaces[1].gateway.detail.loading.unwrap();
        let _ = app.remove_workspace(app.workspaces[0].id);
        let callback = Message::TaskDetailLoaded(scope, id, request, Ok(task(id, "project-1")));
        let _ = app.update(callback);
        assert_eq!(
            app.workspaces[0].gateway.detail.task.as_ref().unwrap()["id"],
            id.to_string()
        );
    }
}
