//! Empty-composer readiness from atomic native grids. Fixtures mirror CMUX's
//! observed Codex/Claude layouts; no provider is launched and no turn submitted.
#![cfg(feature = "desktop")]
use amux::delegation_readiness::{InputState, classify};
use amux::identity::Provider::{self, Claude, Codex};
use iced_term::{
    NamedColor, TermMode, TerminalCellFlags, TerminalCellStyle, TerminalColor,
    TerminalGridObservation, TerminalObservation,
};

const COLUMNS: usize = 64;
const SHADE: TerminalColor = TerminalColor::Indexed(238);

fn plain() -> TerminalCellStyle {
    TerminalCellStyle {
        foreground: TerminalColor::Named(NamedColor::Foreground),
        background: TerminalColor::Named(NamedColor::Background),
        flags: TerminalCellFlags::empty(),
    }
}

/// Full-width rows with default styling; tests restyle cells explicitly.
fn grid(rows: &[&str], cursor: (i32, usize)) -> TerminalGridObservation {
    let lines: Vec<String> = rows
        .iter()
        .map(|row| {
            let mut line: String = row.chars().take(COLUMNS).collect();
            let width = line.chars().count();
            line.extend(std::iter::repeat_n(' ', COLUMNS - width));
            line
        })
        .collect();
    let styles = vec![vec![plain(); COLUMNS]; lines.len()];
    TerminalGridObservation {
        observation: TerminalObservation {
            lines,
            cursor_row: cursor.0,
            cursor_column: cursor.1,
            cursor_line_styles: styles[cursor.0 as usize].clone(),
            display_offset: 0,
            mode: TermMode::SHOW_CURSOR | TermMode::BRACKETED_PASTE,
        },
        styles,
    }
}

fn set_line(grid: &mut TerminalGridObservation, row: usize, text: &str) {
    let mut line: String = text.chars().take(COLUMNS).collect();
    let width = line.chars().count();
    line.extend(std::iter::repeat_n(' ', COLUMNS - width));
    grid.observation.lines[row] = line;
}

fn dim(grid: &mut TerminalGridObservation, row: usize, columns: std::ops::Range<usize>) {
    for style in &mut grid.styles[row][columns] {
        style.flags |= TerminalCellFlags::DIM;
    }
}

fn shade(grid: &mut TerminalGridObservation, rows: std::ops::Range<usize>) {
    for row in rows {
        for style in &mut grid.styles[row] {
            style.background = SHADE;
        }
    }
}

fn assert_state(grid: &TerminalGridObservation, provider: Provider, expected: InputState) {
    assert_eq!(
        classify(grid, provider),
        expected,
        "{:#?}",
        grid.observation.lines
    );
}

/// Codex 0.160 shaded composer: padding/input rows share a background, the
/// placeholder is faint, and model status plus hints sit outside the shading.
fn shaded_codex() -> TerminalGridObservation {
    let mut g = grid(
        &[
            "The permission check is preserved; repair approved.",
            "",
            "› Ask Codex to do anything",
            "",
            "  GPT-6.1-Sol high · ~/project · Context 77% left",
            "  ← for agents · ? for shortcuts",
            "",
            "",
        ],
        (2, 2),
    );
    shade(&mut g, 1..4);
    dim(&mut g, 2, 2..COLUMNS);
    g
}

#[test]
fn shaded_codex_empty_composer_is_ready_and_ignores_prior_reply_words() {
    assert_state(&shaded_codex(), Codex, InputState::EmptyReady);
    let mut themed = shaded_codex();
    for row in 1..4 {
        for style in &mut themed.styles[row] {
            style.background = TerminalColor::Indexed(255);
        }
    }
    assert_state(&themed, Codex, InputState::EmptyReady);
}

#[test]
fn shaded_codex_blocks_drafts_attachments_multiline_and_dialogs() {
    let mut typed = shaded_codex();
    typed.styles[2][2].flags = TerminalCellFlags::empty();
    assert_state(&typed, Codex, InputState::Unfinished);

    let mut multiline = shaded_codex();
    set_line(&mut multiline, 3, "a second draft line");
    assert_state(&multiline, Codex, InputState::Unfinished);

    let mut attachment = shaded_codex();
    set_line(&mut attachment, 1, "[Image #1]");
    assert_state(&attachment, Codex, InputState::Unfinished);

    let mut busy = shaded_codex();
    set_line(&mut busy, 0, "Working · esc to interrupt");
    assert_state(&busy, Codex, InputState::Busy);

    let mut dialog = shaded_codex();
    set_line(&mut dialog, 5, "Allow once · ? for shortcuts");
    assert_state(&dialog, Codex, InputState::Busy);

    let mut partial_shade = shaded_codex();
    partial_shade.styles[2][COLUMNS - 1].background = TerminalColor::Named(NamedColor::Background);
    assert_ne!(classify(&partial_shade, Codex), InputState::EmptyReady);
}

/// ConPTY Codex 0.161 without shading: blank padding and a model/path status row.
fn unshaded_codex() -> TerminalGridObservation {
    let mut rows = vec![""; 19];
    rows.extend([
        "",
        "› Ask Codex to do anything",
        "",
        "  GPT-6.1-Sol default · D:\\project",
        "  ? for shortcuts",
    ]);
    let mut g = grid(&rows, (20, 2));
    dim(&mut g, 20, 2..COLUMNS);
    g
}

#[test]
fn unshaded_codex_separates_input_from_status() {
    assert_state(&unshaded_codex(), Codex, InputState::EmptyReady);

    let mut typed = unshaded_codex();
    typed.styles[20] = vec![plain(); COLUMNS];
    assert_state(&typed, Codex, InputState::Unfinished);

    let mut attachment = unshaded_codex();
    set_line(&mut attachment, 19, "[Image #1]");
    assert_state(&attachment, Codex, InputState::Unfinished);

    let mut multiline = unshaded_codex();
    set_line(&mut multiline, 21, "a second draft line · keep it");
    assert_state(&multiline, Codex, InputState::Unfinished);

    let mut busy = unshaded_codex();
    set_line(&mut busy, 19, "Working · esc to interrupt");
    assert_state(&busy, Codex, InputState::Busy);

    let mut menu = unshaded_codex();
    set_line(&mut menu, 23, "enter select · esc back");
    assert_state(&menu, Codex, InputState::Unknown);
}

/// Claude 2.1.294 custom status line with the auto-mode footer.
fn claude() -> TerminalGridObservation {
    grid(
        &[
            "",
            "────────────────────────",
            "❯\u{a0}",
            "",
            "────────────────────────",
            "Opus 5.5 | cmux-gtk | ctx",
            "⏵⏵ auto mode on (shift+tab to cycle) · ← for agents",
            "",
        ],
        (2, 2),
    )
}

#[test]
fn claude_auto_mode_composer_is_ready_only_when_empty() {
    assert_state(&claude(), Claude, InputState::EmptyReady);

    let mut shortcuts = claude();
    set_line(&mut shortcuts, 6, "? for shortcuts");
    assert_state(&shortcuts, Claude, InputState::EmptyReady);

    let mut placeholder = claude();
    set_line(&mut placeholder, 2, "❯\u{a0}Try \"fix lint errors\"");
    assert_state(&placeholder, Claude, InputState::Unfinished);
    dim(&mut placeholder, 2, 2..COLUMNS);
    assert_state(&placeholder, Claude, InputState::EmptyReady);

    let mut draft = claude();
    set_line(&mut draft, 2, "❯ draft");
    assert_state(&draft, Claude, InputState::Unfinished);

    let mut multiline = claude();
    set_line(&mut multiline, 3, "second draft line");
    assert_state(&multiline, Claude, InputState::Unfinished);

    let mut caret = claude();
    caret.observation.cursor_column = 3;
    assert_state(&caret, Claude, InputState::Unfinished);

    let mut busy = claude();
    set_line(&mut busy, 0, "Working · esc to interrupt");
    assert_state(&busy, Claude, InputState::Busy);

    let mut dialog = claude();
    set_line(&mut dialog, 5, "Allow once");
    assert_state(&dialog, Claude, InputState::Busy);

    let mut boundary = claude();
    set_line(&mut boundary, 4, "unrecognized boundary");
    assert_state(&boundary, Claude, InputState::Unknown);

    let mut footer = claude();
    set_line(&mut footer, 6, "unrecognized footer");
    assert_state(&footer, Claude, InputState::Unknown);
}

#[test]
fn hidden_caret_scrollback_wrong_provider_and_malformed_grids_are_unknown() {
    for (grid, provider) in [(shaded_codex(), Codex), (claude(), Claude)] {
        let mut hidden = grid.clone();
        hidden.observation.mode.remove(TermMode::SHOW_CURSOR);
        assert_state(&hidden, provider, InputState::Unknown);

        let mut scrolled = grid.clone();
        scrolled.observation.display_offset = 3;
        assert_state(&scrolled, provider, InputState::Unknown);

        let other = if provider == Codex { Claude } else { Codex };
        assert_state(&grid, other, InputState::Unknown);

        let mut short = grid.clone();
        short.styles[0].pop();
        assert_state(&short, provider, InputState::Unknown);

        let mut outside = grid;
        outside.observation.cursor_row = outside.observation.lines.len() as i32;
        assert_state(&outside, provider, InputState::Unknown);
    }
    let shell = grid(&["$ ", "? for shortcuts"], (0, 2));
    assert_state(&shell, Codex, InputState::Unknown);
}

/// The vendored adapter captures text and per-cell parser styles from a real PTY.
#[cfg(target_os = "linux")]
#[test]
fn native_grid_observation_reports_faint_and_background_styles() {
    use iced_term::{Terminal, settings};
    use std::time::{Duration, Instant};
    let script = r"printf '\033[2J\033[H\033[48;5;238m› \033[2mAsk Codex\033[0m\nplain'; sleep 10";
    let terminal = Terminal::new(
        970_101,
        settings::Settings {
            backend: settings::BackendSettings {
                program: "/bin/sh".into(),
                args: vec!["-c".into(), script.into()],
                ..Default::default()
            },
            ..Default::default()
        },
    )
    .unwrap();
    let deadline = Instant::now() + Duration::from_secs(10);
    let grid = loop {
        if let Some(grid) = terminal.grid_observation(32_768)
            && grid
                .observation
                .lines
                .get(1)
                .is_some_and(|l| l.starts_with("plain"))
        {
            break grid;
        }
        assert!(Instant::now() < deadline, "fixture output never observed");
        std::thread::sleep(Duration::from_millis(20));
    };
    let screen = &grid.observation;
    assert_eq!(grid.styles.len(), screen.lines.len());
    assert!(screen.lines[0].starts_with("› Ask Codex"));
    assert_eq!(grid.styles[0][0].background, SHADE);
    assert!(!grid.styles[0][0].flags.contains(TerminalCellFlags::DIM));
    assert!(grid.styles[0][2].flags.contains(TerminalCellFlags::DIM));
    assert_eq!(grid.styles[0][2].background, SHADE);
    assert_eq!(grid.styles[1][0], plain());
    assert_eq!(
        grid.styles[screen.cursor_row as usize],
        screen.cursor_line_styles
    );
    assert!(
        terminal.observation(4).is_none(),
        "budget still bounds observations"
    );
}
