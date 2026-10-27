//! Real network-hosted enrollment, with an explicit rejecting system proxy.
use amux::{
    gateway::Gateway, gateway_preferences::ServerPreferences, gateway_transport::TransportProfile,
    server_controller,
};
use std::{
    fs,
    io::Write,
    net::TcpListener,
    path::{Path, PathBuf},
    process::{Command, Stdio},
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    time::{Duration, Instant},
};

#[test]
fn pairing_bypasses_a_rejecting_proxy_and_recovers_after_server_restart() {
    let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&root).unwrap();
    let fixture = tempfile::tempdir_in(root).unwrap();
    let proxy = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = format!("http://{}", proxy.local_addr().unwrap());
    proxy.set_nonblocking(true).unwrap();
    let stop = Arc::new(AtomicBool::new(false));
    let requests = Arc::new(AtomicUsize::new(0));
    let finished = stop.clone();
    let seen = requests.clone();
    let proxy_thread = std::thread::spawn(move || {
        while !finished.load(Ordering::Relaxed) {
            match proxy.accept() {
                Ok((mut stream, _)) => {
                    seen.fetch_add(1, Ordering::Relaxed);
                    stream
                        .set_write_timeout(Some(Duration::from_secs(1)))
                        .unwrap();
                    let _ = stream.write_all(
                        b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
                    );
                }
                Err(_) => std::thread::sleep(Duration::from_millis(10)),
            }
        }
    });
    let log_path = fixture.path().join("worker.log");
    let log = fs::File::create(&log_path).unwrap();
    let mut command = Command::new(std::env::current_exe().unwrap());
    command
        .args([
            "--exact",
            "network_pairing_worker",
            "--ignored",
            "--nocapture",
        ])
        .env_clear()
        .current_dir(fixture.path());
    for name in [
        "PATH",
        "SystemRoot",
        "WINDIR",
        "COMSPEC",
        "LD_LIBRARY_PATH",
        "DYLD_LIBRARY_PATH",
    ] {
        if let Some(value) = std::env::var_os(name) {
            command.env(name, value);
        }
    }
    for name in [
        "HOME",
        "USERPROFILE",
        "XDG_CONFIG_HOME",
        "XDG_DATA_HOME",
        "XDG_STATE_HOME",
        "XDG_CACHE_HOME",
        "TMP",
        "TEMP",
        "TMPDIR",
    ] {
        let path = fixture.path().join(name.to_lowercase());
        fs::create_dir(&path).unwrap();
        command.env(name, path);
    }
    for name in [
        "HTTP_PROXY",
        "HTTPS_PROXY",
        "ALL_PROXY",
        "http_proxy",
        "https_proxy",
        "all_proxy",
    ] {
        command.env(name, &address);
    }
    command
        .env("NO_PROXY", "")
        .env("no_proxy", "")
        .env("AMUX_NETWORK_PAIRING_FIXTURE", fixture.path())
        .env("AMUX_STATE_DIR", fixture.path().join("identity"))
        .env("DEFAULT_CHANNEL", "fixture-disabled")
        .env("GATEWAY_API_KEY", "fixture-only")
        .env("RUST_LOG", "warn")
        .stdin(Stdio::null())
        .stdout(Stdio::from(log.try_clone().unwrap()))
        .stderr(Stdio::from(log));
    let mut child = command.spawn().unwrap();
    let deadline = Instant::now() + Duration::from_secs(90);
    let result = loop {
        if let Some(status) = child.try_wait().unwrap() {
            break Some(status);
        }
        if Instant::now() >= deadline {
            child.kill().unwrap();
            child.wait().unwrap();
            break None;
        }
        std::thread::sleep(Duration::from_millis(20));
    };
    stop.store(true, Ordering::Relaxed);
    proxy_thread.join().unwrap();
    assert!(
        result.is_some_and(|status| status.success()),
        "{}",
        fs::read_to_string(&log_path).unwrap()
    );
    assert_eq!(
        requests.load(Ordering::Relaxed),
        0,
        "native connections went through the rejecting proxy"
    );
}

fn free_port() -> u16 {
    TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

#[test]
#[ignore = "isolated subprocess launched by the network pairing contract"]
fn network_pairing_worker() {
    let root = PathBuf::from(
        std::env::var_os("AMUX_NETWORK_PAIRING_FIXTURE").expect("parent fixture required"),
    );
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let secure_port = free_port();
        let mut discovery_port = free_port();
        while discovery_port == secure_port {
            discovery_port = free_port();
        }
        // This is a real address on the loopback subnet, not one of the three
        // literal hosts the old implementation excluded from system proxies.
        let config = ServerPreferences {
            network_hosting: true,
            server_name: "127.0.0.2".into(),
            secure_port,
            discovery_port,
        };
        let host = root.join("host");
        let client = root.join("client");
        let cli = PathBuf::from(env!("CARGO_BIN_EXE_amux"));
        let server = server_controller::ensure_local_with_executable(
            host.clone(),
            config.clone(),
            cli.clone(),
        )
        .await
        .unwrap();
        let admin = server.connection();
        let endpoint = config.client_url().unwrap();
        assert_ne!(endpoint, admin.url);
        assert_eq!(admin.url, format!("https://127.0.0.1:{secure_port}"));
        let original = server_controller::invitation(admin.clone(), true)
            .await
            .unwrap();
        server.stop().await.unwrap();
        drop(server);
        let server = server_controller::ensure_local_with_executable(host, config, cli)
            .await
            .unwrap();
        let admin = server.connection();
        assert!(
            server_controller::invitation(admin.clone(), false)
                .await
                .unwrap_err()
                .to_string()
                .contains("No active peering key")
        );
        let rejected = server_controller::pair(
            client.clone(),
            endpoint.clone(),
            original.invitation.clone(),
        )
        .await
        .unwrap_err()
        .to_string();
        assert!(
            rejected.contains("Enrollment failed") && rejected.contains("403"),
            "{rejected}"
        );
        assert!(
            rejected.contains("No active peering key") && rejected.contains("restarting"),
            "{rejected}"
        );
        assert!(!rejected.contains(&original.invitation));
        let fresh = server_controller::invitation(admin.clone(), true)
            .await
            .unwrap();
        assert_eq!(
            server_controller::invitation(admin.clone(), false)
                .await
                .unwrap()
                .invitation,
            fresh.invitation
        );
        // Reuse the failed client's durable state: a new valid invitation must
        // recover without deleting identities or pretending activation succeeded.
        let paired = server_controller::pair(client, endpoint.clone(), fresh.invitation.clone())
            .await
            .unwrap();
        assert_eq!(paired.url.trim_end_matches('/'), endpoint);
        let profile = TransportProfile::enrolled(&paired).unwrap();
        assert!(
            Gateway::from_profile(profile.clone())
                .unwrap()
                .projects()
                .await
                .is_ok()
        );
        assert!(
            server_controller::status(paired.clone())
                .await
                .unwrap_err()
                .to_string()
                .contains("403"),
            "paired client gained local administration"
        );
        server_controller::renew_now(paired.clone()).await.unwrap();
        assert!(
            Gateway::from_profile(profile)
                .unwrap()
                .projects()
                .await
                .is_ok()
        );
        server.stop().await.unwrap();
        let log = fs::read_to_string(root.join("host/server/server.log")).unwrap();
        assert!(
            log.contains("peering_key_unavailable"),
            "server did not log a rejection reason"
        );
        assert!(!log.contains(&original.invitation) && !log.contains(&fresh.invitation));
    });
}

#[test]
fn client_endpoint_uses_saved_host_and_supports_ipv6_without_changing_local_administration() {
    let mut config = ServerPreferences {
        network_hosting: true,
        server_name: "10.0.1.101".into(),
        ..ServerPreferences::default()
    };
    assert_eq!(config.client_url().unwrap(), "https://10.0.1.101:24182");
    config.server_name = "2001:db8::1".into();
    assert_eq!(config.client_url().unwrap(), "https://[2001:db8::1]:24182");
    config.network_hosting = false;
    assert_eq!(config.client_url().unwrap(), "https://127.0.0.1:24182");
}

#[tokio::test]
async fn forbidden_discovery_is_reported_as_discovery_and_does_not_reflect_response_text() {
    use axum::{Router, http::StatusCode, routing::get};
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    let app = Router::new().route(
        "/v1/discovery",
        get(|| async { (StatusCode::FORBIDDEN, "untrusted-secret-response") }),
    );
    let server = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    let key = format!(
        "amux1.{}.{}.{}.{}.{port}",
        uuid::Uuid::new_v4(),
        "0".repeat(64),
        uuid::Uuid::new_v4(),
        "1".repeat(64)
    );
    let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&root).unwrap();
    let fixture = tempfile::tempdir_in(root).unwrap();
    let error = server_controller::pair(
        fixture.path().join("client"),
        "https://127.0.0.1:24182".into(),
        key,
    )
    .await
    .unwrap_err()
    .to_string();
    assert!(
        error.contains("Server discovery failed") && error.contains("403"),
        "{error}"
    );
    assert!(
        !error.contains("operation is unconfirmed")
            && !error.contains("untrusted-secret-response")
            && !error.contains("fixture-secret")
    );
    server.abort();
}
