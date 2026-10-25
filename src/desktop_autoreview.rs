//! Review preparation is asynchronous; final native input remains on the UI thread.
use super::super::QueryScope;
use super::*;
use amux::autoreview;

#[derive(Default)]
pub(super) struct State {
    pub(super) flight: Option<Uuid>,
    checked: HashMap<Uuid, Instant>,
    pub(super) status: String,
}

#[derive(Debug, Clone)]
pub(in super::super) struct Prepared {
    request: Uuid,
    scope: QueryScope,
    candidate: Candidate,
    batch: autoreview::Batch,
    path: PathBuf,
    verified: Instant,
}

fn queued(tasks: &[serde_json::Value]) -> bool {
    tasks
        .iter()
        .any(|task| matches!(task["status"].as_str(), Some("todo" | "queued")))
}

impl Desktop {
    pub(crate) fn autoreview_tick(&mut self) -> Task<Message> {
        self.autoreview_tick_after(amux::session::IDLE_AFTER)
    }

    fn review_ready(&mut self, scope: &QueryScope, candidate: &Candidate, idle: Duration) -> bool {
        if !self.preferences.current.autoreview
            || self.preferences.open
            || self.workspace_dialog.open
            || self.delegation.connection.is_none()
            || self.delegation.verifying
            || self.delegation.backlog.flight.is_some()
            || !self.scope_current(scope)
            || !self
                .delegation
                .candidates
                .iter()
                .any(|c| c.target == candidate.target)
            || self
                .delegation
                .scan_completed
                .is_none_or(|at| at.elapsed() > Duration::from_secs(2))
        {
            return false;
        }
        let workspaces: Vec<_> = self
            .workspaces
            .iter()
            .filter(|w| w.gateway.project == scope.project)
            .collect();
        if workspaces.iter().any(|w| {
            w.gateway.loading
                || w.gateway
                    .loaded_at
                    .is_none_or(|at| at.elapsed() > Duration::from_secs(10))
                || w.gateway.request.is_some()
                || w.gateway.detail.unresolved()
                || queued(&w.gateway.tasks)
                || w.gateway.tasks.iter().any(|task| {
                    task["status"] == "in_progress" && amux::backlog::owns(task, &candidate.target)
                })
        }) {
            return false;
        }
        let Some(queue) = &self.delegation.queue else {
            return false;
        };
        if queue.journal.backlog.iter().any(|r| {
            r.target.session == candidate.target.session
                && matches!(
                    r.outcome,
                    Outcome::Queued | Outcome::Submitting | Outcome::Uncertain
                )
        }) || queue.journal.receipts.iter().any(|r| {
            r.recipients
                .iter()
                .any(|r| r.target.session == candidate.target.session && !r.outcome.terminal())
        }) {
            return false;
        }
        tab(self, candidate.target.terminal).is_some_and(|tab| {
            tab.delegation.input.is_empty()
                && tab.delegation.root.as_ref()
                    == Some(&(candidate.root.pid, candidate.root.generation.clone()))
                && tab.terminal.as_ref().is_some_and(|terminal| {
                    terminal
                        .activity_elapsed()
                        .is_some_and(|elapsed| elapsed >= idle)
                        && foreground(terminal) == candidate.root.foreground
                })
        })
    }

    pub(super) fn autoreview_tick_after(&mut self, idle: Duration) -> Task<Message> {
        if !self.preferences.current.autoreview || self.delegation.autoreview.flight.is_some() {
            return Task::none();
        }
        let mut queries = Vec::new();
        for index in 0..self.workspaces.len() {
            let panel = &self.workspaces[index].gateway;
            if !panel.loading
                && panel
                    .queried_at
                    .is_none_or(|at| at.elapsed() >= Duration::from_secs(10))
                && self
                    .delegation
                    .candidates
                    .iter()
                    .any(|c| c.root.workspace == self.workspaces[index].id)
            {
                queries.push(self.query(index));
            }
        }
        if !queries.is_empty() {
            return Task::batch(queries);
        }
        for index in 0..self.workspaces.len() {
            let scope = self.scope(index);
            if self
                .delegation
                .autoreview
                .checked
                .get(&scope.workspace_id)
                .is_some_and(|at| at.elapsed() < Duration::from_secs(10))
            {
                continue;
            }
            let Some(project) = self
                .delegation
                .candidates
                .iter()
                .find(|c| {
                    c.root.workspace == scope.workspace_id && c.target.project == scope.project
                })
                .map(|c| c.actor.project_id)
            else {
                continue;
            };
            let mut candidates = self.delegation.candidates.clone();
            candidates.retain(|c| {
                c.actor.project_id == project
                    && c.target.project == scope.project
                    && self.review_ready(&scope, c, idle)
            });
            if candidates.is_empty() {
                continue;
            }
            candidates.sort_by_key(|c| (c.root.number, c.root.terminal));
            let Ok(gateway) = self.preferences.current.gateway() else {
                return Task::none();
            };
            let request = Uuid::new_v4();
            self.delegation.autoreview.flight = Some(request);
            self.delegation
                .autoreview
                .checked
                .insert(scope.workspace_id, Instant::now());
            let epoch = self.delegation.epoch;
            return Task::perform(
                async move {
                    let result = async {
                        let directory = candidates[0].actor.project_directory.clone();
                        let (path, batch) =
                            tokio::task::spawn_blocking(move || -> anyhow::Result<_> {
                                let path = autoreview::path(&directory)?;
                                let batch = autoreview::prepare(&directory, &path)?;
                                Ok((path, batch))
                            })
                            .await??;
                        let Some(batch) = batch else {
                            return Ok(None);
                        };
                        prefer(&mut candidates, batch.preferred);
                        let tasks = gateway.list_all(&scope.project).await?;
                        let tasks = tasks
                            .as_array()
                            .ok_or_else(|| anyhow::anyhow!("Gateway task list is incomplete"))?;
                        if queued(tasks) {
                            return Ok(None);
                        }
                        for candidate in candidates {
                            if tasks.iter().any(|t| {
                                t["status"] == "in_progress"
                                    && amux::backlog::owns(t, &candidate.target)
                            }) {
                                continue;
                            }
                            let c = candidate.clone();
                            let valid = tokio::task::spawn_blocking(move || {
                                delegation_process::verify(
                                    c.root.pid,
                                    &c.root.generation,
                                    c.root.foreground,
                                    &c.process,
                                )
                            })
                            .await?;
                            if valid {
                                return Ok(Some(Prepared {
                                    request,
                                    scope,
                                    candidate,
                                    batch,
                                    path,
                                    verified: Instant::now(),
                                }));
                            }
                        }
                        Ok(None)
                    }
                    .await;
                    Event::AutoreviewPrepared(
                        request,
                        result
                            .map_err(|e: anyhow::Error| e.to_string())
                            .map(|p| p.map(Box::new)),
                    )
                },
                move |event| Message::Delegation(epoch, event),
            );
        }
        Task::none()
    }

    pub(super) fn autoreview_prepared(
        &mut self,
        request: Uuid,
        result: Result<Option<Box<Prepared>>, String>,
    ) -> Task<Message> {
        self.autoreview_prepared_after(request, result, amux::session::IDLE_AFTER)
    }

    fn autoreview_prepared_after(
        &mut self,
        request: Uuid,
        result: Result<Option<Box<Prepared>>, String>,
        idle: Duration,
    ) -> Task<Message> {
        if self.delegation.autoreview.flight != Some(request) {
            return Task::none();
        }
        self.delegation.autoreview.flight = None;
        let prepared = match result {
            Ok(Some(prepared)) => prepared,
            Ok(None) => return Task::none(),
            Err(error) => {
                self.delegation.autoreview.status = format!("Autoreview deferred: {error}");
                return Task::none();
            }
        };
        if prepared.request != request
            || prepared.verified.elapsed() > Duration::from_secs(2)
            || !self.review_ready(&prepared.scope, &prepared.candidate, idle)
        {
            return Task::none();
        }
        if let Err(error) =
            autoreview::reserve(&prepared.path, &prepared.batch, &prepared.candidate.actor)
        {
            self.delegation.autoreview.status = format!("Autoreview deferred: {error}");
            return Task::none();
        }
        let message = autoreview::prompt(&prepared.batch, &prepared.candidate.actor.agent_id);
        let terminal = tab(self, prepared.candidate.target.terminal)
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
        self.delegation.autoreview.status = if terminal.try_enqueue_input(bytes).is_ok() {
            format!(
                "Autoreview: {} reviewing {} commits",
                prepared.candidate.name,
                prepared.batch.commits.len()
            )
        } else {
            format!(
                "Autoreview delivery uncertain ({}). Inspect before using amux reviews release; no automatic retry.",
                prepared.batch.id
            )
        };
        Task::none()
    }
}

fn prefer(candidates: &mut [Candidate], provider: Option<Provider>) {
    candidates.sort_by_key(|c| {
        (
            Some(c.actor.provider) != provider,
            c.root.number,
            c.root.terminal,
        )
    });
}

#[cfg(test)]
mod tests {
    use super::super::tests::{Agent, bridge, human, project, scan, spawn, wait_lines};
    use super::*;
    use serde_json::json;

    fn fixture() -> (Desktop, Agent, Agent) {
        let mut app = super::super::super::tests::desktop();
        let first = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
        let second = spawn(&mut app, 0, 2, Provider::Claude, "opaque");
        let _frames = bridge(&mut app, &first.directory.join("review-events.json"));
        project(&mut app, &first, "alpha");
        project(&mut app, &second, "alpha");
        app.workspaces[0].gateway.project = "alpha".into();
        app.workspaces[0].gateway.loaded_at = Some(Instant::now());
        app.workspaces[0].gateway.queried_at = Some(Instant::now());
        scan(&mut app, None);
        app.preferences.current.autoreview = true;
        app.preferences.current.automatic_notifications = false;
        (app, first, second)
    }

    fn prepared(app: &mut Desktop, path: PathBuf) -> Prepared {
        let mut candidates = app.delegation.candidates.clone();
        prefer(&mut candidates, Some(Provider::Claude));
        let candidate = candidates.remove(0);
        assert!(delegation_process::verify(
            candidate.root.pid,
            &candidate.root.generation,
            candidate.root.foreground,
            &candidate.process
        ));
        let request = Uuid::new_v4();
        app.delegation.autoreview.flight = Some(request);
        Prepared {
            request,
            scope: app.scope(0),
            candidate,
            batch: autoreview::Batch {
                id: Uuid::new_v4(),
                commits: vec!["a".repeat(40)],
                preferred: Some(Provider::Claude),
            },
            path,
            verified: Instant::now(),
        }
    }

    #[test]
    fn native_review_uses_preferred_provider_once_and_does_not_mark_commits_reviewed() {
        let (mut app, codex, claude) = fixture();
        let path = claude.directory.join("review-history.json");
        let work = prepared(&mut app, path.clone());
        assert_eq!(work.candidate.actor.provider, Provider::Claude);
        // A just-active agent is not eligible under the real 30-second clock.
        assert!(!app.review_ready(&work.scope, &work.candidate, amux::session::IDLE_AFTER));
        let request = work.request;
        drop(app.autoreview_prepared_after(
            request,
            Ok(Some(Box::new(work.clone()))),
            Duration::ZERO,
        ));
        let lines = wait_lines(&claude, 1);
        assert_eq!(lines.len(), 1);
        assert!(
            lines[0].contains("Review ONLY")
                && lines[0].contains("--review-finding")
                && lines[0].contains("--get-guidelines")
        );
        assert!(codex.lines().is_empty());
        let history = autoreview::load(&path).unwrap();
        assert!(history.reviewed.is_empty() && history.pending.is_some());
        drop(app.autoreview_prepared_after(request, Ok(Some(Box::new(work))), Duration::ZERO));
        assert_eq!(claude.lines().len(), 1);
    }

    #[test]
    fn new_queue_work_drafts_disabled_setting_and_rebound_project_block_review_input() {
        for reason in 0..4 {
            let (mut app, codex, claude) = fixture();
            let path = claude.directory.join("blocked-review.json");
            let work = prepared(&mut app, path.clone());
            match reason {
                0 => app.workspaces[0]
                    .gateway
                    .tasks
                    .push(json!({"status":"todo"})),
                1 => human(&mut app, claude.terminal, b"keep my draft"),
                2 => app.preferences.current.autoreview = false,
                _ => app.workspaces[0].gateway.project = "rebound".into(),
            }
            drop(app.autoreview_prepared_after(
                work.request,
                Ok(Some(Box::new(work))),
                Duration::ZERO,
            ));
            assert!(!path.exists());
            assert!(codex.lines().is_empty() && claude.lines().is_empty());
        }
    }
}
