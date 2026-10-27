//! Completion review creation never selects an agent or writes terminal input.
use super::*;
use amux::delegation::ReviewRequest;

#[derive(Default)]
pub(super) struct State {
    pub(super) flight: Option<Uuid>,
    retry_at: Option<Instant>,
    pub(super) status: String,
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn review_creation_failure_keeps_durable_request_and_never_claims_an_agent() {
        let mut app = super::super::super::tests::desktop();
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        let directory = tempfile::tempdir_in(root).unwrap();
        let path = directory.path().join("reviews.json");
        let request = ReviewRequest {
            project: "alpha".into(),
            task: Uuid::new_v4(),
            event: 10,
        };
        let mut queue = Queue::open(path.clone(), "fixture").unwrap();
        queue.schedule_review(request.clone()).unwrap();
        app.delegation.queue = Some(queue);
        let id = Uuid::new_v4();
        app.delegation.autoreview.flight = Some(id);
        drop(app.autoreview_prepared(id, Err("gateway unavailable".into())));
        assert!(app.delegation.autoreview.flight.is_none());
        assert!(app.delegation.backlog.flight.is_none());
        assert_eq!(
            Queue::open(path, "fixture")
                .unwrap()
                .journal
                .pending_reviews,
            vec![request]
        );
        assert!(app.delegation.autoreview.status.contains("retrying"));
    }
}

#[derive(Debug, Clone)]
pub(in super::super) struct Prepared {
    request: ReviewRequest,
    review: Option<serde_json::Value>,
}

impl Desktop {
    pub(crate) fn autoreview_tick(&mut self) -> Task<Message> {
        if self.server_schedules()
            || !self.preferences.current.autoreview
            || self.delegation.autoreview.flight.is_some()
            || self
                .delegation
                .autoreview
                .retry_at
                .is_some_and(|at| at.elapsed() < Duration::from_secs(30))
        {
            return Task::none();
        }
        let Some(request) = self
            .delegation
            .queue
            .as_ref()
            .and_then(|queue| queue.journal.pending_reviews.first())
            .cloned()
        else {
            return Task::none();
        };
        let Ok(gateway) = self.preferences.current.gateway() else {
            return Task::none();
        };
        let id = Uuid::new_v4();
        let epoch = self.delegation.epoch;
        self.delegation.autoreview.flight = Some(id);
        Task::perform(
            async move {
                let result = gateway
                    .enqueue_completion_review(&request.project, request.task)
                    .await
                    .map(|review| Some(Box::new(Prepared { request, review })))
                    .map_err(|error| error.to_string());
                Event::AutoreviewPrepared(id, result)
            },
            move |event| Message::Delegation(epoch, event),
        )
    }

    pub(super) fn autoreview_prepared(
        &mut self,
        id: Uuid,
        result: Result<Option<Box<Prepared>>, String>,
    ) -> Task<Message> {
        if self.delegation.autoreview.flight != Some(id) {
            return Task::none();
        }
        self.delegation.autoreview.flight = None;
        match result {
            Ok(Some(prepared)) => {
                if let Some(queue) = &mut self.delegation.queue
                    && let Err(error) = queue.finish_review(&prepared.request)
                {
                    self.delegation.fail(error);
                    return Task::none();
                }
                self.delegation.autoreview.retry_at = None;
                self.delegation.autoreview.status = match prepared.review {
                    Some(review) => format!(
                        "Review queued: {}",
                        review["id"].as_str().unwrap_or("confirmed")
                    ),
                    None => "No additional review needed for this completion".into(),
                };
                self.queue_diagnostic(self.delegation.autoreview.status.clone());
                return self.refresh_task_project(&prepared.request.project);
            }
            Ok(None) => {}
            Err(error) => {
                self.delegation.autoreview.retry_at = Some(Instant::now());
                self.delegation.autoreview.status =
                    format!("Review creation pending; retrying in 30s: {error}");
                self.queue_diagnostic(self.delegation.autoreview.status.clone());
            }
        }
        Task::none()
    }
}
