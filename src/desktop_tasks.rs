//! Existing-task actions stay in the gateway sidebar and are attributed to the user.
use super::*;
use amux::task_management::TaskEdits;
use desktop_routing::Choice;
use iced::widget::column;
use reqwest::Method;
use serde_json::Value;

#[derive(Debug, Clone)]
pub(super) enum Action {
    AnimateDrawer,
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
    ShowWatcher,
    Target(Choice),
    Save,
    PostComment,
    Assign,
    Watch,
    ClearWatcher,
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
    Watch,
    ClearWatcher,
}

#[derive(Default, PartialEq)]
enum Mode {
    #[default]
    View,
    Edit,
    Assign,
    Watch,
}

#[derive(Default)]
pub(super) struct Panel {
    pub selected: Option<Uuid>,
    opened_at: Option<std::time::Instant>,
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
    pub(super) fn drawer_progress(&self) -> f32 {
        self.opened_at.map_or(1.0, |opened| {
            let elapsed = (opened.elapsed().as_secs_f32() / 0.18).min(1.0);
            1.0 - (1.0 - elapsed).powi(3)
        })
    }
    pub(super) fn animating(&self) -> bool {
        self.selected.is_some() && self.drawer_progress() < 1.0
    }
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
    /// A user comment on parked work also returns it to its last live agent,
    /// or to the queue when that agent is gone.
    Reply(String, Vec<amux::task_routing::SessionTarget>),
    Assign(amux::task_routing::SessionTarget),
    Close,
    Resume,
    Requester(Option<amux::task_routing::SessionTarget>),
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
                    opened_at: Some(std::time::Instant::now()),
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
            // A read-only refresh must not interrupt the local draft. Keep
            // mutations gated by ready(), and freeze an unresolved submission.
            Action::Comment(action) if panel.task.is_some() && !panel.unresolved() => {
                edit_text(&mut panel.comment, action)
            }
            Action::Target(choice @ Choice::Session(..)) if panel.ready() => {
                panel.recipient = Some(choice)
            }
            Action::ShowAgents if panel.ready() => {
                panel.mode = Mode::Assign;
                return self.refresh_task_targets();
            }
            Action::ShowWatcher if panel.ready() => {
                panel.mode = Mode::Watch;
                panel.recipient = None;
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
            Action::Watch if panel.ready() => return self.change_task(Change::Watch),
            Action::ClearWatcher if panel.ready() => return self.change_task(Change::ClearWatcher),
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
            Change::Watch if panel.mode == Mode::Watch => {
                let Some(Choice::Session(target, _)) = &panel.recipient else {
                    return Task::none();
                };
                if !self.workspaces[self.active]
                    .gateway
                    .routing
                    .available(target)
                    || !amux::task_platform::matches(
                        panel.task.as_ref().unwrap(),
                        Some(&target.origin.os),
                    )
                {
                    self.workspaces[self.active].gateway.detail.status =
                        "Result recipient is unavailable or incompatible; refresh agents.".into();
                    return Task::none();
                }
                Mutation::Requester(Some(target.clone()))
            }
            Change::ClearWatcher => Mutation::Requester(None),
            Change::Edit if panel.mode == Mode::Edit => Mutation::Edit(TaskEdits {
                title: panel.title.clone(),
                description: panel.description.text(),
                specification: panel.specification.text(),
            }),
            Change::Comment if panel.task.as_ref().unwrap()["status"] == "user_review" => {
                let routing = &self.workspaces[self.active].gateway.routing;
                let live = routing
                    .directory
                    .as_ref()
                    .map(|d| d.targets.iter().map(|e| e.target.clone()))
                    .into_iter()
                    .flatten()
                    .filter(|target| routing.available(target))
                    .collect();
                Mutation::Reply(panel.comment.text(), live)
            }
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
            Mutation::Comment(content) | Mutation::Reply(content, _) => {
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
                        Mutation::Reply(content, live) => {
                            gateway
                                .comment_on_task(&scope.project, id, &content)
                                .await?;
                            let task = gateway.resume_desktop_task(&scope.project, id).await?;
                            let last = gateway.last_task_owner(&scope.project, id).await?;
                            match live.into_iter().find(|target| {
                                Some(&target.origin) == last.as_ref()
                                    && amux::task_platform::matches(&task, Some(&target.origin.os))
                            }) {
                                Some(target) => {
                                    gateway
                                        .assign_desktop_task(&scope.project, id, &target)
                                        .await
                                }
                                None => Ok(task),
                            }
                        }
                        Mutation::Assign(target) => {
                            gateway
                                .assign_desktop_task(&scope.project, id, &target)
                                .await
                        }
                        Mutation::Close => gateway.close_desktop_task(&scope.project, id).await,
                        Mutation::Resume => gateway.resume_desktop_task(&scope.project, id).await,
                        Mutation::Requester(target) => {
                            gateway
                                .set_task_requester(&scope.project, id, target.as_ref(), None)
                                .await
                        }
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
                // A reply to parked work returns the task, not the comment.
                let returned = value["status"].as_str().map(|status| status == "todo");
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
                // Posting a comment does not save or cancel metadata drafts.
                if !matches!(change, Change::Comment) || panel.mode != Mode::Edit {
                    panel.mode = Mode::View;
                }
                panel.status = match change {
                    Change::Edit => "Task saved",
                    Change::Comment if returned == Some(true) => {
                        "Comment posted; task returned to the queue (its last agent is offline)"
                    }
                    Change::Comment if returned == Some(false) => {
                        "Comment posted; task returned to its last agent"
                    }
                    Change::Comment => "Comment posted",
                    Change::Assign => "Task assigned",
                    Change::Close => "Task closed",
                    Change::Resume => "Task returned to queue",
                    Change::Watch => "Result recipient saved",
                    Change::ClearWatcher => "Result recipient cleared",
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
            button(text(label).size(13)).on_press_maybe(enabled.then(|| message(action)))
        };
        // Navigation and task actions stay at the top and the comment box at
        // the bottom; only the details and comments between them scroll.
        let mut header = column![
            row![
                control("‹ Go back", Action::Back, !panel.unresolved()),
                control(
                    "Refresh",
                    Action::Refresh,
                    panel.loading.is_none() && panel.pending.is_none()
                )
            ]
            .spacing(6),
            text(&panel.status).size(13),
        ]
        .spacing(10);
        let Some(task) = &panel.task else {
            return header
                .push(text(if panel.loading.is_some() {
                    "Loading task…"
                } else {
                    "Task unavailable"
                }))
                .into();
        };
        let mut body = column![].spacing(14).width(Length::Fill);
        body = body
            .push(
                text(task["title"].as_str().unwrap_or("Untitled"))
                    .size(17)
                    .width(Length::Fill)
                    .wrapping(iced::widget::text::Wrapping::WordOrGlyph),
            )
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
                .size(13)
                .width(Length::Fill)
                .wrapping(iced::widget::text::Wrapping::WordOrGlyph),
            )
            .push(
                text(format!("#{}", panel.selected.unwrap()))
                    .size(12)
                    .width(Length::Fill)
                    .wrapping(iced::widget::text::Wrapping::WordOrGlyph),
            );
        for (label, field) in [
            ("Created", "created_at"),
            ("Updated", "updated_at"),
            ("Completed", "done_at"),
        ] {
            if !task[field].is_null() {
                body = body.push(
                    text(format!("{label}: {}", task_time(&task[field])))
                        .size(12)
                        .width(Length::Fill),
                );
            }
        }
        match amux::task_platform::required_os(task) {
            Ok(Some(os)) => body = body.push(text(format!("Requires {}", os.as_str())).size(13)),
            Err(_) => {
                body = body.push(
                    text("Invalid OS requirement; correct the task labels before assignment.")
                        .size(13),
                )
            }
            _ => {}
        }
        if panel.mode == Mode::Edit {
            body = body
                .push(
                    text_input("Title", &panel.title)
                        .size(15)
                        .on_input(|s| message(Action::Title(s))),
                )
                .push(text("Description").size(13))
                .push(
                    text_editor(&panel.description)
                        .size(15)
                        .wrapping(iced::widget::text::Wrapping::WordOrGlyph)
                        .height(95)
                        .on_action(|a| message(Action::Description(a))),
                )
                .push(text("Specification / handoff").size(13))
                .push(
                    text_editor(&panel.specification)
                        .size(15)
                        .wrapping(iced::widget::text::Wrapping::WordOrGlyph)
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
        } else if matches!(panel.mode, Mode::Assign | Mode::Watch) {
            let watching = panel.mode == Mode::Watch;
            let choices: Vec<_> = self.task_agent_choices().into_iter().filter(|choice| matches!(choice, Choice::Session(target, _) if amux::task_platform::matches(task, Some(&target.origin.os)))).collect();
            let current = panel.recipient.clone();
            let available = current.as_ref().is_some_and(|c| matches!(c, Choice::Session(target, _) if self.workspaces[self.active].gateway.routing.available(target) && amux::task_platform::matches(task, Some(&target.origin.os))));
            body = body
                .push(
                    text(if watching {
                        "Send results to agent"
                    } else {
                        "Assign to agent"
                    })
                    .size(15),
                )
                .push(text("Local and gateway agents for this repository:").size(13))
                .push(text(&self.workspaces[self.active].gateway.routing.status).size(12));
            for choice in &choices {
                let selected = current.as_ref() == Some(choice);
                body = body.push(
                    button(
                        text(choice.to_string())
                            .size(13)
                            .wrapping(iced::widget::text::Wrapping::WordOrGlyph),
                    )
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
                body = body.push(text("No live agents available for this repository.").size(13));
            }
            if let Some(choice) = current.filter(|_| !available) {
                body = body.push(
                    text(format!(
                        "Unavailable selection: {choice}. Refresh and select a live agent."
                    ))
                    .size(13),
                );
            }
            body = body
                .push(
                    row![
                        control(
                            if watching {
                                "Save result recipient"
                            } else {
                                "Assign"
                            },
                            if watching {
                                Action::Watch
                            } else {
                                Action::Assign
                            },
                            ready && available
                        ),
                        control(
                            "Refresh agents",
                            if watching {
                                Action::ShowWatcher
                            } else {
                                Action::ShowAgents
                            },
                            ready
                        )
                    ]
                    .spacing(5),
                )
                .push(control("Cancel", Action::Cancel, !panel.unresolved()))
                .push(
                    text(if watching {
                        "Comment and completion notices only; task ownership is unchanged."
                    } else {
                        "Assignments expire through the gateway's inactivity timeout."
                    })
                    .size(12),
                );
        } else {
            if task["status"] == "user_review" {
                body = body
                    .push(
                        text("Waiting for your decision. Agents will not pick up this task.")
                            .size(13),
                    )
                    .push(control("Return to queue", Action::Resume, ready));
            }
            header = header.push(
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
            body = body.push(
                row![
                    control("Result recipient…", Action::ShowWatcher, ready),
                    control(
                        "Clear result recipient",
                        Action::ClearWatcher,
                        ready && !task["requester"].is_null()
                    ),
                ]
                .spacing(5),
            );
            if let Ok(target) = serde_json::from_value::<amux::task_routing::SessionTarget>(
                task["requester"].clone(),
            ) {
                body = body.push(
                    text(format!(
                        "Results to: {} · {} · {}",
                        target.origin.provider.command(),
                        target.origin.os,
                        target.origin.session_id
                    ))
                    .size(12),
                );
            }
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
                    body = body.push(text(label).size(13)).push(
                        text(content)
                            .size(14)
                            .width(Length::Fill)
                            .wrapping(iced::widget::text::Wrapping::WordOrGlyph),
                    );
                }
            }
        }
        body = body.push(text("Comments").size(15));
        let comments = task["comments"].as_array();
        if comments.is_none_or(|c| c.is_empty()) {
            body = body.push(text("No comments yet.").size(13));
        }
        if let Some(comments) = comments {
            for comment in comments {
                body = body.push(comment_bubble(comment));
            }
        }
        let footer = column![
            text_editor(&panel.comment)
                .size(15)
                .wrapping(iced::widget::text::Wrapping::WordOrGlyph)
                .placeholder("Add a comment…")
                .height(100)
                .on_action(|a| message(Action::Comment(a))),
            control(
                "Post comment",
                Action::PostComment,
                ready && !panel.comment.text().trim().is_empty(),
            )
        ]
        .spacing(10);
        column![
            header,
            scrollable(container(body).padding(iced::Padding {
                right: 12.0,
                ..Default::default()
            }))
            .width(Length::Fill)
            .height(Length::Fill),
            footer
        ]
        .spacing(10)
        .into()
    }
}

/// Store Unix milliseconds unchanged; display the client's local date/time with
/// the UTC offset that applied at that instant, including daylight saving time.
fn task_time(value: &Value) -> String {
    value
        .as_i64()
        .filter(|millis| (0..=253_402_300_799_999).contains(millis))
        .and_then(chrono::DateTime::from_timestamp_millis)
        .map(|date| {
            date.with_timezone(&chrono::Local)
                .format("%Y-%m-%d %H:%M:%S %:z")
                .to_string()
        })
        .unwrap_or_else(|| "Time unavailable".into())
}

fn comment_bubble(comment: &Value) -> Element<'_, Message> {
    use iced::{Border, Font, font};
    let wrap = iced::widget::text::Wrapping::WordOrGlyph;
    container(
        column![
            text(
                comment["author"]
                    .as_str()
                    .filter(|name| !name.trim().is_empty())
                    .unwrap_or("Unknown author")
            )
            .font(Font {
                weight: font::Weight::Bold,
                ..Font::DEFAULT
            })
            .size(14)
            .width(Length::Fill)
            .wrapping(wrap),
            text(format!(
                "{} · {} · {}",
                task_time(&comment["created_at"]),
                comment["author_type"].as_str().unwrap_or("unknown"),
                comment["hostname"]
                    .as_str()
                    .filter(|host| !host.trim().is_empty())
                    .unwrap_or("Machine not recorded")
            ))
            .size(12)
            .width(Length::Fill)
            .wrapping(wrap),
            text(comment["content"].as_str().unwrap_or_default())
                .size(14)
                .width(Length::Fill)
                .wrapping(wrap),
        ]
        .spacing(8)
        .width(Length::Fill),
    )
    .padding(14)
    .width(Length::Fill)
    .style(|theme: &Theme| {
        let palette = theme.extended_palette();
        container::Style {
            background: Some(palette.background.weak.color.into()),
            border: Border {
                color: palette.background.strong.color,
                width: 1.0,
                radius: 9.0.into(),
            },
            ..Default::default()
        }
    })
    .into()
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

    #[test]
    fn task_dates_preserve_invalid_timestamp_fallback() {
        assert_ne!(task_time(&json!(0)), "Time unavailable");
        for missing in [
            Value::Null,
            json!(-1),
            json!(u64::MAX),
            json!(253_402_300_800_000_i64),
        ] {
            assert_eq!(task_time(&missing), "Time unavailable");
        }
    }

    #[cfg(unix)]
    #[test]
    fn task_dates_follow_local_timezone_across_dst() {
        const FIXTURE: &str = "AMUX_TEST_TASK_TIME_ZONE";
        if let Ok(zone) = std::env::var(FIXTURE) {
            match zone.as_str() {
                "utc" => assert_eq!(task_time(&json!(1_000)), "1970-01-01 00:00:01 +00:00"),
                "new_york" => {
                    assert_eq!(task_time(&json!(1_000)), "1969-12-31 19:00:01 -05:00");
                    assert_eq!(
                        task_time(&json!(1_793_512_799_000_i64)),
                        "2026-11-01 01:59:59 -04:00"
                    );
                    assert_eq!(
                        task_time(&json!(1_793_512_800_000_i64)),
                        "2026-11-01 01:00:00 -05:00"
                    );
                }
                _ => panic!("unknown timezone fixture"),
            }
            return;
        }
        // Isolate TZ changes from other test threads and the timezone cache.
        // POSIX rules keep this independent of installed zoneinfo files.
        for (zone, tz) in [("utc", "UTC0"), ("new_york", "EST5EDT,M3.2.0,M11.1.0")] {
            let output = std::process::Command::new(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "desktop::desktop_tasks::tests::task_dates_follow_local_timezone_across_dst",
                    "--nocapture",
                ])
                .env(FIXTURE, zone)
                .env("TZ", tz)
                .output()
                .unwrap();
            assert!(
                output.status.success(),
                "{zone}: {}\n{}",
                String::from_utf8_lossy(&output.stdout),
                String::from_utf8_lossy(&output.stderr)
            );
        }
    }

    fn app() -> Desktop {
        // These tests drive synchronous UI callbacks; keep one immutable
        // native credential fixture alive for the test process.
        static PROFILE: std::sync::OnceLock<tempfile::TempDir> = std::sync::OnceLock::new();
        let mut app = super::super::tests::desktop();
        let directory = PROFILE.get_or_init(|| {
            let (directory, _) = super::super::tests::preferences_fixture();
            directory
        });
        let profile_path = amux_gateway_backend::security::local_profile_path(
            &directory.path().join("native-profile"),
        );
        let local = amux_gateway_backend::security::LocalProfile::load(&profile_path).unwrap();
        app.preferences.current.mode = GatewayMode::Client;
        app.preferences.current.enrolled = Some(EnrolledConnection {
            url: local.url,
            cluster_id: local.cluster_id,
            node_id: local.node_id,
            profile_path,
        });
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
    fn comment_typing_survives_delayed_manual_and_automatic_detail_refreshes() {
        for automatic in [false, true] {
            let mut app = app();
            let id = Uuid::new_v4();
            open(&mut app, id);
            let type_text = |app: &mut Desktop, text: &str| {
                for character in text.chars() {
                    let _ = app.manage_task(Action::Comment(text_editor::Action::Edit(
                        text_editor::Edit::Insert(character),
                    )));
                }
            };
            type_text(&mut app, "Before ");
            if automatic {
                assert!(app.workspaces[0].gateway.detail.refresh_with_list());
                let _ = app.refresh_open_task(0);
            } else {
                let _ = app.manage_task(Action::Refresh);
            }
            let request = app.workspaces[0].gateway.detail.loading.unwrap();
            type_text(&mut app, "during ");
            let _ = app.manage_task(Action::PostComment);
            assert!(app.workspaces[0].gateway.detail.pending.is_none());
            let scope = app.scope(0);
            let mut refreshed = task(id, &scope.project);
            refreshed["comments"] = json!([{"content": "Another user's comment"}]);
            app.task_detail_loaded(scope, id, request, Ok(refreshed));
            type_text(&mut app, "after");
            let panel = &app.workspaces[0].gateway.detail;
            assert_eq!(panel.comment.text().trim_end(), "Before during after");
            assert_eq!(
                panel.task.as_ref().unwrap()["comments"][0]["content"],
                "Another user's comment"
            );
            let _ = app.manage_task(Action::PostComment);
            let pending = app.workspaces[0].gateway.detail.pending.unwrap();
            type_text(&mut app, " blocked");
            assert_eq!(
                app.workspaces[0].gateway.detail.comment.text().trim_end(),
                "Before during after"
            );
            let scope = app.scope(0);
            let _ = app.task_changed(scope, id, pending, Change::Comment, Ok(json!({})));
            assert!(
                app.workspaces[0]
                    .gateway
                    .detail
                    .comment
                    .text()
                    .trim()
                    .is_empty()
            );
        }
    }

    #[test]
    fn posting_a_comment_keeps_metadata_drafts_until_explicit_save_or_cancel() {
        for save in [false, true] {
            let mut app = app();
            let id = Uuid::new_v4();
            open(&mut app, id);
            let _ = app.manage_task(Action::Edit);
            let _ = app.manage_task(Action::Title("Unsaved title".into()));
            let insert = || text_editor::Action::Edit(text_editor::Edit::Insert('X'));
            let _ = app.manage_task(Action::Description(insert()));
            let _ = app.manage_task(Action::Specification(insert()));
            let description = app.workspaces[0].gateway.detail.description.text();
            let specification = app.workspaces[0].gateway.detail.specification.text();
            let _ = app.manage_task(Action::Comment(insert()));
            let _ = app.manage_task(Action::PostComment);
            let pending = app.workspaces[0].gateway.detail.pending.unwrap();
            let scope = app.scope(0);
            let _ = app.task_changed(scope.clone(), id, pending, Change::Comment, Ok(json!({})));
            let reload = app.workspaces[0].gateway.detail.loading.unwrap();
            let project = scope.project.clone();
            app.task_detail_loaded(scope.clone(), id, reload, Ok(task(id, &project)));
            let panel = &app.workspaces[0].gateway.detail;
            assert!(panel.mode == Mode::Edit);
            assert_eq!(panel.title, "Unsaved title");
            assert_eq!(panel.description.text(), description);
            assert_eq!(panel.specification.text(), specification);
            assert!(panel.comment.text().trim().is_empty());
            assert_eq!(panel.task.as_ref().unwrap()["title"], "Original");

            if save {
                let _ = app.manage_task(Action::Save);
                let pending = app.workspaces[0].gateway.detail.pending.unwrap();
                let mut saved = task(id, &project);
                saved["title"] = json!("Unsaved title");
                saved["description"] = json!(description);
                saved["specification"] = json!(specification);
                let _ = app.task_changed(scope, id, pending, Change::Edit, Ok(saved));
                let panel = &app.workspaces[0].gateway.detail;
                assert!(panel.mode == Mode::View);
                assert_eq!(panel.task.as_ref().unwrap()["title"], "Unsaved title");
            } else {
                let _ = app.manage_task(Action::Cancel);
                assert!(app.workspaces[0].gateway.detail.mode == Mode::View);
                let _ = app.manage_task(Action::Edit);
                assert_eq!(app.workspaces[0].gateway.detail.title, "Original");
            }
        }
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
        // A watcher selection is fenced by the same exact advertised generation.
        app.workspaces[0].gateway.detail.mode = Mode::Watch;
        let _ = app.manage_task(Action::Watch);
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

    #[test]
    fn watcher_ack_updates_the_result_recipient_without_changing_task_ownership() {
        let mut app = app();
        let id = Uuid::new_v4();
        open(&mut app, id);
        assert!(app.workspaces[0].gateway.detail.task.as_ref().unwrap()["requester"].is_null());
        let owner = Uuid::new_v4();
        let requester = amux::task_routing::SessionTarget {
            origin: amux::delegation::Origin {
                session_id: Uuid::new_v4(),
                instance_id: Uuid::new_v4(),
                provider: Provider::Claude,
                os: "windows".into(),
            },
            generation: Uuid::new_v4(),
        };
        let mut value = task(id, "project-0");
        value["owner_agent_id"] = json!(owner);
        value["requester"] = json!(requester);
        let scope = app.scope(0);
        let request = Uuid::new_v4();
        app.workspaces[0].gateway.detail.pending = Some(request);
        let _ = app.task_changed(scope, id, request, Change::Watch, Ok(value));
        let stored = app.workspaces[0].gateway.detail.task.as_ref().unwrap();
        assert_eq!(stored["requester"], json!(requester));
        assert_eq!(stored["owner_agent_id"], json!(owner));
        assert!(app.workspaces[0].gateway.detail.pending.is_none());
    }

    #[test]
    fn replying_to_parked_work_reports_where_the_task_went() {
        let mut app = app();
        let id = Uuid::new_v4();
        open(&mut app, id);
        app.workspaces[0].gateway.detail.task.as_mut().unwrap()["status"] = json!("user_review");
        for (status, message) in [
            (
                "in_progress",
                "Comment posted; task returned to its last agent",
            ),
            (
                "todo",
                "Comment posted; task returned to the queue (its last agent is offline)",
            ),
        ] {
            app.workspaces[0].gateway.detail.comment = text_editor::Content::with_text("Go ahead");
            let _ = app.manage_task(Action::PostComment);
            let pending = app.workspaces[0].gateway.detail.pending.unwrap();
            let mut returned = task(id, "project-0");
            returned["status"] = json!(status);
            let scope = app.scope(0);
            let _ = app.task_changed(scope, id, pending, Change::Comment, Ok(returned));
            let panel = &app.workspaces[0].gateway.detail;
            assert_eq!(panel.status, message);
            assert!(panel.comment.text().trim().is_empty());
            app.workspaces[0].gateway.detail.loading = None;
        }
    }
}
