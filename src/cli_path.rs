//! The desktop runs agents through the console CLI, never through itself.
use std::{
    io,
    path::{Path, PathBuf},
};

/// The `amux` CLI next to `exe`, which may be the CLI itself. The GUI entry point
/// has no console on Windows and would read agent arguments as its own.
pub fn cli_beside(exe: &Path) -> io::Result<PathBuf> {
    let cli = exe.with_file_name(format!("amux{}", std::env::consts::EXE_SUFFIX));
    if cli.is_file() {
        return Ok(cli);
    }
    Err(io::Error::new(
        io::ErrorKind::NotFound,
        format!(
            "amux CLI not found at {}; keep amux and amux-desktop in the same directory",
            cli.display()
        ),
    ))
}
