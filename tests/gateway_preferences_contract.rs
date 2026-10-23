use amux::gateway_preferences::GatewayPreferences;
use std::io::{Read, Write};

fn directory() -> tempfile::TempDir {
    let base = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    std::fs::create_dir_all(&base).unwrap();
    tempfile::tempdir_in(base).unwrap()
}

#[tokio::test]
async fn saved_preferences_connect_without_environment_configuration() {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let dir = directory();
    let path = dir.path().join("gateway.json");
    let settings = GatewayPreferences {
        url: format!("http://{}", listener.local_addr().unwrap()),
        api_key: "private-fixture-key".into(),
        project: "amux-fixture".into(),
        ..Default::default()
    };
    settings.save(&path).unwrap();
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        assert_eq!(
            std::fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o600
        );
    }
    let server = std::thread::spawn(move || {
        let (mut socket, _) = listener.accept().unwrap();
        socket
            .set_read_timeout(Some(std::time::Duration::from_secs(5)))
            .unwrap();
        let mut request = Vec::new();
        while !request.ends_with(b"\r\n\r\n") {
            let mut byte = [0];
            socket.read_exact(&mut byte).unwrap();
            request.push(byte[0]);
        }
        let request = String::from_utf8(request).unwrap();
        assert!(request.starts_with("GET /v1/projects/amux-fixture/tasks"));
        assert!(
            request
                .to_ascii_lowercase()
                .contains("authorization: bearer private-fixture-key")
        );
        socket
            .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n[]")
            .unwrap();
    });
    let restored = GatewayPreferences::load(&path).unwrap().unwrap();
    assert_eq!(
        restored
            .gateway()
            .unwrap()
            .list(&restored.project)
            .await
            .unwrap(),
        serde_json::json!([])
    );
    server.join().unwrap();
}

#[test]
fn invalid_settings_preserve_saved_credentials_and_hide_secret_errors() {
    let dir = directory();
    let path = dir.path().join("gateway.json");
    let mut settings = GatewayPreferences {
        url: "https://gateway.example.test".into(),
        api_key: "fixture-key".into(),
        project: "amux".into(),
        ..Default::default()
    };
    settings.save(&path).unwrap();
    let original = std::fs::read(&path).unwrap();
    settings.url = "https://user:do-not-show@example.test".into();
    assert!(
        !settings
            .save(&path)
            .unwrap_err()
            .to_string()
            .contains("do-not-show")
    );
    assert_eq!(std::fs::read(&path).unwrap(), original);
    settings.url = "https://gateway.example.test".into();
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
