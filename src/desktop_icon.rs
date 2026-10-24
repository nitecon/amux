//! The native window and desktop shell use the same teal terminal mark.
pub(super) fn settings() -> iced::window::Settings {
    #[allow(unused_mut)]
    let mut settings = iced::window::Settings {
        icon: Some(
            iced::window::icon::from_rgba(
                include_bytes!("../assets/icons/amux.rgba").to_vec(),
                64,
                64,
            )
            .expect("embedded 64x64 RGBA icon"),
        ),
        ..Default::default()
    };
    #[cfg(target_os = "linux")]
    {
        settings.platform_specific.application_id = "io.amux.amux".into();
    }
    settings
}

#[cfg(target_os = "linux")]
pub(super) fn install() -> anyhow::Result<()> {
    // A portable release has a stable installation path. Development builds must
    // not replace the user's installed launcher with a target/debug path.
    if cfg!(debug_assertions) {
        return Ok(());
    }
    let executable = std::env::current_exe()?;
    let base = directories::BaseDirs::new().ok_or_else(|| anyhow::anyhow!("No data directory"))?;
    install_at(base.data_dir(), &executable)
}

#[cfg(any(target_os = "linux", test))]
fn install_at(data: &std::path::Path, executable: &std::path::Path) -> anyhow::Result<()> {
    let executable = executable
        .to_str()
        .ok_or_else(|| anyhow::anyhow!("Desktop executable path is not UTF-8"))?;
    anyhow::ensure!(
        !executable.contains(['\n', '\r']),
        "Invalid desktop executable path"
    );
    // Exec quoting is applied before the desktop-entry string escaping layer.
    let mut quoted = String::from("\"");
    for c in executable.chars() {
        if matches!(c, '"' | '`' | '$' | '\\') {
            quoted.push('\\');
        }
        quoted.push(c);
        if c == '%' {
            quoted.push('%');
        }
    }
    quoted.push('"');
    let quoted = quoted.replace('\\', "\\\\");
    let entry = format!(
        "[Desktop Entry]\nType=Application\nName=amux\nExec={quoted}\nIcon=io.amux.amux\nTerminal=false\nCategories=Development;TerminalEmulator;\nStartupWMClass=io.amux.amux\n"
    );
    for (path, bytes) in [
        (
            data.join("applications/io.amux.amux.desktop"),
            entry.as_bytes(),
        ),
        (
            data.join("icons/hicolor/256x256/apps/io.amux.amux.png"),
            include_bytes!("../assets/icons/amux.png").as_slice(),
        ),
    ] {
        if std::fs::read(&path).ok().as_deref() != Some(bytes) {
            std::fs::create_dir_all(path.parent().expect("desktop asset parent"))?;
            std::fs::write(path, bytes)?;
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn launcher_escapes_paths_and_installs_matching_icon() {
        let dir = tempfile::tempdir().unwrap();
        install_at(
            dir.path(),
            std::path::Path::new("/apps/my $amux%/amux-desktop"),
        )
        .unwrap();
        let entry =
            std::fs::read_to_string(dir.path().join("applications/io.amux.amux.desktop")).unwrap();
        assert!(entry.contains("Exec=\"/apps/my \\\\$amux%%/amux-desktop\"\n"));
        assert!(entry.contains("Icon=io.amux.amux\n"));
        assert_eq!(
            std::fs::read(
                dir.path()
                    .join("icons/hicolor/256x256/apps/io.amux.amux.png")
            )
            .unwrap(),
            include_bytes!("../assets/icons/amux.png")
        );
    }
}
