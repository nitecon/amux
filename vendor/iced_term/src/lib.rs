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
pub use alacritty_terminal::term::TermMode;
pub use backend::Command as BackendCommand;
pub use backend::{LinkAction, MouseButton};
pub use terminal::{Command, Event, Terminal};
pub use theme::{ColorPalette, Theme};
pub use view::TerminalView;

/// Live screen state for recipient/readiness checks. No scrollback is returned.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TerminalObservation {
    pub lines: Vec<String>,
    pub cursor_row: i32,
    pub cursor_column: usize,
    pub display_offset: usize,
    pub mode: TermMode,
}
