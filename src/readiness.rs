//! Provisional Linux/Codex 0.163.0 prompt profile. No input or acknowledgment occurs here.
//! Idle and partial-input checks are exercised locally; busy/continuation acceptance
//! remains required before connecting this profile to automatic reminder submission.
use crate::{
    identity::Provider,
    recipient::{NativeInputEvidence, RecipientAddress, RecipientCheckpoint, TerminalRecipient},
};
use iced_term::{
    NamedColor, TermMode, Terminal, TerminalCellFlags, TerminalColor, TerminalObservation,
};
use std::time::{Duration, Instant};

const SETTLE: Duration = Duration::from_millis(500);
const PROOF_AGE: Duration = Duration::from_millis(100);
const PLACEHOLDER: &str = "Ask Codex to do anything";

struct Candidate {
    address: RecipientAddress,
    image: NativeInputEvidence,
    screen: TerminalObservation,
    checkpoint: RecipientCheckpoint,
    since: Instant,
}

#[derive(Default)]
pub struct ReadinessGate {
    known_executor: Option<(RecipientAddress, NativeInputEvidence)>,
    candidate: Option<Candidate>,
    invalidated: bool,
}

/// Ephemeral native checks, not a delivery or an acknowledgment. Recheck immediately
/// before a write after the durable attempt fence, since persistence can take time.
pub struct ReadyPrompt {
    address: RecipientAddress,
    image: NativeInputEvidence,
    screen: TerminalObservation,
    checkpoint: RecipientCheckpoint,
    observed: Instant,
}

impl ReadinessGate {
    pub fn observe(
        &mut self,
        terminal: &Terminal,
        recipient: &TerminalRecipient,
        now: Instant,
    ) -> Option<ReadyPrompt> {
        // Any failed observation discards the settling interval, including a
        // temporary foreground change with an otherwise unchanged screen.
        let previous = self.candidate.take();
        if self.invalidated {
            return None;
        }
        let address = recipient.address(terminal.id)?;
        if address.identity().provider != Provider::Codex {
            self.candidate = None;
            return None;
        }
        let image = recipient.native_input_evidence(terminal)?;
        if self
            .known_executor
            .as_ref()
            .is_some_and(|known| known != &(address.clone(), image))
        {
            // An observed native executable/recipient replacement cannot inherit
            // this profile, even if it later redraws an identical old header.
            self.invalidated = true;
            return None;
        }
        let screen = terminal.observation(32_768)?;
        let headers: Vec<_> = screen
            .lines
            .iter()
            .take(4)
            .filter(|line| line.trim_start().starts_with(">_ OpenAI Codex (v"))
            .collect();
        if !headers.is_empty() {
            if headers.len() != 1 || headers[0].trim() != ">_ OpenAI Codex (v0.163.0)" {
                self.known_executor = None;
                self.candidate = None;
                self.invalidated = true;
                return None;
            }
            self.known_executor = Some((address.clone(), image));
        }
        if self.known_executor.as_ref() != Some(&(address.clone(), image))
            || !empty_codex_prompt(&screen)
        {
            self.candidate = None;
            return None;
        }
        let checkpoint = recipient.checkpoint()?;
        let stable = previous.as_ref().is_some_and(|previous| {
            previous.address == address
                && previous.image == image
                && previous.screen == screen
                && recipient.is_current(&previous.checkpoint)
        });
        if !stable {
            self.candidate = Some(Candidate {
                address,
                image,
                screen,
                checkpoint,
                since: now,
            });
            return None;
        }
        let since = previous.as_ref()?.since;
        self.candidate = previous;
        if now.checked_duration_since(since)? < SETTLE {
            return None;
        }
        Some(ReadyPrompt {
            address,
            image,
            screen,
            checkpoint,
            observed: now,
        })
    }
}

impl ReadyPrompt {
    pub fn checkpoint(&self) -> &RecipientCheckpoint {
        &self.checkpoint
    }

    pub fn is_current(
        &self,
        terminal: &Terminal,
        recipient: &TerminalRecipient,
        now: Instant,
    ) -> bool {
        now.checked_duration_since(self.observed)
            .is_some_and(|age| age <= PROOF_AGE)
            && terminal.id == self.address.terminal_id()
            && recipient.is_current(&self.checkpoint)
            && recipient.matches_address(&self.address)
            && recipient.native_input_evidence(terminal) == Some(self.image)
            && terminal.observation(32_768).as_ref() == Some(&self.screen)
    }
}

fn empty_codex_prompt(screen: &TerminalObservation) -> bool {
    let Ok(row) = usize::try_from(screen.cursor_row) else {
        return false;
    };
    let required = TermMode::SHOW_CURSOR | TermMode::BRACKETED_PASTE | TermMode::ALT_SCREEN;
    let supported = required
        | TermMode::SGR_MOUSE
        | TermMode::MOUSE_MOTION
        | TermMode::LINE_WRAP
        | TermMode::FOCUS_IN_OUT
        | TermMode::URGENCY_HINTS;
    if screen.display_offset != 0
        || screen.cursor_column != 2
        || row.checked_add(4) != Some(screen.lines.len())
        || !screen.mode.contains(required)
        || !screen.mode.difference(supported).is_empty()
        || screen
            .lines
            .get(row)
            .is_none_or(|line| line.trim_end() != format!("› {PLACEHOLDER}"))
        || screen.cursor_line_styles.len() < 2 + PLACEHOLDER.len()
    {
        return false;
    }
    if !screen.cursor_line_styles[2..2 + PLACEHOLDER.len()]
        .iter()
        .all(|style| {
            style.foreground == TerminalColor::Named(NamedColor::Foreground)
                && style.background == TerminalColor::Named(NamedColor::Background)
                && style.flags == TerminalCellFlags::DIM
        })
    {
        return false;
    }
    let footer = screen.lines.last().unwrap();
    if !footer.contains("for agents") || !footer.contains("for shortcuts") {
        return false;
    }
    !screen.lines[row.saturating_sub(4)..].iter().any(|line| {
        let line = line.to_ascii_lowercase();
        line.contains("esc to interrupt")
            || line.contains("esc to stop")
            || line.contains("working")
    })
}
