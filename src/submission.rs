//! Native input-path coordination. A submission never claims a task or reports start.
use crate::{delivery::DeliveryLedger, readiness::ReadinessGate, recipient::TerminalRecipient};
use anyhow::{Result, bail};
use iced_term::Terminal;
use std::time::Instant;
use uuid::Uuid;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Submission {
    Deferred,
    Enqueued,
}

/// The caller supplies compact text from an authoritative task query, not raw gateway
/// notification bodies. Control characters and non-ASCII input are deliberately excluded.
pub fn submit_literal(
    ledger: &mut DeliveryLedger,
    gate: &mut ReadinessGate,
    terminal: &Terminal,
    recipient: &mut TerminalRecipient,
    id: Uuid,
    text: &str,
    now_ms: u64,
) -> Result<Submission> {
    if text.is_empty()
        || text.len() > 1024
        || !text
            .bytes()
            .all(|byte| byte == b' ' || byte.is_ascii_graphic())
    {
        bail!("reminder must be one compact ASCII line without terminal controls");
    }
    let Some(proof) = gate.observe(terminal, recipient, Instant::now()) else {
        return Ok(Submission::Deferred);
    };
    if !proof.is_current(terminal, recipient, Instant::now()) {
        return Ok(Submission::Deferred);
    }
    let attempt = ledger.begin_attempt(id, terminal.id, recipient, proof.checkpoint(), now_ms)?;
    if !proof.is_current(terminal, recipient, Instant::now()) {
        // Persistence may have taken time. No input API has been touched yet.
        ledger.cancel_before_write(attempt)?;
        return Ok(Submission::Deferred);
    }
    let bytes = format!(
        "\x1b[200~<Gateway message>[amux delivery:{id}] {text}</Gateway message>\x1b[201~\r"
    )
    .into_bytes();
    let enqueued = terminal.try_enqueue_input(bytes);
    // Even an error can follow channel acceptance. Block other reminders until
    // the unique marker is observed; never retry this uncertain attempt blindly.
    recipient.note_input();
    gate.note_input_attempt(id);
    enqueued?;
    ledger.mark_enqueued(attempt)?;
    Ok(Submission::Enqueued)
}
