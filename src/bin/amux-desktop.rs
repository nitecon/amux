//! Native desktop entry point, without a console window on Windows. Agent tabs
//! run the sibling `amux` CLI, so agent tools keep their stdout and stderr.
#![cfg_attr(windows, windows_subsystem = "windows")]

#[path = "../desktop.rs"]
mod desktop;

use clap::Parser;
use std::{path::PathBuf, process::ExitCode};

/// Commands and agents use the `amux` CLI; this entry point only opens the desktop.
#[derive(Parser)]
#[command(name = "amux-desktop", version)]
struct Args {
    #[arg(long)]
    project_dir: Option<PathBuf>,
}

fn main() -> ExitCode {
    match Args::parse()
        .project_dir
        .map(Ok)
        .unwrap_or_else(amux::workspace_launch::home_directory)
        .and_then(desktop::run)
    {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("amux-desktop: {error:#}");
            ExitCode::FAILURE
        }
    }
}
