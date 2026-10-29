//! A Finder-style environment must still find providers installed by shell profiles.
#![cfg(target_os = "macos")]

use std::{fs, os::unix::fs::PermissionsExt, path::Path, process::Command};

fn executable(path: &Path, body: &[u8]) {
    fs::write(path, body).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o700)).unwrap();
}

#[test]
fn providers_use_the_login_profile_keep_arguments_and_find_the_bundled_cli() {
    let fixtures = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&fixtures).unwrap();
    let fixture = tempfile::tempdir_in(fixtures).unwrap();
    let root = fixture.path();
    let bin = root.join("App Bundle");
    let providers = root.join("provider bin");
    let profile = root.join("shell profile");
    let project = root.join("project with spaces");
    for directory in [&bin, &providers, &profile, &project] {
        fs::create_dir(directory).unwrap();
    }
    let cli = bin.join("amux");
    fs::copy(env!("CARGO_BIN_EXE_amux"), &cli).unwrap();
    fs::set_permissions(&cli, fs::Permissions::from_mode(0o700)).unwrap();
    // Override the inherited PATH entirely: the launcher must restore access to
    // its own CLI after the login shell has loaded the provider's directory.
    fs::write(
        profile.join(".zshrc"),
        "export PATH=\"$AMUX_TEST_PROVIDER_BIN:/usr/bin:/bin:/usr/sbin:/sbin\"\n",
    )
    .unwrap();
    let args = [
        "two words",
        "$HOME; $(touch SHOULD_NOT_EXIST)",
        "quote'\"\\",
        "界",
    ];
    for provider in ["codex", "claude"] {
        executable(
            &providers.join(provider),
            b"#!/bin/sh\nprintf 'ARG:%s\\n' \"$@\"\nprintf 'CLI:%s\\n' \"$(command -v amux)\"\nprintf 'CWD:%s\\n' \"$(pwd -P)\"\nprintf 'NUMBER:%s\\n' \"$AMUX_TERMINAL_NUMBER\"\n",
        );
        let output = Command::new(&cli)
            .args([
                "--project-dir",
                project.to_str().unwrap(),
                "agent",
                provider,
                "--",
            ])
            .args(args)
            .env("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
            .env("SHELL", "/bin/zsh")
            .env("ZDOTDIR", &profile)
            .env("AMUX_TEST_PROVIDER_BIN", &providers)
            .env("AMUX_STATE_DIR", root.join("state"))
            .env("AMUX_TERMINAL_NUMBER", "42")
            .current_dir(&project)
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "{provider}: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        let stdout = String::from_utf8_lossy(&output.stdout);
        let actual: Vec<_> = stdout
            .lines()
            .filter_map(|line| line.strip_prefix("ARG:"))
            .collect();
        assert_eq!(actual, args, "{provider}: {stdout}");
        assert!(
            stdout.contains(&format!("CLI:{}", cli.display())),
            "{stdout}"
        );
        assert!(
            stdout.contains(&format!(
                "CWD:{}",
                project.canonicalize().unwrap().display()
            )),
            "{stdout}"
        );
        assert!(stdout.contains("NUMBER:42"), "{stdout}");
        assert!(!project.join("SHOULD_NOT_EXIST").exists());
    }
}
