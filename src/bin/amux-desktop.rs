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
    #[arg(long, default_value = ".")]
    project_dir: PathBuf,
}

fn main() -> ExitCode {
    match desktop::run(Args::parse().project_dir) {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("amux-desktop: {error:#}");
            ExitCode::FAILURE
        }
    }
}
