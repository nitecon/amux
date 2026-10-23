//! CLI and GUI entry points: agents always run through the console CLI.
use amux::cli_path::cli_beside;
use std::{fs, path::Path};

fn exe(name: &str) -> String {
    format!("{name}{}", std::env::consts::EXE_SUFFIX)
}

#[test]
fn desktop_resolves_the_sibling_cli_and_refuses_a_missing_one() {
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&target).unwrap();
    let fixture = tempfile::tempdir_in(target).unwrap();
    let gui = fixture.path().join(exe("amux-desktop"));
    fs::write(&gui, b"").unwrap();
    let error = cli_beside(&gui).unwrap_err();
    assert_eq!(error.kind(), std::io::ErrorKind::NotFound);
    assert!(error.to_string().contains("amux CLI not found"), "{error}");

    let cli = fixture.path().join(exe("amux"));
    fs::write(&cli, b"").unwrap();
    assert_eq!(cli_beside(&gui).unwrap(), cli, "never the GUI itself");
    assert_eq!(cli_beside(&cli).unwrap(), cli);
}

#[cfg(feature = "desktop")]
#[test]
fn gui_entry_never_accepts_cli_commands_or_opens_a_window_on_bad_input() {
    use std::process::Command;
    let gui = env!("CARGO_BIN_EXE_amux-desktop");
    // Agent and task commands belong to the CLI; the GUI rejects them before
    // creating any window.
    for args in [
        &["agent", "codex"][..],
        &["--project-dir", ".", "agent", "claude"],
        &["tasks", "list"],
    ] {
        let output = Command::new(gui).args(args).output().unwrap();
        assert!(!output.status.success(), "{args:?}");
    }
    let missing = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/no-such-project-dir");
    let output = Command::new(gui)
        .arg("--project-dir")
        .arg(&missing)
        .output()
        .unwrap();
    assert!(!output.status.success());
    assert!(
        Command::new(gui)
            .arg("--version")
            .output()
            .unwrap()
            .status
            .success()
    );
}

#[test]
fn cli_update_recognizes_the_flag_but_never_overwrites_an_unmanaged_install() {
    let output = std::process::Command::new(env!("CARGO_BIN_EXE_amux"))
        .arg("--update")
        .output()
        .unwrap();
    assert!(!output.status.success());
    let error = String::from_utf8_lossy(&output.stderr);
    assert!(
        error.contains("not a verified amux direct-download bundle"),
        "{error}"
    );
    assert!(error.contains("Nothing was changed"), "{error}");
}
