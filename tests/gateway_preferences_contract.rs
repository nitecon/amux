use amux::gateway_preferences::{EnrolledConnection, GatewayMode, GatewayPreferences};

fn directory() -> tempfile::TempDir {
    let base = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    std::fs::create_dir_all(&base).unwrap();
    tempfile::tempdir_in(base).unwrap()
}

fn connection(directory: &std::path::Path) -> EnrolledConnection {
    EnrolledConnection {
        url: "https://server.example.test:24182".into(),
        cluster_id: uuid::Uuid::new_v4().to_string(),
        node_id: uuid::Uuid::new_v4().to_string(),
        profile_path: directory.join("profile.json"),
    }
}

#[test]
fn native_connection_and_device_preferences_round_trip_without_secrets() {
    let dir = directory();
    let path = dir.path().join("gateway.json");
    let settings = GatewayPreferences {
        mode: GatewayMode::Client,
        enrolled: Some(connection(dir.path())),
        automatic_notifications: true,
        projects_dir: "my-projects".into(),
        ..Default::default()
    };
    settings.save(&path).unwrap();
    let restored = GatewayPreferences::load(&path).unwrap().unwrap();
    assert_eq!(restored.mode, GatewayMode::Client);
    assert_eq!(restored.enrolled, settings.enrolled);
    assert_eq!(restored.projects_dir, "my-projects");
    assert!(restored.automatic_notifications);
    assert!(restored.has_connection());
    let serialized = serde_json::to_value(restored).unwrap();
    for retired in ["url", "api_key", "project"] {
        assert!(serialized.get(retired).is_none());
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        assert_eq!(
            std::fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o600
        );
    }
}

#[test]
fn retired_connections_become_disconnected_without_switching_authority() {
    let dir = directory();
    let path = dir.path().join("gateway.json");
    for explicit_mode in [true, false] {
        let mut stored = serde_json::json!({
            "url": "https://retired.example.test",
            "api_key": "retired-private-key",
            "enrolled": connection(dir.path()),
            "automatic_notifications": true,
            "auto_resume": false,
            "projects_dir": "existing-projects"
        });
        if explicit_mode {
            stored["mode"] = serde_json::json!("legacy");
        }
        std::fs::write(&path, serde_json::to_vec(&stored).unwrap()).unwrap();
        let restored = GatewayPreferences::load(&path).unwrap().unwrap();
        assert_eq!(restored.mode, GatewayMode::Client);
        assert!(!restored.has_connection());
        assert!(!restored.automatic_notifications);
        assert!(!restored.auto_resume);
        assert_eq!(restored.projects_dir, "existing-projects");
        assert!(restored.gateway().is_err());
        restored.save(&path).unwrap();
        let saved = std::fs::read_to_string(&path).unwrap();
        assert!(!saved.contains("retired-private-key"));
        assert!(!saved.contains("retired.example.test"));
    }
}

#[test]
fn invalid_settings_preserve_saved_connection_and_hide_secret_errors() {
    let dir = directory();
    let path = dir.path().join("gateway.json");
    let mut settings = GatewayPreferences {
        mode: GatewayMode::Client,
        enrolled: Some(connection(dir.path())),
        ..Default::default()
    };
    settings.save(&path).unwrap();
    let original = std::fs::read(&path).unwrap();
    settings.enrolled.as_mut().unwrap().url = "https://user:do-not-show@example.test".into();
    assert!(
        !settings
            .save(&path)
            .unwrap_err()
            .to_string()
            .contains("do-not-show")
    );
    assert_eq!(std::fs::read(&path).unwrap(), original);
    settings.enrolled.as_mut().unwrap().url = "https://server.example.test".into();
    settings.machine_id = uuid::Uuid::new_v4().to_string();
    assert!(settings.validate().is_err());
    std::fs::write(&path, b"{broken-private-secret}").unwrap();
    assert!(
        !GatewayPreferences::load(&path)
            .err()
            .unwrap()
            .to_string()
            .contains("private-secret")
    );
}

#[test]
fn retired_environment_connections_do_not_start_a_new_authority() {
    const CHILD: &str = "AMUX_TEST_RETIRED_ENVIRONMENT";
    const MACHINE: &str = "00000000-0000-4000-8000-000000000001";
    if std::env::var_os(CHILD).is_some() {
        let retired = ["AMUX_GATEWAY_URL", "AMUX_GATEWAY_KEY"]
            .iter()
            .any(|name| !std::env::var(name).unwrap().is_empty());
        let preferences = GatewayPreferences::from_environment();
        assert_eq!(
            preferences.mode,
            if retired {
                GatewayMode::Client
            } else {
                GatewayMode::Server
            }
        );
        assert!(!preferences.has_connection());
        assert!(!preferences.automatic_notifications);
        assert_eq!(preferences.machine_id, MACHINE);
        assert_eq!(preferences.machine_key, "a".repeat(64));
        assert!(preferences.gateway().is_err());
        return;
    }
    for (url, key) in [
        ("", ""),
        ("https://retired.example.test", ""),
        ("", "retired-private-key"),
        ("https://retired.example.test", "retired-private-key"),
    ] {
        let output = std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "retired_environment_connections_do_not_start_a_new_authority",
                "--nocapture",
            ])
            .env(CHILD, "1")
            .env("AMUX_GATEWAY_URL", url)
            .env("AMUX_GATEWAY_KEY", key)
            .env("AMUX_GATEWAY_MACHINE_ID", MACHINE)
            .env("AMUX_GATEWAY_MACHINE_KEY", "a".repeat(64))
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "{}\n{}",
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
    }
}
