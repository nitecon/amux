//! Local desktop notifications. A delivery attempt is never task receipt, claim
//! or execution start. One worker thread owns every attempt, so a missing, slow
//! or failing notification service cannot block the caller.
use std::sync::mpsc::{self, Receiver, SyncSender, TrySendError};

/// Pending notifications beyond this are dropped, never queued without bound.
pub const QUEUE: usize = 16;
pub const MAX_TITLE: usize = 120;
pub const MAX_BODY: usize = 1000;

/// Shows one notification. Errors are reported, never retried.
pub trait Backend: Send + 'static {
    fn show(&mut self, title: &str, body: &str) -> Result<(), String>;
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Outcome {
    /// The OS notification service accepted the request. It does not prove the
    /// user saw it.
    Shown {
        id: u64,
    },
    Failed {
        id: u64,
        reason: String,
    },
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Dropped {
    EmptyTitle,
    QueueFull,
    WorkerStopped,
}

struct Job {
    id: u64,
    title: String,
    body: String,
}

pub struct Notifier {
    jobs: SyncSender<Job>,
    outcomes: Receiver<Outcome>,
    next: u64,
}

impl Notifier {
    pub fn new(mut backend: impl Backend) -> Self {
        let (jobs, queue) = mpsc::sync_channel::<Job>(QUEUE);
        let (report, outcomes) = mpsc::sync_channel(QUEUE);
        std::thread::Builder::new()
            .name("amux-notifications".into())
            .spawn(move || {
                for job in queue {
                    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                        backend.show(&job.title, &job.body)
                    }));
                    let outcome = match result {
                        Ok(Ok(())) => Outcome::Shown { id: job.id },
                        Ok(Err(reason)) => Outcome::Failed { id: job.id, reason },
                        Err(_) => Outcome::Failed {
                            id: job.id,
                            reason: "notification backend panicked".into(),
                        },
                    };
                    // Unread outcomes are dropped rather than stalling delivery.
                    let _ = report.try_send(outcome);
                }
            })
            .expect("spawn notification worker");
        Self {
            jobs,
            outcomes,
            next: 1,
        }
    }

    /// The platform's desktop notification service.
    #[cfg(feature = "desktop")]
    pub fn native() -> Self {
        Self::new(Native)
    }

    /// Queues one attempt and returns its ID without waiting for delivery.
    pub fn notify(&mut self, title: &str, body: &str) -> Result<u64, Dropped> {
        let title = clean(title, MAX_TITLE, false);
        if title.is_empty() {
            return Err(Dropped::EmptyTitle);
        }
        let id = self.next;
        let job = Job {
            id,
            title,
            body: clean(body, MAX_BODY, true),
        };
        match self.jobs.try_send(job) {
            Ok(()) => {
                self.next += 1;
                Ok(id)
            }
            Err(TrySendError::Full(_)) => Err(Dropped::QueueFull),
            Err(TrySendError::Disconnected(_)) => Err(Dropped::WorkerStopped),
        }
    }

    /// Outcomes finished so far, without blocking.
    pub fn outcomes(&self) -> Vec<Outcome> {
        self.outcomes.try_iter().collect()
    }
}

/// Drops control characters (keeping body line breaks) and truncates to `max`
/// characters, marking the cut.
fn clean(text: &str, max: usize, lines: bool) -> String {
    let text: String = text
        .chars()
        .map(|c| match c {
            '\n' if lines => '\n',
            '\n' | '\t' => ' ',
            c => c,
        })
        .filter(|c| *c == '\n' || !c.is_control())
        .collect();
    let text = text.trim();
    if text.chars().count() <= max {
        return text.to_owned();
    }
    let mut cut: String = text.chars().take(max - 1).collect();
    cut.push('…');
    cut
}

/// Linux: the freedesktop D-Bus notification service. Windows: a toast under
/// the PowerShell AppUserModelID, since a portable executable has no registered
/// identity of its own.
#[cfg(feature = "desktop")]
struct Native;

#[cfg(feature = "desktop")]
impl Backend for Native {
    fn show(&mut self, title: &str, body: &str) -> Result<(), String> {
        notify_rust::Notification::new()
            .appname("amux")
            .summary(title)
            .body(body)
            .show()
            .map(|_| ())
            .map_err(|e| e.to_string())
    }
}
