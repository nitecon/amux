//! Conservative hookless empty-composer recognition from one atomic live grid,
//! ported from CMUX's gateway readiness classifier. Never reads scrollback.
//! A ready result is not delivery, acknowledgment or recipient proof: callers
//! still check process generation, exact recipient, human input and consent.
use crate::identity::Provider;
use iced_term::{NamedColor, TermMode, TerminalCellFlags, TerminalColor, TerminalGridObservation};
use std::ops::Range;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum InputState {
    /// Unsupported layout, hidden caret, scrollback view or malformed grid.
    Unknown,
    /// Working, connecting, permission or other dialog state.
    Busy,
    /// A draft, attachment, multiline input or caret away from the marker.
    Unfinished,
    EmptyReady,
}

const BUSY_NEAR_CARET: [&str; 3] = ["esc to interrupt", "escape to interrupt", "esc to cancel"];
const BUSY_IN_COMPOSER: [&str; 8] = [
    "connecting",
    "reconnecting",
    "input disabled",
    "viewing sub-agent",
    "permission",
    "approve",
    "allow once",
    "queued message",
];
const CLAUDE_AUTO_FOOTER: &str = "⏵⏵ auto mode on (shift+tab to cycle)";

/// Classify the expected provider's editable composer. A marker belonging to
/// another provider, or any layout not recognized here, stays Unknown.
pub fn classify(grid: &TerminalGridObservation, provider: Provider) -> InputState {
    let screen = &grid.observation;
    let lines: Vec<Vec<char>> = screen.lines.iter().map(|l| l.chars().collect()).collect();
    let rows = lines.len();
    let Ok(row) = usize::try_from(screen.cursor_row) else {
        return InputState::Unknown;
    };
    if rows == 0
        || rows > 200
        || row >= rows
        || screen.display_offset != 0
        || !screen.mode.contains(TermMode::SHOW_CURSOR)
        || grid.styles.len() != rows
        || lines
            .iter()
            .zip(&grid.styles)
            .any(|(line, styles)| line.len() != styles.len())
    {
        return InputState::Unknown;
    }
    let text = |y: usize| lines[y].iter().collect::<String>();
    let marker = match provider {
        Provider::Codex => '›',
        Provider::Claude => '❯',
    };
    let leading = lines[row].iter().take_while(|c| **c == ' ').count();
    if lines[row].get(leading) != Some(&marker) {
        return InputState::Unknown;
    }
    let composer = (provider == Provider::Codex)
        .then(|| shaded_composer(grid, row))
        .flatten();
    // Interrupt indicators sit just above the composer; dialog words in prior
    // replies are not UI state, so those only count from the composer down.
    let near = (row.saturating_sub(6)..rows).map(text).collect::<Vec<_>>();
    let below = (composer.as_ref().map_or(row.saturating_sub(6), |r| r.start)..rows)
        .map(text)
        .collect::<Vec<_>>();
    let near = near.join("\n").to_lowercase();
    let below = below.join("\n").to_lowercase();
    if BUSY_NEAR_CARET.iter().any(|s| near.contains(s))
        || BUSY_IN_COMPOSER.iter().any(|s| below.contains(s))
    {
        return InputState::Busy;
    }
    // Editable providers put the caret right after their marker and one blank.
    if screen.cursor_column != leading + 2 {
        return InputState::Unfinished;
    }
    let Some(footer) = (row + 1..rows.min(row + 5)).find(|&y| {
        let line = text(y);
        line.to_lowercase().contains("for shortcuts")
            || (provider == Provider::Claude && line.trim_start().starts_with(CLAUDE_AUTO_FOOTER))
    }) else {
        return InputState::Unknown;
    };
    let closing_rule = (provider == Provider::Claude)
        .then(|| (row + 1..footer).find(|&y| horizontal_rule(&text(y))))
        .flatten();
    // The auto-mode footer is supported only below Claude's composer boundary.
    if !text(footer).to_lowercase().contains("for shortcuts") && closing_rule.is_none() {
        return InputState::Unknown;
    }
    // Unshaded (ConPTY) Codex pads input from its model/path status row with a
    // blank row; only that status row directly above the footer is excluded.
    let plain_codex = (provider == Provider::Codex
        && footer >= row + 3
        && text(footer - 2).trim().is_empty()
        && text(footer - 1).contains(" · "))
    .then(|| row.saturating_sub(1)..footer - 1);
    let input = composer
        .or(plain_codex)
        .unwrap_or(row..closing_rule.map_or(footer, |y| y + 1));
    if input.end > footer {
        return InputState::Unknown;
    }
    let decoration =
        |c: &char| c.is_whitespace() || matches!(c, '─' | '━' | '╭' | '╮' | '╰' | '╯' | '│');
    if input
        .filter(|&y| y != row)
        .any(|y| !lines[y].iter().all(decoration))
    {
        return InputState::Unfinished;
    }
    // Placeholders render faint; drafts use the editable foreground style.
    if lines[row]
        .iter()
        .zip(&grid.styles[row])
        .skip(leading + 2)
        .any(|(c, style)| !c.is_whitespace() && !style.flags.contains(TerminalCellFlags::DIM))
    {
        return InputState::Unfinished;
    }
    InputState::EmptyReady
}

/// After an amux paste, Enter is allowed only while the composer contains
/// exactly that literal message. Human input is separately fenced by the UI epoch.
pub fn pending_literal(grid: &TerminalGridObservation, provider: Provider, message: &str) -> bool {
    let screen = &grid.observation;
    let Ok(caret) = usize::try_from(screen.cursor_row) else {
        return false;
    };
    if screen.lines.is_empty()
        || screen.lines.len() > 200
        || caret >= screen.lines.len()
        || screen.display_offset != 0
        || !screen.mode.contains(TermMode::SHOW_CURSOR)
    {
        return false;
    }
    let marker = match provider {
        Provider::Codex => '›',
        Provider::Claude => '❯',
    };
    let Some(start) = (0..=caret).rev().find(|&y| {
        screen.lines[y]
            .trim_start()
            .starts_with(&format!("{marker} "))
    }) else {
        return false;
    };
    let Some(footer) = (caret + 1..screen.lines.len().min(caret + 6)).find(|&y| {
        let line = screen.lines[y].trim_start();
        line.to_lowercase().contains("for shortcuts")
            || (provider == Provider::Claude && line.starts_with(CLAUDE_AUTO_FOOTER))
    }) else {
        return false;
    };
    let near = screen.lines[start.saturating_sub(6)..footer]
        .join("\n")
        .to_lowercase();
    let input = screen.lines[start..footer].join("\n").to_lowercase();
    if BUSY_NEAR_CARET.iter().any(|word| near.contains(word))
        || BUSY_IN_COMPOSER.iter().any(|word| input.contains(word))
    {
        return false;
    }
    let mut end = (start + 1..footer)
        .find(|&y| horizontal_rule(&screen.lines[y]))
        .unwrap_or(footer);
    if provider == Provider::Codex
        && footer >= start + 3
        && screen.lines[footer - 2].trim().is_empty()
        && screen.lines[footer - 1].contains(" · ")
    {
        end = end.min(footer - 1);
    }
    let first = screen.lines[start]
        .trim_start()
        .strip_prefix(marker)
        .unwrap_or_default();
    let content = std::iter::once(first)
        .chain(screen.lines[start + 1..end].iter().map(String::as_str))
        .collect::<String>();
    // Soft wrapping and provider indentation can replace spaces at a line break.
    let compact = |text: &str| {
        text.chars()
            .filter(|c| !c.is_whitespace())
            .collect::<String>()
    };
    !message.is_empty() && compact(&content) == compact(message)
}

/// Codex's shaded composer: contiguous full rows sharing one non-default
/// background, independent of theme colors or footer text.
fn shaded_composer(grid: &TerminalGridObservation, row: usize) -> Option<Range<usize>> {
    let background = grid.styles[row].first()?.background;
    if background == TerminalColor::Named(NamedColor::Background) {
        return None;
    }
    let same = |y: usize| grid.styles[y].iter().all(|s| s.background == background);
    if !same(row) {
        return None;
    }
    let mut start = row;
    while start > 0 && same(start - 1) {
        start -= 1;
    }
    let mut end = row + 1;
    while end < grid.styles.len() && same(end) {
        end += 1;
    }
    Some(start..end)
}

/// Claude's closing rule separates editable input from a custom status line.
fn horizontal_rule(line: &str) -> bool {
    let trimmed = line.trim();
    trimmed.chars().count() >= 3 && trimmed.chars().all(|c| matches!(c, '─' | '━'))
}
