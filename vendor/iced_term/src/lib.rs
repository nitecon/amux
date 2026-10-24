pub mod actions;
pub mod bindings;
pub mod settings;

mod backend;
mod font;
mod terminal;
mod theme;
mod view;

pub use alacritty_terminal::event::Event as AlacrittyEvent;
pub use alacritty_terminal::index::Point as AlacrittyPoint;
pub use alacritty_terminal::selection::SelectionType;
pub use alacritty_terminal::term::cell::Flags as TerminalCellFlags;
pub use alacritty_terminal::term::TermMode;
pub use alacritty_terminal::vte::ansi::{Color as TerminalColor, NamedColor};
pub use backend::Command as BackendCommand;
pub use backend::{LinkAction, MouseButton};
pub use terminal::{Command, Event, Terminal};
pub use theme::{ColorPalette, Theme};
pub use view::TerminalView;

/// Parser cell styling, before cursor rendering or theme color resolution.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct TerminalCellStyle {
    pub foreground: alacritty_terminal::vte::ansi::Color,
    pub background: alacritty_terminal::vte::ansi::Color,
    pub flags: alacritty_terminal::term::cell::Flags,
}

/// Live screen state for recipient/readiness checks. No scrollback is returned.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TerminalObservation {
    pub lines: Vec<String>,
    pub cursor_row: i32,
    pub cursor_column: usize,
    /// Styling for the cursor's live row, with one entry per terminal column.
    pub cursor_line_styles: Vec<TerminalCellStyle>,
    pub display_offset: usize,
    pub mode: TermMode,
}

/// Live screen plus parser styling for every visible cell, captured under one
/// parser lock so text and styles describe the same frame. No scrollback.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TerminalGridObservation {
    pub observation: TerminalObservation,
    /// Row-major styling, one row per line and one entry per terminal column.
    pub styles: Vec<Vec<TerminalCellStyle>>,
}
