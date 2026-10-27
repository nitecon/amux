//! One current queued task per idle native agent; claims precede terminal input.
use super::super::QueryScope;
use super::*;
use amux::backlog;
use serde_json::Value;

#[derive(Debug, Clone)]
pub(in super::super) struct Pick {
    request: Uuid,
    task: Uuid,
    revision: i64,
    scope: QueryScope,
    candidate: Candidate,
    started: Instant,
    delivery: bool,
    /// Nudges the owner of in-progress work; never claims anything.
    reminder: bool,
    current_task: Option<Box<Value>>,
}

#[derive(Default)]
pub(super) struct State {
    pub(super) flight: Option<Pick>,
    pending: Vec<Pick>,
    last_assigned: HashMap<String, (u64, u64)>,
    pub status: String,
    diagnostic: Option<(String, Instant)>,
}

impl Desktop {
    pub(in super::super) fn queue_diagnostic(&mut self, message: String) {
        if self
            .delegation
            .backlog
            .diagnostic
            .as_ref()
            .is_some_and(|(last, at)| last == &message || at.elapsed() < Duration::from_secs(5))
        {
            return;
        }
        self.delegation.backlog.diagnostic = Some((message.clone(), Instant::now()));
        let Some(directory) = &self.registry else {
            return;
        };
        let path = directory.join("queue.log");
        if std::fs::metadata(&path).is_ok_and(|meta| meta.len() >= 1024 * 1024) {
            let previous = directory.join("queue.previous.log");
            let _ = std::fs::remove_file(&previous);
            if std::fs::rename(&path, previous).is_err() {
                return;
            }
        }
        let mut options = std::fs::OpenOptions::new();
        options.create(true).append(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        if let Ok(mut file) = options.open(path) {
            use std::io::Write;
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap_or_default()
                .as_millis();
            let _ = writeln!(file, "{now} {message}");
        }
    }

    fn backlog_scope(&self, pick: &Pick) -> Option<QueryScope> {
        let index = self
            .workspaces
            .iter()
            .position(|w| w.id == pick.scope.workspace_id)?;
        let current = self.scope(index);
        (current.project == pick.scope.project
            && current.directory == pick.scope.directory
            && current.revision == pick.scope.revision)
            .then_some(current)
    }

    fn backlog_ready(&mut self, pick: &Pick, idle_after: Duration) -> bool {
        if self.backlog_scope(pick).is_none()
            || !self
                .delegation
                .candidates
                .iter()
                .any(|c| c.target == pick.candidate.target)
        {
            return false;
        }
        let target = &pick.candidate.target;
        // Ten minutes at the default idle threshold, so a turn waiting on a human
        // answer is not nudged at once.
        let idle_after = if pick.reminder {
            idle_after * 20
        } else {
            idle_after
        };
        if !pick.reminder
            && self
                .workspaces
                .iter()
                .filter(|w| w.gateway.project == target.project)
                .flat_map(|w| &w.gateway.tasks)
                .any(|task| {
                    task["status"] == "in_progress"
                        && backlog::owns(task, target)
                        && (!pick.delivery || task["id"] != pick.task.to_string())
                })
        {
            return false;
        }
        tab(self, target.terminal).is_some_and(|tab| {
            tab.delegation.input.is_empty()
                && tab.terminal.as_ref().is_some_and(|terminal| {
                    terminal
                        .activity_elapsed()
                        .is_none_or(|elapsed| elapsed >= idle_after)
                        && foreground(terminal) == pick.candidate.root.foreground
                        && tab.delegation.root.as_ref()
                            == Some(&(
                                pick.candidate.root.pid,
                                pick.candidate.root.generation.clone(),
                            ))
                })
        })
    }

    fn verify_backlog(&mut self, mut pick: Pick) -> Task<Message> {
        pick.started = Instant::now();
        pick.request = Uuid::new_v4();
        self.delegation.backlog.flight = Some(pick.clone());
        let epoch = self.delegation.epoch;
        let gateway = self.preferences.current.gateway().ok();
        Task::perform(
            async move {
                if pick.delivery
                    && let Some(gateway) = gateway
                {
                    pick.current_task = gateway
                        .request(
                            reqwest::Method::GET,
                            &pick.candidate.target.project,
                            &[&pick.task.to_string()],
                            None,
                            None,
                        )
                        .await
                        .ok()
                        .map(Box::new);
                }
                pick.started = Instant::now();
                let c = pick.candidate.clone();
                let valid = (!pick.delivery || pick.current_task.is_some())
                    && tokio::task::spawn_blocking(move || {
                        delegation_process::verify(
                            c.root.pid,
                            &c.root.generation,
                            c.root.foreground,
                            &c.process,
                        )
                    })
                    .await
                    .unwrap_or(false);
                Event::BacklogVerified(Box::new(pick), valid)
            },
            move |event| Message::Delegation(epoch, event),
        )
    }

    pub(crate) fn backlog_tick(&mut self) -> Task<Message> {
        self.backlog_tick_after(amux::session::IDLE_AFTER)
    }

    pub(super) fn backlog_tick_after(&mut self, idle_after: Duration) -> Task<Message> {
        if self.server_schedules() {
            return Task::none();
        }
        let blocked = if !self.preferences.current.automatic_notifications {
            Some("automatic task pickup is disabled in Settings")
        } else if self.delegation.connection.is_none() {
            Some("gateway stream is disconnected")
        } else if self.delegation.queue.is_none() {
            Some("durable delivery journal is unavailable")
        } else if self.delegation.verifying {
            Some("notification process verification is pending")
        } else if self.delegation.backlog.flight.is_some() {
            Some("queue claim/process verification is pending")
        } else if self
            .delegation
            .scan_completed
            .is_none_or(|at| at.elapsed() > Duration::from_secs(2))
        {
            Some("verified agent scan is missing or stale")
        } else {
            None
        };
        if let Some(reason) = blocked {
            self.queue_diagnostic(format!("Queue blocked: {reason}"));
            return Task::none();
        }
        let mut queries = Vec::new();
        for index in 0..self.workspaces.len() {
            let w = &self.workspaces[index];
            if self
                .delegation
                .candidates
                .iter()
                .any(|c| c.root.workspace == w.id)
                && !w.gateway.loading
                && w.gateway
                    .queried_at
                    .is_none_or(|at| at.elapsed() >= Duration::from_secs(10))
            {
                queries.push(self.query(index));
            }
        }
        // Resolving or refreshing one workspace must not stall a ready peer.
        // A closed task or retired tab cannot retain an in-memory handoff.
        let pending = std::mem::take(&mut self.delegation.backlog.pending);
        for pick in pending {
            let attached = self.backlog_scope(&pick).is_some()
                && self
                    .delegation
                    .candidates
                    .iter()
                    .any(|c| c.target == pick.candidate.target);
            let recorded = self
                .delegation
                .queue
                .as_ref()
                .unwrap()
                .journal
                .backlog
                .iter()
                .any(|r| r.task == pick.task && r.revision == pick.revision);
            if attached && recorded {
                self.delegation.backlog.pending.push(pick);
            } else if recorded
                && let Err(error) = self.delegation.queue.as_mut().unwrap().finish_backlog(
                    pick.task,
                    pick.revision,
                    Outcome::Skipped,
                )
            {
                self.delegation.fail(error);
                return Task::none();
            }
        }
        // Recheck both process ownership and idle state after an asynchronous claim.
        for mut pick in self.delegation.backlog.pending.clone() {
            pick.delivery = true;
            if self.backlog_ready(&pick, idle_after) {
                queries.push(self.verify_backlog(pick));
                return Task::batch(queries);
            }
        }
        let mut candidates = self.delegation.candidates.clone();
        candidates.sort_by_key(|c| {
            let position = (c.root.number, c.root.terminal);
            let wrapped = self
                .delegation
                .backlog
                .last_assigned
                .get(&c.target.project)
                .is_some_and(|last| position <= *last);
            (c.target.project.clone(), wrapped, position)
        });
        let mut selected: Option<(i64, Pick)> = None;
        for candidate in candidates {
            let Some(index) = self
                .workspaces
                .iter()
                .position(|w| w.id == candidate.root.workspace)
            else {
                continue;
            };
            let panel = &self.workspaces[index].gateway;
            if panel.project != candidate.target.project
                || panel
                    .loaded_at
                    .is_none_or(|at| at.elapsed() > Duration::from_secs(10))
                || self
                    .delegation
                    .queue
                    .as_ref()
                    .unwrap()
                    .journal
                    .backlog
                    .iter()
                    .any(|r| {
                        r.target.session == candidate.target.session
                            && matches!(
                                r.outcome,
                                Outcome::Queued | Outcome::Submitting | Outcome::Uncertain
                            )
                    })
                || self
                    .delegation
                    .queue
                    .as_ref()
                    .unwrap()
                    .journal
                    .receipts
                    .iter()
                    .any(|r| {
                        r.recipients.iter().any(|r| {
                            r.target.session == candidate.target.session && !r.outcome.terminal()
                        })
                    })
            {
                continue;
            }
            let journaled = |task: &Value| {
                self.delegation
                    .queue
                    .as_ref()
                    .unwrap()
                    .journal
                    .backlog
                    .iter()
                    .any(|r| {
                        task["id"] == r.task.to_string() && backlog::revision(task) == r.revision
                    })
            };
            // Owned in-progress work gets one reminder per task revision instead of more work.
            if let Some((id, revision)) = panel
                .tasks
                .iter()
                .find(|task| {
                    task["status"] == "in_progress"
                        && backlog::owns(task, &candidate.target)
                        && amux::task_platform::matches(
                            task,
                            candidate.target.origin.as_ref().map(|o| o.os.as_str()),
                        )
                        && !journaled(task)
                })
                .and_then(|task| {
                    let id = Uuid::parse_str(task["id"].as_str()?).ok()?;
                    Some((id, backlog::revision(task)))
                })
            {
                let pick = Pick {
                    request: Uuid::new_v4(),
                    task: id,
                    revision,
                    scope: self.scope(index),
                    candidate: candidate.clone(),
                    started: Instant::now(),
                    delivery: true,
                    reminder: true,
                    current_task: None,
                };
                if let Err(error) =
                    self.delegation
                        .queue
                        .as_mut()
                        .unwrap()
                        .begin_backlog(backlog::Receipt {
                            task: id,
                            revision,
                            target: candidate.target.clone(),
                            outcome: Outcome::Queued,
                        })
                {
                    self.delegation.fail(error);
                    return Task::none();
                }
                self.delegation.backlog.pending.push(pick);
                continue;
            }
            let task = panel
                .tasks
                .iter()
                .filter(|task| {
                    backlog::eligible(task, &candidate.target)
                        && !self
                            .delegation
                            .queue
                            .as_ref()
                            .unwrap()
                            .journal
                            .backlog
                            .iter()
                            .any(|r| {
                                task["id"] == r.task.to_string()
                                    && backlog::revision(task) == r.revision
                            })
                })
                .min_by_key(|task| task["created_at"].as_i64().unwrap_or(i64::MAX));
            let Some(task) = task else {
                continue;
            };
            let Some(id) = task["id"].as_str().and_then(|id| Uuid::parse_str(id).ok()) else {
                continue;
            };
            let created = task["created_at"].as_i64().unwrap_or(i64::MAX);
            let pick = Pick {
                request: Uuid::new_v4(),
                task: id,
                revision: backlog::revision(task),
                scope: self.scope(index),
                candidate,
                started: Instant::now(),
                delivery: false,
                reminder: false,
                current_task: None,
            };
            if self.backlog_ready(&pick, idle_after) {
                if selected.as_ref().is_none_or(|(age, _)| created < *age) {
                    selected = Some((created, pick));
                }
                continue;
            }
            let state = tab(self, pick.candidate.target.terminal)
                .map(|tab| {
                    format!(
                        "draft={}, output_idle_ms={:?}",
                        !tab.delegation.input.is_empty(),
                        tab.terminal
                            .as_ref()
                            .and_then(|terminal| terminal.activity_elapsed())
                            .map(|age| age.as_millis())
                    )
                })
                .unwrap_or_else(|| "terminal unavailable".into());
            self.queue_diagnostic(format!("Queue waiting: task={} agent={} {state}; requires idle output, no draft, verified process and no other owned task", pick.task, pick.candidate.target.session));
        }
        if let Some((_, pick)) = selected {
            queries.push(self.verify_backlog(pick));
            return Task::batch(queries);
        }
        let queued = self
            .workspaces
            .iter()
            .flat_map(|workspace| &workspace.gateway.tasks)
            .filter(|task| task["status"] == "todo")
            .count();
        if queued > 0 {
            self.queue_diagnostic(format!("Queue snapshot: {queued} queued rows, {} verified agents; checking OS, ownership, routing, goal and durable receipts", self.delegation.candidates.len()));
        }
        Task::batch(queries)
    }

    pub(super) fn backlog_verified(&mut self, pick: Pick, valid: bool) -> Task<Message> {
        self.backlog_verified_after(pick, valid, amux::session::IDLE_AFTER)
    }

    pub(super) fn backlog_verified_after(
        &mut self,
        pick: Pick,
        valid: bool,
        idle_after: Duration,
    ) -> Task<Message> {
        if self
            .delegation
            .backlog
            .flight
            .as_ref()
            .is_none_or(|p| p.request != pick.request)
        {
            return Task::none();
        }
        self.delegation.backlog.flight = None;
        if pick.delivery
            && !self.delegation.queue.as_ref().is_some_and(|q| {
                q.journal
                    .backlog
                    .iter()
                    .any(|r| r.task == pick.task && r.revision == pick.revision)
            })
        {
            self.delegation
                .backlog
                .pending
                .retain(|p| p.task != pick.task || p.revision != pick.revision);
            return Task::none();
        }
        if pick.delivery
            && pick.current_task.as_ref().is_some_and(|task| {
                task["status"] != "in_progress"
                    || !backlog::owns(task, &pick.candidate.target)
                    || !amux::task_platform::matches(
                        task,
                        pick.candidate.target.origin.as_ref().map(|o| o.os.as_str()),
                    )
            })
        {
            self.delegation
                .backlog
                .pending
                .retain(|p| p.task != pick.task || p.revision != pick.revision);
            if let Some(queue) = &mut self.delegation.queue
                && let Err(error) = queue.finish_backlog(pick.task, pick.revision, Outcome::Skipped)
            {
                self.delegation.fail(error);
            }
            self.delegation.backlog.status =
                "Queue task changed owner or closed before delivery.".into();
            return Task::none();
        }
        if !valid
            || pick.started.elapsed() > Duration::from_secs(2)
            || !self.preferences.current.automatic_notifications
            || self.delegation.connection.is_none()
            || self.delegation.verifying
            || !self.backlog_ready(&pick, idle_after)
        {
            return Task::none();
        }
        if pick.delivery {
            let Some(index) = self
                .delegation
                .backlog
                .pending
                .iter()
                .position(|p| p.task == pick.task && p.revision == pick.revision)
            else {
                return Task::none();
            };
            let Some(task) = &pick.current_task else {
                return Task::none();
            };
            let text = if pick.reminder {
                backlog::reminder_text
            } else {
                backlog::review_text
            };
            let message = match text(
                task,
                &pick.candidate.target,
                &pick.candidate.name,
                self.delegation.now_ms(),
            ) {
                Ok(message) => message,
                Err(error) => {
                    self.delegation.fail(error);
                    return Task::none();
                }
            };
            let Some(queue) = &mut self.delegation.queue else {
                return Task::none();
            };
            if let Err(error) = queue.finish_backlog(pick.task, pick.revision, Outcome::Submitting)
            {
                self.delegation.fail(error);
                return Task::none();
            }
            let terminal = tab(self, pick.candidate.target.terminal)
                .unwrap()
                .terminal
                .as_ref()
                .unwrap();
            let mut bytes = Vec::new();
            if terminal
                .input_mode()
                .contains(iced_term::TermMode::BRACKETED_PASTE)
            {
                bytes.extend_from_slice(b"\x1b[200~");
                bytes.extend_from_slice(message.as_bytes());
                bytes.extend_from_slice(b"\x1b[201~");
            } else {
                bytes.extend_from_slice(message.as_bytes());
            }
            bytes.extend_from_slice(b"\x1b[F\r");
            let outcome = if terminal.try_enqueue_input(bytes).is_ok() {
                Outcome::Submitted
            } else {
                Outcome::Uncertain
            };
            self.delegation.backlog.pending.remove(index);
            if let Err(error) = self.delegation.queue.as_mut().unwrap().finish_backlog(
                pick.task,
                pick.revision,
                outcome,
            ) {
                self.delegation.fail(error);
            }
            self.delegation.backlog.status = if outcome == Outcome::Submitted && pick.reminder {
                format!(
                    "Queue: reminded {} of in-progress task",
                    pick.candidate.name
                )
            } else if outcome == Outcome::Submitted {
                format!("Queue: sent task to {} for review", pick.candidate.name)
            } else {
                "Queue delivery unconfirmed. Inspect the task before assigning it again.".into()
            };
            self.queue_diagnostic(format!(
                "{}; task={} agent={}",
                self.delegation.backlog.status, pick.task, pick.candidate.target.session
            ));
            return Task::none();
        }
        let Ok(gateway) = self.preferences.current.gateway() else {
            return Task::none();
        };
        let receipt = backlog::Receipt {
            task: pick.task,
            revision: pick.revision,
            target: pick.candidate.target.clone(),
            outcome: Outcome::Submitting,
        };
        if let Err(error) = self
            .delegation
            .queue
            .as_mut()
            .unwrap()
            .begin_backlog(receipt)
        {
            self.delegation.fail(error);
            return Task::none();
        }
        self.delegation.backlog.flight = Some(pick.clone());
        self.delegation.backlog.status =
            format!("Queue: assigning task to {}", pick.candidate.name);
        let epoch = self.delegation.epoch;
        Task::perform(
            async move {
                let result = gateway
                    .claim_backlog(
                        pick.task,
                        pick.revision,
                        &pick.candidate.target,
                        &pick.candidate.actor,
                    )
                    .await
                    .map_err(|e| e.to_string());
                Event::BacklogClaimed(pick.request, result)
            },
            move |event| Message::Delegation(epoch, event),
        )
    }

    pub(super) fn backlog_claimed(
        &mut self,
        request: Uuid,
        mut result: Result<Option<Value>, String>,
    ) -> Task<Message> {
        let Some(pick) = self.delegation.backlog.flight.take() else {
            return Task::none();
        };
        if pick.request != request {
            self.delegation.backlog.flight = Some(pick);
            return Task::none();
        }
        if result.as_ref().is_ok_and(|task| {
            task.as_ref().is_some_and(|task| {
                task["id"] != pick.task.to_string()
                    || task["project_ident"] != pick.candidate.target.project
                    || task["status"] != "in_progress"
                    || !backlog::owns(task, &pick.candidate.target)
            })
        }) {
            result = Err("Gateway did not confirm the selected task and agent".into());
        }
        let outcome = match &result {
            Ok(Some(_)) => Outcome::Queued,
            Ok(None) => Outcome::Skipped,
            Err(_) => Outcome::Uncertain,
        };
        if let Some(queue) = &mut self.delegation.queue {
            if let Err(error) = queue.finish_backlog(pick.task, pick.revision, outcome) {
                self.delegation.fail(error);
                return Task::none();
            }
        } else {
            return Task::none();
        }
        match result {
            Ok(Some(task)) => {
                self.delegation.backlog.last_assigned.insert(
                    pick.candidate.target.project.clone(),
                    (pick.candidate.root.number, pick.candidate.root.terminal),
                );
                for w in &mut self.workspaces {
                    if w.gateway.project == pick.candidate.target.project {
                        for row in &mut w.gateway.tasks {
                            if row["id"] == pick.task.to_string() {
                                *row = task.clone();
                            }
                        }
                    }
                }
                self.delegation.backlog.status = format!(
                    "Queue: assigned to {}; waiting for idle terminal",
                    pick.candidate.name
                );
                self.delegation.backlog.pending.push(pick.clone());
            }
            Ok(None) => {
                self.delegation.backlog.status =
                    "Queue changed before assignment; refreshing tasks.".into()
            }
            Err(error) => {
                self.delegation.backlog.status = format!(
                    "Queue assignment unconfirmed: {error}. Inspect the task before continuing; this agent's queue is paused."
                )
            }
        }
        self.queue_diagnostic(format!(
            "{}; task={} agent={}",
            self.delegation.backlog.status, pick.task, pick.candidate.target.session
        ));
        self.refresh_task_project(&pick.candidate.target.project)
    }
}

#[cfg(test)]
mod tests {
    use super::super::tests::{Agent, bridge, find, human, project, scan, spawn, wait_lines};
    use super::*;
    use serde_json::json;

    fn fixture() -> (Desktop, Agent, Agent, Vec<Value>) {
        let mut app = super::super::super::tests::desktop();
        let first = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
        let second = spawn(&mut app, 0, 2, Provider::Claude, "opaque");
        let _frames = bridge(&mut app, &first.directory.join("backlog.json"));
        app.preferences.current.url = "http://127.0.0.1:9".into();
        app.preferences.current.api_key = "fixture-only".into();
        project(&mut app, &first, "alpha");
        project(&mut app, &second, "alpha");
        scan(&mut app, None);
        let tasks = (0..2).map(|i| json!({"id":Uuid::new_v4(), "project_ident":"alpha", "status":"todo", "title":format!("Queued item {i}"), "details":"Review whether this remains useful", "updated_at":10, "routing":{"mode":"unassigned"}})).collect::<Vec<_>>();
        let panel = &mut app.workspaces[0].gateway;
        panel.project = "alpha".into();
        panel.tasks = tasks.clone();
        refreshed(&mut app);
        (app, first, second, tasks)
    }

    fn refreshed(app: &mut Desktop) {
        let panel = &mut app.workspaces[0].gateway;
        panel.loading = false;
        panel.loaded_at = Some(Instant::now());
        panel.queried_at = Some(Instant::now());
        scan(app, None);
    }

    fn verified(app: &mut Desktop, task: Option<Value>) {
        let mut pick = app.delegation.backlog.flight.clone().unwrap();
        pick.current_task = task.map(Box::new);
        let c = &pick.candidate;
        let valid = delegation_process::verify(
            c.root.pid,
            &c.root.generation,
            c.root.foreground,
            &c.process,
        );
        assert!(valid);
        drop(app.backlog_verified_after(pick, valid, Duration::ZERO));
    }

    #[test]
    fn fifo_selects_oldest_eligible_work_across_agents_ignoring_rank_and_updates() {
        let (mut app, first, second, mut tasks) = fixture();
        tasks[0]["created_at"] = json!(100);
        tasks[0]["rank"] = json!(-100);
        tasks[0]["updated_at"] = json!(99999);
        tasks[1]["created_at"] = json!(10);
        tasks[1]["rank"] = json!(99999);
        let original = find(&app, first.terminal).delegation.root.clone();
        assert!(original.is_some());
        let author = app
            .delegation
            .candidates
            .iter()
            .find(|candidate| candidate.target.terminal == first.terminal)
            .unwrap()
            .target
            .session;
        tasks[1]["labels"] = json!(["task-review", format!("review-exclude:{author}")]);
        app.workspaces[0].gateway.tasks = tasks.clone();
        drop(app.backlog_tick_after(Duration::ZERO));
        let picked = app.delegation.backlog.flight.as_ref().unwrap();
        assert_eq!(picked.task.to_string(), tasks[1]["id"]);
        assert_eq!(picked.candidate.target.terminal, second.terminal);
        assert!(first.lines().is_empty() && second.lines().is_empty());
    }

    #[test]
    fn in_progress_work_is_flagged_only_when_its_owner_is_not_a_local_agent() {
        let (app, _, _, tasks) = fixture();
        let local = app.delegation.candidates[0].target.origin.clone().unwrap();
        let mut remote = local.clone();
        remote.session_id = Uuid::new_v4();
        remote.instance_id = Uuid::new_v4();
        let task = |status: &str, origin: &amux::delegation::Origin| {
            let mut task = tasks[0].clone();
            task["status"] = json!(status);
            task["owner_agent_id"] = json!("Peer - Codex");
            task["owner_origin"] = json!(origin);
            task
        };
        assert!(app.task_owned_elsewhere(&task("in_progress", &remote)));
        assert!(!app.task_owned_elsewhere(&task("in_progress", &local)));
        assert!(!app.task_owned_elsewhere(&task("todo", &remote)));
        assert!(!app.task_owned_elsewhere(&tasks[0]));
        let mut legacy = task("in_progress", &local);
        legacy["owner_origin"] = Value::Null;
        legacy["owner_agent_id"] = json!(local.session_id.to_string());
        assert!(!app.task_owned_elsewhere(&legacy));
        legacy["owner_agent_id"] = json!("Peer - Codex");
        assert!(app.task_owned_elsewhere(&legacy));
        if let Some(id) = &app.delegation.candidates[0].target.agent_id {
            legacy["owner_agent_id"] = json!(id);
            assert!(!app.task_owned_elsewhere(&legacy));
        }
    }

    #[test]
    fn unresolved_workspace_is_polled_without_stalling_another_idle_workspace() {
        let (mut app, _, _, tasks) = fixture();
        let peer = spawn(&mut app, 1, 1, Provider::Codex, "opaque");
        project(&mut app, &peer, "beta");
        let mut queued = tasks[0].clone();
        queued["project_ident"] = json!("beta");
        let panel = &mut app.workspaces[1].gateway;
        panel.project = "beta".into();
        panel.tasks = vec![queued];
        panel.loaded_at = Some(Instant::now());
        panel.queried_at = Some(Instant::now());
        // A failed initial lookup leaves this workspace unbound, even after its
        // native agent's repository identity has been discovered independently.
        app.workspaces[0].gateway.project.clear();
        app.workspaces[0].gateway.queried_at = None;
        scan(&mut app, None);
        drop(app.backlog_tick_after(Duration::ZERO));
        assert!(
            app.workspaces[0].gateway.loading,
            "retry unresolved workspace"
        );
        let pick = app
            .delegation
            .backlog
            .flight
            .as_ref()
            .expect("other workspace still dispatches");
        assert_eq!(pick.candidate.target.terminal, peer.terminal);
        assert_eq!(pick.scope.workspace_id, app.workspaces[1].id);
        assert_eq!(pick.candidate.target.project, "beta");
    }

    #[test]
    fn user_review_never_dispatches_or_reminds_and_parking_cancels_a_pending_handoff() {
        let (mut app, first, second, tasks) = fixture();
        let mut parked = tasks[0].clone();
        parked["status"] = json!("user_review");
        app.workspaces[0].gateway.tasks = vec![parked.clone()];
        for _ in 0..3 {
            refreshed(&mut app);
            drop(app.backlog_tick_after(Duration::ZERO));
            assert!(app.delegation.backlog.flight.is_none());
        }
        let mut returned = parked.clone();
        returned["status"] = json!("todo");
        returned["updated_at"] = json!(11);
        app.workspaces[0].gateway.tasks = vec![returned.clone()];
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        let pick = app.delegation.backlog.flight.clone().unwrap();
        verified(&mut app, None);
        let mut claimed = returned;
        claimed["status"] = json!("in_progress");
        claimed["owner_origin"] = json!(pick.candidate.target.origin);
        drop(app.backlog_claimed(pick.request, Ok(Some(claimed))));
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        // The final authoritative GET sees User review after the claim completed.
        verified(&mut app, Some(parked));
        assert!(first.lines().is_empty() && second.lines().is_empty());
        assert!(app.delegation.backlog.pending.is_empty());
        assert_eq!(
            app.delegation.queue.as_ref().unwrap().journal.backlog[0].outcome,
            Outcome::Skipped
        );
    }

    #[test]
    fn empty_autoreview_check_does_not_lock_out_a_returned_finding() {
        let (mut app, first, second, tasks) = fixture();
        let mut task = tasks[0].clone();
        task["labels"] = json!(["autoreview-finding", "bug"]);
        app.workspaces[0].gateway.tasks = vec![task.clone()];
        app.preferences.current.autoreview = true;
        let request = Uuid::new_v4();
        app.delegation.autoreview.flight = Some(request);
        drop(app.backlog_tick_after(Duration::ZERO));
        assert!(
            app.delegation.backlog.flight.is_some(),
            "review creation never reserves an agent"
        );
        app.delegation.backlog.flight = None;
        // Preparing a review found no unreviewed commits. Only its transient
        // flight is cleared; this must not reserve the agent indefinitely.
        drop(app.autoreview_prepared(request, Ok(None)));
        assert!(app.delegation.autoreview.flight.is_none());
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        assert!(
            app.delegation.backlog.flight.is_some(),
            "queued findings have no hidden label hold"
        );
        app.delegation.backlog.flight = None;

        // The human's authoritative return clears the hold, preserving labels.
        task["labels"] = json!(["bug"]);
        task["updated_at"] = json!(12);
        app.workspaces[0].gateway.tasks = vec![task.clone()];
        app.delegation.reconcile_backlog("alpha", &[task.clone()]);
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        let pick = app
            .delegation
            .backlog
            .flight
            .clone()
            .expect("returned work is selected");
        assert_eq!(pick.task.to_string(), task["id"]);
        assert_eq!(pick.candidate.target.terminal, first.terminal);
        verified(&mut app, None);
        task["status"] = json!("in_progress");
        task["owner_origin"] = json!(pick.candidate.target.origin);
        drop(app.backlog_claimed(pick.request, Ok(Some(task.clone()))));
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        verified(&mut app, Some(task));
        let lines = wait_lines(&first, 1);
        assert_eq!(lines.len(), 1);
        assert!(lines[0].contains("Queued task review"));
        assert!(second.lines().is_empty());
    }

    #[test]
    fn ordinary_requeue_after_assignment_is_not_pinned_to_a_retired_execution() {
        let (mut app, first, second, tasks) = fixture();
        app.workspaces[0].gateway.tasks = vec![tasks[0].clone()];
        drop(app.backlog_tick_after(Duration::ZERO));
        let pick = app.delegation.backlog.flight.clone().unwrap();
        verified(&mut app, None);
        let mut task = tasks[0].clone();
        task["status"] = json!("in_progress");
        task["owner_origin"] = json!(pick.candidate.target.origin);
        drop(app.backlog_claimed(pick.request, Ok(Some(task.clone()))));
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        verified(&mut app, Some(task.clone()));
        assert_eq!(wait_lines(&first, 1).len(), 1);

        task["status"] = json!("user_review");
        task["owner_origin"] = Value::Null;
        task["owner_agent_id"] = Value::Null;
        task["updated_at"] = json!(20);
        app.workspaces[0].gateway.tasks = vec![task.clone()];
        app.delegation.reconcile_backlog("alpha", &[task.clone()]);
        assert!(
            app.delegation
                .queue
                .as_ref()
                .unwrap()
                .journal
                .backlog
                .is_empty()
        );

        // An ordinary task has no finding label. If Return to queue retains a
        // retired explicit assignment, no current execution can claim it.
        let mut retired = pick.candidate.target.origin.clone().unwrap();
        retired.instance_id = Uuid::new_v4();
        task["status"] = json!("todo");
        task["updated_at"] = json!(21);
        task["routing"] = json!({"mode":"session","target":{
            "origin":retired,"generation":Uuid::new_v4()}});
        app.workspaces[0].gateway.tasks = vec![task.clone()];
        let review = Uuid::new_v4();
        app.delegation.autoreview.flight = Some(review);
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        assert!(app.delegation.backlog.flight.is_none());
        drop(app.autoreview_prepared(review, Ok(None)));
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        assert!(
            app.delegation.backlog.flight.is_none(),
            "stale explicit route still blocks"
        );

        // Returning to the general queue must clear that assignment server-side.
        task["routing"] = json!({"mode":"auto"});
        app.workspaces[0].gateway.tasks = vec![task.clone()];
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        let next = app.delegation.backlog.flight.clone().unwrap();
        assert_eq!(next.candidate.target.terminal, second.terminal);
        verified(&mut app, None);
        task["status"] = json!("in_progress");
        task["owner_origin"] = json!(next.candidate.target.origin);
        drop(app.backlog_claimed(next.request, Ok(Some(task.clone()))));
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        verified(&mut app, Some(task));
        assert_eq!(wait_lines(&second, 1).len(), 1);
        assert_eq!(
            first.lines().len(),
            1,
            "old execution is not injected again"
        );
    }

    #[test]
    fn idle_assignments_rotate_after_completion_and_failed_claims_do_not_advance() {
        let (mut app, first, second, tasks) = fixture();
        drop(app.backlog_tick_after(Duration::ZERO));
        verified(&mut app, None);
        let request = app.delegation.backlog.flight.as_ref().unwrap().request;
        drop(app.backlog_claimed(request, Ok(None)));
        assert!(app.delegation.backlog.last_assigned.is_empty());
        for (i, expected) in [first.terminal, second.terminal, first.terminal]
            .into_iter()
            .enumerate()
        {
            let mut queued = tasks[0].clone();
            queued["id"] = json!(Uuid::new_v4());
            app.workspaces[0].gateway.tasks = vec![queued.clone()];
            refreshed(&mut app);
            drop(app.backlog_tick_after(Duration::ZERO));
            let pick = app.delegation.backlog.flight.clone().unwrap();
            assert_eq!(
                pick.candidate.target.terminal, expected,
                "round-robin assignment {i}"
            );
            verified(&mut app, None);
            let mut claimed = queued;
            claimed["status"] = json!("in_progress");
            claimed["owner_origin"] = json!(pick.candidate.target.origin);
            drop(app.backlog_claimed(pick.request, Ok(Some(claimed.clone()))));
            refreshed(&mut app);
            drop(app.backlog_tick_after(Duration::ZERO));
            verified(&mut app, Some(claimed));
            let (agent, count) = if expected == first.terminal {
                (&first, if i == 0 { 1 } else { 2 })
            } else {
                (&second, 1)
            };
            assert_eq!(wait_lines(agent, count).len(), count);
            app.workspaces[0].gateway.tasks[0]["status"] = json!("done");
            app.delegation
                .reconcile_backlog("alpha", &app.workspaces[0].gateway.tasks);
        }
        assert_eq!(first.lines().len(), 2);
        assert_eq!(second.lines().len(), 1);
    }

    #[test]
    fn native_backlog_claims_one_task_for_first_idle_agent_before_injection() {
        let (mut app, first, second, tasks) = fixture();
        // Production uses the common 30-second clock. The native fixture just printed.
        drop(app.backlog_tick());
        assert!(app.delegation.backlog.flight.is_none());
        drop(app.backlog_tick_after(Duration::ZERO));
        let pick = app.delegation.backlog.flight.clone().unwrap();
        assert_eq!(pick.task.to_string(), tasks[0]["id"]);
        assert_eq!(pick.candidate.target.terminal, first.terminal);
        assert!(first.lines().is_empty() && second.lines().is_empty());
        verified(&mut app, None);
        let queue = app.delegation.queue.as_ref().unwrap();
        assert_eq!(queue.journal.backlog[0].outcome, Outcome::Submitting);
        let disk =
            amux::delegation::Journal::load(&first.directory.join("backlog.json"), "fixture")
                .unwrap();
        assert_eq!(
            disk.backlog[0].outcome,
            Outcome::Uncertain,
            "claim is fenced before HTTP"
        );
        assert!(first.lines().is_empty());
        let mut claimed = tasks[0].clone();
        claimed["status"] = json!("in_progress");
        claimed["owner_origin"] = serde_json::to_value(&pick.candidate.target.origin).unwrap();
        claimed["owner_agent_id"] = json!(pick.candidate.actor.gateway_id());
        let request = app.delegation.backlog.flight.as_ref().unwrap().request;
        drop(app.backlog_claimed(request, Ok(Some(claimed.clone()))));
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        verified(&mut app, Some(claimed));
        let lines = wait_lines(&first, 1);
        assert_eq!(lines.len(), 1);
        assert!(lines[0].contains("Queued task review") && lines[0].contains("Queued item 0"));
        assert!(
            lines[0].contains("already claimed")
                && lines[0].contains("close it with a reason if obsolete")
        );
        assert!(lines[0].contains(&format!(
            "amux --agent {} tasks user-review {}",
            pick.candidate.target.session, pick.task
        )));
        assert!(second.lines().is_empty());
        assert_eq!(
            app.delegation.queue.as_ref().unwrap().journal.backlog[0].outcome,
            Outcome::Submitted
        );
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        let next = app.delegation.backlog.flight.as_ref().unwrap();
        assert_eq!(next.task.to_string(), tasks[1]["id"]);
        assert_eq!(next.candidate.target.terminal, second.terminal);
        assert_eq!(
            first.lines().len(),
            1,
            "owned active task prevents another assignment"
        );
    }

    #[test]
    fn idle_owner_of_in_progress_work_gets_one_reminder_per_revision_without_a_claim() {
        let (mut app, first, second, tasks) = fixture();
        let target = app
            .delegation
            .candidates
            .iter()
            .find(|c| c.target.terminal == first.terminal)
            .unwrap()
            .target
            .clone();
        let mut owned = tasks[0].clone();
        owned["status"] = json!("in_progress");
        owned["updated_at"] = json!(20);
        owned["owner_origin"] = serde_json::to_value(&target.origin).unwrap();
        app.workspaces[0].gateway.tasks = vec![owned.clone()];
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        assert!(app.delegation.backlog.flight.is_none(), "nothing to claim");
        assert_eq!(
            app.delegation.queue.as_ref().unwrap().journal.backlog[0].outcome,
            Outcome::Queued
        );
        // The production threshold waits well past ordinary output.
        drop(app.backlog_tick());
        assert!(app.delegation.backlog.flight.is_none());
        drop(app.backlog_tick_after(Duration::ZERO));
        let pick = app.delegation.backlog.flight.clone().unwrap();
        assert!(pick.reminder && pick.delivery);
        assert_eq!(pick.candidate.target.terminal, first.terminal);
        verified(&mut app, Some(owned));
        let lines = wait_lines(&first, 1);
        assert!(
            lines[0].contains("In-progress task reminder") && lines[0].contains("Queued item 0")
        );
        assert!(lines[0].contains("mark it done if complete"));
        assert!(lines[0].contains("tasks user-review"));
        assert!(second.lines().is_empty());
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        assert!(app.delegation.backlog.flight.is_none());
        assert!(
            app.delegation.backlog.pending.is_empty(),
            "one reminder per revision"
        );
        assert_eq!(first.lines().len(), 1);
    }

    #[test]
    fn drafts_and_input_during_verification_defer_backlog_without_a_claim() {
        let (mut app, first, second, _) = fixture();
        human(&mut app, first.terminal, b"unfinished user draft");
        drop(app.backlog_tick_after(Duration::ZERO));
        assert_eq!(
            app.delegation
                .backlog
                .flight
                .as_ref()
                .unwrap()
                .candidate
                .target
                .terminal,
            second.terminal
        );
        human(&mut app, second.terminal, b"new text during verification");
        verified(&mut app, None);
        assert!(app.delegation.backlog.flight.is_none());
        assert!(
            app.delegation
                .queue
                .as_ref()
                .unwrap()
                .journal
                .backlog
                .is_empty()
        );
        assert!(first.lines().is_empty() && second.lines().is_empty());
        assert!(!find(&app, first.terminal).delegation.input.is_empty());
    }

    #[test]
    fn assignment_picker_combines_local_workspaces_and_gateway_agents() {
        use amux::task_routing::{Available, Directory, SessionTarget};
        let (mut app, _, _, _) = fixture();
        let peer = spawn(&mut app, 1, 1, Provider::Codex, "opaque");
        project(&mut app, &peer, "alpha");
        app.workspaces[1].gateway.project = "alpha".into();
        scan(&mut app, None);
        assert_eq!(app.routing_actors(app.workspaces[0].id).len(), 3);
        let mut targets = app
            .delegation
            .candidates
            .iter()
            .map(|candidate| Available {
                target: SessionTarget {
                    origin: candidate.target.origin.clone().unwrap(),
                    generation: candidate.target.executor_generation(),
                },
                seen_at: unix_ms() as i64,
                expires_at: unix_ms() as i64 + 120_000,
                display_name: Some(candidate.name.clone()),
                hostname: Some("LocalMachine".into()),
            })
            .collect::<Vec<_>>();
        targets.push(Available {
            target: SessionTarget {
                origin: delegation::Origin {
                    session_id: Uuid::new_v4(),
                    instance_id: Uuid::new_v4(),
                    provider: Provider::Codex,
                    os: "windows".into(),
                },
                generation: Uuid::new_v4(),
            },
            seen_at: unix_ms() as i64,
            expires_at: unix_ms() as i64 + 120_000,
            display_name: Some("Remote Peer".into()),
            hostname: Some("OtherMachine".into()),
        });
        app.workspaces[0].gateway.routing.checked = Some(Instant::now());
        app.workspaces[0].gateway.routing.directory = Some(Directory {
            targets,
            canonical_remote: Some("github.com/nitecon/amux".into()),
        });
        let labels = app
            .task_agent_choices()
            .iter()
            .map(ToString::to_string)
            .collect::<Vec<_>>();
        assert_eq!(labels.len(), 4);
        assert!(labels[..3].iter().all(|label| label.starts_with("Local ·")));
        assert!(labels[3].contains("Gateway · Remote Peer · OtherMachine · windows"));
    }

    #[test]
    fn a_draft_or_changed_owner_after_claim_never_receives_the_backlog_prompt() {
        let (mut app, first, second, tasks) = fixture();
        drop(app.backlog_tick_after(Duration::ZERO));
        let pick = app.delegation.backlog.flight.clone().unwrap();
        verified(&mut app, None);
        let mut claimed = tasks[0].clone();
        claimed["status"] = json!("in_progress");
        claimed["owner_origin"] = json!(pick.candidate.target.origin);
        let request = app.delegation.backlog.flight.as_ref().unwrap().request;
        drop(app.backlog_claimed(request, Ok(Some(claimed.clone()))));
        app.workspaces[0]
            .gateway
            .tasks
            .retain(|task| task["id"] == claimed["id"]);
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        human(&mut app, first.terminal, b"draft entered after the claim");
        verified(&mut app, Some(claimed.clone()));
        assert!(first.lines().is_empty());
        assert_eq!(
            app.delegation.queue.as_ref().unwrap().journal.backlog[0].outcome,
            Outcome::Queued
        );
        human(&mut app, first.terminal, b"\x15");
        refreshed(&mut app);
        drop(app.backlog_tick_after(Duration::ZERO));
        claimed["owner_origin"] = json!(
            app.delegation
                .candidates
                .iter()
                .find(|c| c.target.terminal == second.terminal)
                .unwrap()
                .target
                .origin
        );
        verified(&mut app, Some(claimed));
        assert!(first.lines().is_empty() && second.lines().is_empty());
        assert!(app.delegation.backlog.pending.is_empty());
        assert_eq!(
            app.delegation.queue.as_ref().unwrap().journal.backlog[0].outcome,
            Outcome::Skipped
        );
    }
}
