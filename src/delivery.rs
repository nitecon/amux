//! Durable reminder deduplication. This ledger never establishes prompt readiness.
use crate::recipient::{RecipientAddress, RecipientCheckpoint, TerminalRecipient};
use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};
use std::{
    collections::HashSet,
    fs::File,
    io::{Read, Write},
    path::{Path, PathBuf},
};
use uuid::Uuid;

const MAX_BYTES: usize = 2 * 1024 * 1024;
const MAX_RECORDS: usize = 512;
const FRESH_MS: u64 = 5 * 60 * 1000;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub enum DeliveryCause {
    /// Timestamp is the completion time of an authoritative pending-task query.
    RecoveryQuery,
    /// Timestamp is the original event time, never its reconnect arrival time.
    Notification { event_id: Uuid },
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum DeliveryState {
    Pending,
    /// Persisted before any input attempt; never automatically retry.
    Uncertain,
    /// The input channel accepted bytes; agent understanding remains unknown.
    Enqueued,
    Retired,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DeliveryRecord {
    pub id: Uuid,
    pub task_id: Uuid,
    pub recipient: RecipientAddress,
    pub cause: DeliveryCause,
    pub observed_at_ms: u64,
    pub state: DeliveryState,
}

#[derive(Clone, Serialize, Deserialize)]
struct Snapshot {
    version: u32,
    records: Vec<DeliveryRecord>,
}

impl DeliveryRecord {
    fn same_intent(&self, task: Uuid, recipient: &RecipientAddress, cause: &DeliveryCause) -> bool {
        self.recipient == *recipient
            && self.cause == *cause
            && (!matches!(cause, DeliveryCause::RecoveryQuery) || self.task_id == task)
    }
}

/// One desktop owns a ledger at a time. Other processes fail closed rather than race.
pub struct DeliveryLedger {
    path: PathBuf,
    _lock: File,
    snapshot: Snapshot,
    poisoned: bool,
    invocation: Uuid,
}

/// A durable attempt fence. It is not consent, readiness or a write capability.
pub struct DeliveryAttempt {
    id: Uuid,
    invocation: Uuid,
}

impl DeliveryLedger {
    pub fn open(path: &Path) -> Result<Self> {
        let directory = path.parent().context("delivery ledger has no directory")?;
        std::fs::create_dir_all(directory)?;
        let lock = std::fs::OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(path.with_extension("lock"))?;
        lock.try_lock()
            .context("delivery ledger is already owned by another process")?;
        let snapshot = match File::open(path) {
            Ok(file) => {
                let mut bytes = Vec::new();
                file.take(MAX_BYTES as u64 + 1).read_to_end(&mut bytes)?;
                if bytes.len() > MAX_BYTES {
                    bail!("delivery ledger exceeds 2 MiB");
                }
                serde_json::from_slice::<Snapshot>(&bytes)
                    .context("invalid delivery ledger; original preserved")?
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Snapshot {
                version: 1,
                records: Vec::new(),
            },
            Err(error) => return Err(error.into()),
        };
        let mut ledger = Self {
            path: path.into(),
            _lock: lock,
            snapshot,
            poisoned: false,
            invocation: Uuid::new_v4(),
        };
        ledger.validate()?;
        // A new desktop never resumes old queued input. Accepted gateway tasks remain intact.
        let mut changed = false;
        for record in &mut ledger.snapshot.records {
            if record.state == DeliveryState::Pending {
                record.state = DeliveryState::Retired;
                changed = true;
            }
        }
        if changed {
            ledger.persist()?;
        }
        Ok(ledger)
    }

    fn validate(&self) -> Result<()> {
        if self.snapshot.version != 1 || self.snapshot.records.len() > MAX_RECORDS {
            bail!("invalid delivery ledger version or record count");
        }
        let mut ids = HashSet::new();
        for (index, record) in self.snapshot.records.iter().enumerate() {
            record.recipient.validate()?;
            if record.id.is_nil()
                || record.task_id.is_nil()
                || !ids.insert(record.id)
                || matches!(record.cause, DeliveryCause::Notification { event_id } if event_id.is_nil())
                || self.snapshot.records[..index].iter().any(|prior| {
                    prior.same_intent(record.task_id, &record.recipient, &record.cause)
                })
            {
                bail!("invalid or duplicate reminder identity");
            }
        }
        Ok(())
    }

    fn healthy(&self) -> Result<()> {
        if self.poisoned {
            bail!("delivery storage failed; terminal delivery is disabled for this invocation");
        }
        Ok(())
    }

    fn persist(&mut self) -> Result<()> {
        self.healthy()?;
        let result = (|| -> Result<()> {
            self.validate()?;
            let bytes = serde_json::to_vec(&self.snapshot)?;
            if bytes.len() > MAX_BYTES {
                bail!("delivery ledger exceeds 2 MiB");
            }
            let directory = self.path.parent().unwrap();
            let mut file = tempfile::NamedTempFile::new_in(directory)?;
            file.write_all(&bytes)?;
            file.as_file().sync_all()?;
            file.persist(&self.path).map_err(|error| error.error)?;
            #[cfg(unix)]
            File::open(directory)?.sync_all()?;
            Ok(())
        })();
        if result.is_err() {
            self.poisoned = true;
        }
        result
    }

    pub fn records(&self) -> &[DeliveryRecord] {
        &self.snapshot.records
    }

    /// Only use fresh authoritative query results or original transient-event timestamps.
    /// Repeated queries preserve ID; they may refresh a still-pending recovery observation.
    pub fn queue(
        &mut self,
        task: Uuid,
        recipient: RecipientAddress,
        cause: DeliveryCause,
        observed_at_ms: u64,
        now_ms: u64,
    ) -> Result<Uuid> {
        self.healthy()?;
        recipient.validate()?;
        if task.is_nil()
            || !fresh(observed_at_ms, now_ms)
            || matches!(cause, DeliveryCause::Notification { event_id } if event_id.is_nil())
        {
            bail!("invalid or stale reminder source");
        }
        if let Some(record) = self
            .snapshot
            .records
            .iter_mut()
            .find(|record| record.same_intent(task, &recipient, &cause))
        {
            if record.task_id != task {
                bail!("notification identity reused for a different task");
            }
            let id = record.id;
            if record.state == DeliveryState::Pending
                && cause == DeliveryCause::RecoveryQuery
                && observed_at_ms > record.observed_at_ms
            {
                record.observed_at_ms = observed_at_ms;
                self.persist()?;
            }
            return Ok(id);
        }
        if self.snapshot.records.len() >= MAX_RECORDS {
            bail!("delivery ledger is full; existing deduplication fences retained");
        }
        let id = Uuid::new_v4();
        self.snapshot.records.push(DeliveryRecord {
            id,
            task_id: task,
            recipient,
            cause,
            observed_at_ms,
            state: DeliveryState::Pending,
        });
        self.persist()?;
        Ok(id)
    }

    pub fn retire_recipient(&mut self, recipient: &RecipientAddress) -> Result<()> {
        self.healthy()?;
        let mut changed = false;
        for record in &mut self.snapshot.records {
            if record.recipient == *recipient && record.state == DeliveryState::Pending {
                record.state = DeliveryState::Retired;
                changed = true;
            }
        }
        if changed {
            self.persist()?;
        }
        Ok(())
    }

    /// Terminal IDs are unique within a desktop invocation. Cold open already
    /// retires pending records from older invocations; never alter attempt history.
    pub fn retire_terminal(&mut self, terminal_id: u64) -> Result<()> {
        self.healthy()?;
        let mut changed = false;
        for record in &mut self.snapshot.records {
            if record.recipient.terminal_id() == terminal_id
                && record.state == DeliveryState::Pending
            {
                record.state = DeliveryState::Retired;
                changed = true;
            }
        }
        if changed {
            self.persist()?;
        }
        Ok(())
    }

    pub fn retire_pending(&mut self, id: Uuid) -> Result<()> {
        self.healthy()?;
        if let Some(record) = self
            .snapshot
            .records
            .iter_mut()
            .find(|record| record.id == id && record.state == DeliveryState::Pending)
        {
            record.state = DeliveryState::Retired;
            self.persist()?;
        }
        Ok(())
    }

    /// Call only after a separate native readiness check. No terminal input occurs here.
    pub fn begin_attempt(
        &mut self,
        id: Uuid,
        terminal_id: u64,
        recipient: &TerminalRecipient,
        checkpoint: &RecipientCheckpoint,
        now_ms: u64,
    ) -> Result<DeliveryAttempt> {
        self.healthy()?;
        let record = self
            .snapshot
            .records
            .iter_mut()
            .find(|record| record.id == id)
            .context("unknown reminder")?;
        if record.state != DeliveryState::Pending
            || !fresh(record.observed_at_ms, now_ms)
            || record.recipient.terminal_id() != terminal_id
            || !recipient.matches_address(&record.recipient)
            || !recipient.is_current(checkpoint)
        {
            bail!(
                "reminder attempt deferred: stale, retired, already attempted, or recipient/consent/input fence changed"
            );
        }
        record.state = DeliveryState::Uncertain;
        self.persist()?;
        Ok(DeliveryAttempt {
            id,
            invocation: self.invocation,
        })
    }

    /// Use only after the exact native input channel accepted the complete submission.
    pub fn mark_enqueued(&mut self, attempt: DeliveryAttempt) -> Result<()> {
        self.healthy()?;
        if attempt.invocation != self.invocation {
            bail!("attempt belongs to a different ledger invocation");
        }
        let record = self
            .snapshot
            .records
            .iter_mut()
            .find(|record| record.id == attempt.id)
            .context("unknown reminder attempt")?;
        if record.state != DeliveryState::Uncertain {
            bail!("reminder is not an uncertain attempt");
        }
        record.state = DeliveryState::Enqueued;
        self.persist()
    }

    /// Only the submission coordinator calls this, before touching the input channel.
    #[cfg(feature = "desktop")]
    pub(crate) fn cancel_before_write(&mut self, attempt: DeliveryAttempt) -> Result<()> {
        self.healthy()?;
        if attempt.invocation != self.invocation {
            bail!("attempt belongs to a different ledger invocation");
        }
        let record = self
            .snapshot
            .records
            .iter_mut()
            .find(|record| record.id == attempt.id)
            .context("unknown reminder attempt")?;
        if record.state != DeliveryState::Uncertain {
            bail!("reminder is not an uncertain attempt");
        }
        record.state = DeliveryState::Pending;
        self.persist()
    }
}

fn fresh(observed: u64, now: u64) -> bool {
    now.checked_sub(observed).is_some_and(|age| age <= FRESH_MS)
}
