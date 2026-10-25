//! Durable lifecycle routing. Native prompt observation and input remain on the UI thread.
use crate::delegation::{
    Event, Journal, MAX_PENDING, MAX_RECEIPTS, Outcome, Receipt, Recipient, Rejected, Target,
};
use anyhow::{Result, bail, ensure};
use serde_json::{Value, json};
use std::{
    collections::{HashMap, HashSet},
    path::PathBuf,
};
use uuid::Uuid;

pub struct Queue {
    pub journal: Journal,
    path: PathBuf,
    sent: HashMap<i64, (String, String)>,
    replay_through: i64,
}

#[derive(Debug, Clone)]
pub struct Attempt {
    pub event: Event,
    pub target: Target,
}

fn awaits_assignment(event: &Event) -> bool {
    event.state() == "todo" && event.owner.is_none() && event.owner_agent_id.is_none()
}

impl Queue {
    pub fn begin_backlog(&mut self, receipt: crate::backlog::Receipt) -> Result<()> {
        ensure!(
            !self
                .journal
                .backlog
                .iter()
                .any(|r| r.task == receipt.task && r.revision == receipt.revision),
            "Backlog task already attempted"
        );
        let mut next = self.journal.clone();
        next.backlog.push(receipt);
        self.commit(next)
    }

    pub fn finish_backlog(&mut self, task: Uuid, revision: i64, outcome: Outcome) -> Result<()> {
        let mut next = self.journal.clone();
        let receipt = next
            .backlog
            .iter_mut()
            .find(|r| r.task == task && r.revision == revision)
            .ok_or_else(|| anyhow::anyhow!("Backlog receipt missing"))?;
        receipt.outcome = outcome;
        self.commit(next)
    }

    /// Only an authoritative closure or explicit requeue can retire a backlog fence.
    pub fn reconcile_backlog(&mut self, project: &str, tasks: &[Value]) -> Result<()> {
        let mut next = self.journal.clone();
        next.backlog.retain(|r| {
            r.target.project != project
                || !tasks.iter().any(|task| {
                    task["id"] == r.task.to_string()
                        && r.outcome != Outcome::Submitting
                        && (matches!(task["status"].as_str(), Some("done" | "cancelled"))
                            || (r.outcome != Outcome::Uncertain
                                && task["status"] == "todo"
                                && crate::backlog::revision(task) != r.revision))
                })
        });
        if next.backlog.len() != self.journal.backlog.len() {
            self.commit(next)?;
        }
        Ok(())
    }

    pub fn open(path: PathBuf, namespace: &str) -> Result<Self> {
        let journal = Journal::load(&path, namespace)?;
        journal.save(&path)?;
        Ok(Self {
            journal,
            path,
            sent: HashMap::new(),
            replay_through: i64::MAX,
        })
    }

    // Commit the next state before exposing any permission to send native input.
    fn commit(&mut self, next: Journal) -> Result<()> {
        next.save(&self.path)?;
        self.journal = next;
        Ok(())
    }

    pub fn subscribed(&mut self, cursor: i64, replay_through: i64) -> Result<()> {
        ensure!(
            cursor >= 0
                && replay_through >= cursor
                && self.journal.cursor.is_none_or(|c| cursor >= c),
            "Invalid subscription cursor"
        );
        let mut next = self.journal.clone();
        next.cursor = Some(cursor);
        self.commit(next)?;
        self.replay_through = replay_through;
        self.sent.clear();
        Ok(())
    }

    pub fn disconnected(&mut self) {
        self.sent.clear();
        self.replay_through = i64::MAX;
    }

    pub fn reject(&mut self, id: i64, reason: &str) -> Result<()> {
        ensure!(
            id > 0 && reason.len() <= 512,
            "Invalid rejected lifecycle event"
        );
        if self.journal.receipts.iter().any(|r| r.event.id == id)
            || self.journal.rejected.iter().any(|r| r.id == id)
        {
            return Ok(());
        }
        let mut next = self.journal.clone();
        next.rejected.push(Rejected {
            id,
            reason: reason.into(),
            confirmed: false,
        });
        self.commit(next)
    }

    pub fn receive(
        &mut self,
        event: Event,
        candidates: &[Target],
        approved: bool,
        now: i64,
    ) -> Result<()> {
        if self.journal.receipts.iter().any(|r| r.event.id == event.id)
            || self.journal.rejected.iter().any(|r| r.id == event.id)
        {
            return Ok(());
        }
        let mut next = self.journal.clone();
        // Never evict a pending or unconfirmed fence to make room for input.
        next.receipts.retain(|r| {
            !(r.confirmed && r.terminal() && next.cursor.is_some_and(|c| c >= r.event.id))
        });
        next.rejected
            .retain(|r| !(r.confirmed && next.cursor.is_some_and(|c| c >= r.id)));
        let rows: usize = next
            .receipts
            .iter()
            .map(|r| r.recipients.len().max(1))
            .sum::<usize>()
            + next.rejected.len();
        ensure!(
            rows < MAX_RECEIPTS,
            "Delivery journal full; notifications paused"
        );
        let pending: usize = next
            .receipts
            .iter()
            .flat_map(|r| &r.recipients)
            .filter(|r| !r.outcome.terminal())
            .count();
        next.recent.push(event.clone());
        if next.recent.len() > crate::delegation::MAX_RECENT_EVENTS {
            next.recent.remove(0);
        }
        let mut receipt = Receipt {
            event,
            recipients: Vec::new(),
            outcome: Outcome::Received,
            reason: String::new(),
            confirmed: false,
        };
        let skip = if receipt.event.id <= self.replay_through {
            Some("Offline history is not sent to terminals")
        } else if !receipt.event.fresh(now) {
            Some("Event expired or has no valid timestamp")
        } else if !approved {
            Some("Automatic terminal notifications are disabled")
        } else if awaits_assignment(&receipt.event) {
            Some("Queued for idle-agent assignment; only the claimed agent receives work")
        } else if candidates.is_empty() {
            Some("No running agent at event arrival")
        } else {
            None
        };
        if let Some(reason) = skip {
            receipt.outcome = Outcome::Skipped;
            receipt.reason = reason.into();
        } else if candidates.len() > MAX_PENDING
            || pending + candidates.len() > MAX_PENDING
            || rows + candidates.len() > MAX_RECEIPTS
        {
            receipt.outcome = Outcome::Failed;
            receipt.reason = "Delivery queue capacity reached".into();
        } else {
            let mut seen = HashSet::new();
            for target in candidates {
                ensure!(seen.insert(target.session), "Duplicate recipient session");
                let outcome = if target.project.is_empty() {
                    Outcome::Received
                } else if target.accepts(&receipt.event) {
                    Outcome::Queued
                } else {
                    Outcome::Skipped
                };
                receipt.recipients.push(Recipient {
                    target: target.clone(),
                    outcome,
                    reason: if outcome == Outcome::Received {
                        "Waiting for repository metadata"
                    } else if outcome == Outcome::Queued {
                        "Waiting for terminal input availability"
                    } else {
                        target
                            .mismatch_reason(&receipt.event)
                            .unwrap_or("Recipient is not eligible")
                    }
                    .into(),
                });
            }
        }
        next.receipts.push(receipt);
        self.commit(next)
    }

    /// Refresh only arrival-time attachments. A later agent can never inherit pending work.
    pub fn refresh(&mut self, targets: &[Target], approved: bool, now: i64) -> Result<()> {
        let mut next = self.journal.clone();
        let mut changed = false;
        for receipt in &mut next.receipts {
            for recipient in &mut receipt.recipients {
                if recipient.outcome.terminal() {
                    continue;
                }
                let current = targets.iter().find(|t| recipient.target.same_attachment(t));
                let reason = if !approved {
                    Some("Automatic terminal notifications disabled")
                } else if awaits_assignment(&receipt.event) {
                    Some("Queued for idle-agent assignment; only the claimed agent receives work")
                } else if !receipt.event.fresh(now) {
                    Some("Event expired while waiting")
                } else if current.is_none() {
                    Some("Agent attachment changed or exited")
                } else {
                    None
                };
                if let Some(reason) = reason {
                    recipient.outcome = if recipient.outcome == Outcome::Submitting {
                        Outcome::Uncertain
                    } else {
                        Outcome::Skipped
                    };
                    recipient.reason = reason.into();
                    receipt.confirmed = false;
                    changed = true;
                    continue;
                }
                let current = current.expect("checked attachment");
                // Named selections and a previously known origin cannot change underneath queued input.
                if (!recipient.target.project.is_empty()
                    && recipient.target.project != current.project)
                    || (receipt.event.goal.is_some() && recipient.target.goal != current.goal)
                    || recipient
                        .target
                        .origin
                        .as_ref()
                        .is_some_and(|o| current.origin.as_ref() != Some(o))
                {
                    recipient.outcome = Outcome::Skipped;
                    recipient.reason = "Repository or actor selection changed".into();
                    receipt.confirmed = false;
                    changed = true;
                } else if recipient.outcome == Outcome::Received && !current.project.is_empty() {
                    recipient.target = current.clone();
                    recipient.outcome = if current.accepts(&receipt.event) {
                        Outcome::Queued
                    } else {
                        Outcome::Skipped
                    };
                    recipient.reason = if recipient.outcome == Outcome::Queued {
                        "Waiting for terminal input availability"
                    } else {
                        current
                            .mismatch_reason(&receipt.event)
                            .unwrap_or("Recipient is not eligible")
                    }
                    .into();
                    receipt.confirmed = false;
                    changed = true;
                } else if recipient.outcome == Outcome::Queued && !current.accepts(&receipt.event) {
                    recipient.outcome = Outcome::Skipped;
                    recipient.reason = "Recipient no longer matches the event".into();
                    receipt.confirmed = false;
                    changed = true;
                } else if recipient.outcome == Outcome::Queued && recipient.target != *current {
                    recipient.target = current.clone();
                    receipt.confirmed = false;
                    changed = true;
                }
            }
        }
        if changed {
            self.commit(next)?;
        }
        Ok(())
    }

    /// FIFO per session, while a busy session does not block other ready peers.
    pub fn next(&self, ready: &HashSet<Uuid>) -> Option<Attempt> {
        let mut visited = HashSet::new();
        for receipt in &self.journal.receipts {
            for recipient in &receipt.recipients {
                if recipient.outcome.terminal() || !visited.insert(recipient.target.session) {
                    continue;
                }
                if recipient.outcome == Outcome::Queued && ready.contains(&recipient.target.session)
                {
                    return Some(Attempt {
                        event: receipt.event.clone(),
                        target: recipient.target.clone(),
                    });
                }
            }
        }
        None
    }

    pub fn begin(&mut self, attempt: &Attempt) -> Result<()> {
        let mut next = self.journal.clone();
        let receipt = next
            .receipts
            .iter_mut()
            .find(|r| r.event == attempt.event)
            .ok_or_else(|| anyhow::anyhow!("Delivery event changed"))?;
        let recipient = receipt
            .recipients
            .iter_mut()
            .find(|r| r.target == attempt.target)
            .ok_or_else(|| anyhow::anyhow!("Delivery attachment changed"))?;
        ensure!(
            recipient.outcome == Outcome::Queued,
            "Delivery is no longer queued"
        );
        recipient.outcome = Outcome::Submitting;
        recipient.reason = "Native input submission in progress".into();
        receipt.confirmed = false;
        self.commit(next)
    }

    pub fn submitting(&self, attempt: &Attempt) -> bool {
        self.journal
            .receipts
            .iter()
            .find(|r| r.event == attempt.event)
            .is_some_and(|r| {
                r.recipients
                    .iter()
                    .any(|r| r.target == attempt.target && r.outcome == Outcome::Submitting)
            })
    }

    /// Only a known-not-written UI deferral may return to queued. Lost/ambiguous results are terminal.
    pub fn finish(&mut self, attempt: &Attempt, outcome: Outcome) -> Result<()> {
        ensure!(
            matches!(
                outcome,
                Outcome::Submitted | Outcome::Queued | Outcome::Uncertain | Outcome::Skipped
            ),
            "Invalid native submission result"
        );
        let mut next = self.journal.clone();
        let receipt = next
            .receipts
            .iter_mut()
            .find(|r| r.event.id == attempt.event.id)
            .ok_or_else(|| anyhow::anyhow!("Unknown delivery event"))?;
        let recipient = receipt
            .recipients
            .iter_mut()
            .find(|r| r.target == attempt.target)
            .ok_or_else(|| anyhow::anyhow!("Unknown delivery target"))?;
        ensure!(
            recipient.outcome == Outcome::Submitting,
            "Delivery has no durable submitting fence"
        );
        recipient.outcome = outcome;
        recipient.reason = match outcome {
            Outcome::Submitted => {
                "Submitted to native input; agent receipt and execution remain unknown"
            }
            Outcome::Queued => "Terminal input changed before any input was sent; waiting",
            Outcome::Skipped => "Agent attachment changed before input",
            _ => "Input outcome uncertain; automatic replay disabled",
        }
        .into();
        receipt.confirmed = false;
        self.commit(next)
    }

    /// At most one outstanding snapshot per event, because protocol v1 records carry no nonce.
    pub fn acknowledgments(&mut self) -> Vec<Value> {
        let mut frames = Vec::new();
        for receipt in &self.journal.receipts {
            if frames.len() >= 32 {
                break;
            }
            if receipt.confirmed || self.sent.contains_key(&receipt.event.id) {
                continue;
            }
            let one = (receipt.recipients.len() == 1).then(|| &receipt.recipients[0].target);
            let summary = (!receipt.recipients.is_empty()).then(|| json!({"recipients":receipt.recipients.iter().map(|r| json!({"session_id":r.target.session,"surface_id":r.target.terminal.to_string(),"status":r.outcome.wire()})).collect::<Vec<_>>()} ).to_string());
            frames.push(json!({"type":"ack","event_id":receipt.event.id,"status":receipt.status(),"workspace_id":one.map(|t|t.workspace),"surface_id":one.map(|t|t.terminal.to_string()),"message":if receipt.recipients.len()==1 {receipt.recipients[0].reason.as_str()} else {receipt.reason.as_str()},"summary":summary}));
            self.sent.insert(
                receipt.event.id,
                (receipt.status().into(), receipt.signature()),
            );
        }
        for rejection in &self.journal.rejected {
            if frames.len() >= 32 {
                break;
            }
            if rejection.confirmed || self.sent.contains_key(&rejection.id) {
                continue;
            }
            frames.push(json!({"type":"ack","event_id":rejection.id,"status":"skipped","workspace_id":null,"surface_id":null,"message":rejection.reason,"summary":null}));
            self.sent
                .insert(rejection.id, ("skipped".into(), "rejected".into()));
        }
        frames
    }

    pub fn recorded(&mut self, id: i64, status: &str) -> Result<()> {
        let terminal = match status {
            "injected" => Some(Outcome::Submitted),
            "skipped" => Some(Outcome::Skipped),
            "failed" => Some(Outcome::Failed),
            "uncertain" => Some(Outcome::Uncertain),
            "received" | "queued" => None,
            _ => bail!("Invalid gateway delivery status"),
        };
        let sent = self.sent.remove(&id);
        let mut next = self.journal.clone();
        if let Some(rejection) = next.rejected.iter_mut().find(|r| r.id == id) {
            rejection.confirmed = terminal.is_some();
            next.cursor = Some(next.cursor.unwrap_or(0).max(id));
            return self.commit(next);
        }
        if !next.receipts.iter().any(|r| r.event.id == id) && next.cursor.is_some_and(|c| id <= c) {
            return Ok(());
        }
        let receipt = next
            .receipts
            .iter_mut()
            .find(|r| r.event.id == id)
            .ok_or_else(|| anyhow::anyhow!("Unknown gateway receipt"))?;
        if sent
            .as_ref()
            .is_some_and(|(s, signature)| s == status && signature == &receipt.signature())
        {
            receipt.confirmed = true;
        } else if let Some(outcome) = terminal {
            // The server retained an earlier terminal outcome; retire local pending work.
            receipt.outcome = outcome;
            receipt.reason =
                "Gateway retained a terminal outcome; automatic replay disabled".into();
            for recipient in &mut receipt.recipients {
                recipient.outcome = outcome;
                recipient.reason = receipt.reason.clone();
            }
            receipt.confirmed = true;
        }
        next.cursor = Some(next.cursor.unwrap_or(0).max(id));
        self.commit(next)
    }
}
