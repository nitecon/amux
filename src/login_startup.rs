//! Opt-in, per-user desktop login startup. Never register from ordinary app startup.
use anyhow::{Context, Result, bail};
use std::path::{Path, PathBuf};

/// Save the intent only after OS registration succeeds. Restore registration if
/// the preferences write fails so the checkbox does not report a false result.
pub fn save_preferences(
    current: &crate::gateway_preferences::GatewayPreferences,
    draft: &crate::gateway_preferences::GatewayPreferences,
    path: &Path,
) -> Result<()> {
    save_with(
        current.start_on_login,
        draft.start_on_login,
        set_enabled,
        || draft.save(path),
    )
}

fn save_with(
    current: bool,
    draft: bool,
    mut register: impl FnMut(bool) -> Result<()>,
    save: impl FnOnce() -> Result<()>,
) -> Result<()> {
    let changed = current != draft;
    if changed {
        register(draft).context("Could not change login startup")?;
    }
    if let Err(error) = save() {
        if changed && let Err(rollback) = register(current) {
            bail!("{error}; could not restore login startup: {rollback:#}");
        }
        return Err(error);
    }
    Ok(())
}

fn desktop_executable() -> Result<PathBuf> {
    let executable = std::env::current_exe()?
        .with_file_name(format!("amux-desktop{}", std::env::consts::EXE_SUFFIX));
    if !executable.is_file() {
        bail!("Keep amux-desktop beside the amux CLI before enabling login startup");
    }
    Ok(executable)
}

fn executable_text(executable: &Path) -> Result<&str> {
    let text = executable
        .to_str()
        .context("Installation path is not Unicode")?;
    if !executable.is_absolute() || text.chars().any(char::is_control) {
        bail!("Login startup requires an absolute installation path without control characters");
    }
    Ok(text)
}

fn set_enabled(enabled: bool) -> Result<()> {
    // Disabling must work even after the old executable has been moved/deleted.
    let executable = enabled.then(desktop_executable).transpose()?;
    platform_set(executable.as_deref())
}

#[cfg(any(target_os = "linux", target_os = "macos"))]
fn write_entry(path: &Path, contents: Option<&str>) -> Result<()> {
    use std::io::Write;
    let Some(contents) = contents else {
        return match std::fs::remove_file(path) {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(error) => Err(error.into()),
        };
    };
    let directory = path
        .parent()
        .context("Login entry has no parent directory")?;
    std::fs::create_dir_all(directory)?;
    let mut file = tempfile::NamedTempFile::new_in(directory)?;
    file.write_all(contents.as_bytes())?;
    file.as_file().sync_all()?;
    file.persist(path).map_err(|error| error.error)?;
    Ok(())
}

#[cfg(target_os = "linux")]
fn platform_set(executable: Option<&Path>) -> Result<()> {
    let config = directories::BaseDirs::new().context("User config directory unavailable")?;
    let path = config.config_dir().join("autostart/amux.desktop");
    let entry = executable.map(linux_entry).transpose()?;
    write_entry(&path, entry.as_deref())
}

#[cfg(any(target_os = "linux", test))]
fn linux_entry(executable: &Path) -> Result<String> {
    // Exec quoting is applied first, then Desktop Entry string escaping. Percent
    // is a field-code introducer even inside quotes; %% means a literal percent.
    let mut argument = String::from("\"");
    for ch in executable_text(executable)?.chars() {
        match ch {
            '%' => argument.push_str("%%"),
            '"' | '`' | '$' | '\\' => {
                argument.push('\\');
                argument.push(ch);
            }
            _ => argument.push(ch),
        }
    }
    argument.push('"');
    let argument = argument.replace('\\', "\\\\");
    // GLib checks the first Exec token exists before expanding %% field codes.
    // env keeps a percent in the install path in an argument until expansion,
    // then execs that exact path without a shell or an extra resident process.
    Ok(format!(
        "[Desktop Entry]\nType=Application\nName=amux\nComment=Agent terminals and gateway\nExec=/usr/bin/env {argument}\nTerminal=false\n"
    ))
}

#[cfg(target_os = "macos")]
fn platform_set(executable: Option<&Path>) -> Result<()> {
    if executable.is_some() && std::env::var_os("APP_SANDBOX_CONTAINER_ID").is_some() {
        bail!("For sandboxed builds, add amux in macOS System Settings > General > Login Items");
    }
    let home = directories::BaseDirs::new().context("User home directory unavailable")?;
    let path = home
        .home_dir()
        .join("Library/LaunchAgents/org.amux.desktop.plist");
    let entry = executable.map(macos_entry).transpose()?;
    // Only install/remove the next-login entry: do not launch a second GUI now
    // or unload this process when the user turns the setting off.
    write_entry(&path, entry.as_deref())
}

#[cfg(any(target_os = "macos", test))]
fn macos_entry(executable: &Path) -> Result<String> {
    let executable = executable_text(executable)?
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
        .replace('\'', "&apos;");
    Ok(format!(
        "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n\
         <!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n\
         <plist version=\"1.0\"><dict>\n\
         <key>Label</key><string>org.amux.desktop</string>\n\
         <key>ProgramArguments</key><array><string>{executable}</string></array>\n\
         <key>RunAtLoad</key><true/>\n\
         <key>LimitLoadToSessionType</key><string>Aqua</string>\n\
         </dict></plist>\n"
    ))
}

#[cfg(any(windows, test))]
fn windows_command(executable: &Path) -> Result<String> {
    let text = executable_text(executable)?;
    if text.contains('"') {
        bail!("Windows installation path contains a quote");
    }
    let command = format!("\"{text}\"");
    if command.encode_utf16().count() > 260 {
        bail!("Windows login startup requires an installation path under 259 UTF-16 characters");
    }
    Ok(command)
}

#[cfg(windows)]
fn platform_set(executable: Option<&Path>) -> Result<()> {
    use std::ptr::{null, null_mut};
    use windows_sys::Win32::{
        Foundation::{ERROR_FILE_NOT_FOUND, ERROR_SUCCESS},
        System::Registry::{
            HKEY_CURRENT_USER, KEY_SET_VALUE, REG_OPTION_NON_VOLATILE, REG_SZ, RegCloseKey,
            RegCreateKeyExW, RegDeleteKeyValueW, RegSetValueExW,
        },
    };
    let wide = |value: &str| value.encode_utf16().chain(Some(0)).collect::<Vec<_>>();
    let key = wide("Software\\Microsoft\\Windows\\CurrentVersion\\Run");
    let name = wide("amux");
    let status = if let Some(executable) = executable {
        let command = wide(&windows_command(executable)?);
        let mut handle = null_mut();
        // SAFETY: all strings are NUL-terminated and all output pointers are
        // valid. The returned key is closed before examining the write result.
        let opened = unsafe {
            RegCreateKeyExW(
                HKEY_CURRENT_USER,
                key.as_ptr(),
                0,
                null(),
                REG_OPTION_NON_VOLATILE,
                KEY_SET_VALUE,
                null(),
                &mut handle,
                null_mut(),
            )
        };
        if opened != ERROR_SUCCESS {
            return Err(std::io::Error::from_raw_os_error(opened as i32).into());
        }
        unsafe {
            let written = RegSetValueExW(
                handle,
                name.as_ptr(),
                0,
                REG_SZ,
                command.as_ptr().cast(),
                (command.len() * 2) as u32,
            );
            RegCloseKey(handle);
            written
        }
    } else {
        // SAFETY: valid predefined key and NUL-terminated strings. Delete only
        // our value, preserving every other startup application's registration.
        unsafe { RegDeleteKeyValueW(HKEY_CURRENT_USER, key.as_ptr(), name.as_ptr()) }
    };
    if status == ERROR_SUCCESS || (executable.is_none() && status == ERROR_FILE_NOT_FOUND) {
        Ok(())
    } else {
        Err(std::io::Error::from_raw_os_error(status as i32).into())
    }
}

#[cfg(not(any(target_os = "linux", target_os = "macos", windows)))]
fn platform_set(_: Option<&Path>) -> Result<()> {
    bail!("Login startup is supported on Linux, macOS, and Windows")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn registration_failure_does_not_save_and_save_failure_restores_registration() {
        let mut saved = false;
        assert!(
            save_with(
                false,
                true,
                |_| bail!("denied"),
                || {
                    saved = true;
                    Ok(())
                }
            )
            .is_err()
        );
        assert!(!saved);
        let mut changes = Vec::new();
        assert!(
            save_with(
                false,
                true,
                |enabled| {
                    changes.push(enabled);
                    Ok(())
                },
                || bail!("disk full")
            )
            .is_err()
        );
        assert_eq!(changes, [true, false]);
        // Unrelated settings must not undo changes made in OS startup settings.
        save_with(true, true, |_| panic!("unexpected registration"), || Ok(())).unwrap();
    }

    #[test]
    fn startup_entries_escape_paths_without_interpreting_arguments() {
        let root = std::env::current_dir().unwrap();
        let path = root.join("space & percent% dollar$ quote\".exe");
        let linux = linux_entry(&path).unwrap();
        assert!(linux.contains("percent%%"));
        assert!(linux.contains("dollar\\\\$"));
        assert!(linux.contains("quote\\\\\""));
        let macos = macos_entry(&path).unwrap();
        assert!(macos.contains("space &amp; percent% dollar$ quote&quot;.exe"));
        assert!(macos.contains("<key>RunAtLoad</key><true/>"));
        assert!(windows_command(&path).is_err());
        let normal = root.join("space & percent%.exe");
        assert_eq!(
            windows_command(&normal).unwrap(),
            format!("\"{}\"", normal.display())
        );
        assert!(linux_entry(Path::new("relative")).is_err());
        assert!(macos_entry(&root.join("newline\n.exe")).is_err());
        assert!(windows_command(&root.join("x".repeat(261))).is_err());
    }

    #[cfg(any(target_os = "linux", target_os = "macos"))]
    #[test]
    fn file_registration_is_replaceable_and_disable_is_idempotent() {
        let base = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&base).unwrap();
        let dir = tempfile::tempdir_in(base).unwrap();
        let entry = dir.path().join("nested/login-entry");
        write_entry(&entry, None).unwrap();
        write_entry(&entry, Some("first")).unwrap();
        write_entry(&entry, Some("second")).unwrap();
        assert_eq!(std::fs::read_to_string(&entry).unwrap(), "second");
        let unrelated = entry.with_file_name("unrelated");
        std::fs::write(&unrelated, "keep").unwrap();
        write_entry(&entry, None).unwrap();
        write_entry(&entry, None).unwrap();
        assert!(unrelated.exists());
        assert!(!entry.exists());
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn linux_launcher_opens_the_exact_escaped_executable() {
        use std::{os::unix::fs::PermissionsExt, process::Command, time::Duration};
        // gio is the native desktop-entry parser. No desktop session or real
        // login registration is needed to launch this disposable probe.
        if Command::new("gio").arg("version").output().is_err() {
            return;
        }
        let base = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&base).unwrap();
        let dir = tempfile::tempdir_in(base).unwrap();
        let executable = dir.path().join("amux space% $`\\\".sh");
        std::fs::write(
            &executable,
            "#!/bin/sh\nprintf launched > \"$(dirname \"$0\")/receipt\"\n",
        )
        .unwrap();
        std::fs::set_permissions(&executable, std::fs::Permissions::from_mode(0o700)).unwrap();
        let entry = dir.path().join("amux.desktop");
        write_entry(&entry, Some(&linux_entry(&executable).unwrap())).unwrap();
        let output = Command::new("gio")
            .arg("launch")
            .arg(&entry)
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
        let receipt = dir.path().join("receipt");
        for _ in 0..100 {
            if receipt.exists() {
                break;
            }
            std::thread::sleep(Duration::from_millis(10));
        }
        assert_eq!(std::fs::read_to_string(receipt).unwrap(), "launched");
    }
}
