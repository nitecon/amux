//! Real native client/controller contract. The worker inherits only synthetic
//! fixture settings; no installed gateway, channel token or user state is read.
use amux::{
    delegation_connection::{self, Command as StreamCommand, ConnectionEvent},
    gateway::Gateway,
    gateway_preferences::ServerPreferences,
    gateway_transport::TransportProfile,
    identity::{AgentIdentity, Provider},
    server_controller,
};
use amux_gateway_backend::security::{self, LocalProfile};
use reqwest::Method;
use serde_json::{Value, json};
use std::{
    fs,
    net::TcpListener,
    path::{Path, PathBuf},
    process::{Command, Stdio},
    time::{Duration, Instant},
};
use tokio::sync::mpsc;
use uuid::Uuid;

#[test]
fn real_server_two_clients_renewal_and_lifecycle() {
    let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&root).unwrap();
    let fixture = tempfile::tempdir_in(&root).unwrap();
    fs::write(
        fixture.path().join(".env"),
        "# isolated transport fixture\n",
    )
    .unwrap();
    let log_path = fixture.path().join("worker.log");
    let log = fs::File::create(&log_path).unwrap();
    let mut command = Command::new(std::env::current_exe().unwrap());
    command
        .args([
            "--exact",
            "transport_fixture_worker",
            "--ignored",
            "--nocapture",
        ])
        .current_dir(fixture.path())
        .env_clear();
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
    command
        .env("AMUX_TRANSPORT_FIXTURE", fixture.path())
        .env("AMUX_STATE_DIR", fixture.path().join("identity"))
        .env("GATEWAY_API_KEY", "isolated-old-bearer-key")
        .env("DEFAULT_CHANNEL", "fixture-disabled")
        .env("RUST_LOG", "warn")
        .stdin(Stdio::null())
        .stdout(Stdio::from(log.try_clone().unwrap()))
        .stderr(Stdio::from(log));
    let mut child = command.spawn().unwrap();
    let deadline = Instant::now() + Duration::from_secs(120);
    let result = loop {
        if let Some(status) = child.try_wait().unwrap() {
            break status;
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            let _ = child.wait();
            panic!(
                "isolated transport worker timed out: {}",
                fs::read_to_string(&log_path).unwrap()
            );
        }
        std::thread::sleep(Duration::from_millis(20));
    };
    assert!(
        result.success(),
        "{}",
        fs::read_to_string(log_path).unwrap()
    );
}

fn free_port() -> u16 {
    TcpListener::bind(("127.0.0.1", 0))
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}
fn actor(root: &Path) -> AgentIdentity {
    AgentIdentity {
        session_id: Uuid::new_v4(),
        instance_id: Uuid::new_v4(),
        provider: Provider::Codex,
        os: std::env::consts::OS.into(),
        project_directory: root.into(),
        project_id: Uuid::new_v4(),
        agent_id: "transport-fixture".into(),
        name: "Isolated transport fixture".into(),
    }
}
async fn event(receiver: &mut mpsc::Receiver<ConnectionEvent>, phase: &str) -> ConnectionEvent {
    let value = tokio::time::timeout(Duration::from_secs(35), receiver.recv())
        .await
        .unwrap_or_else(|_| panic!("stream wait timed out: {phase}"))
        .unwrap();
    if let ConnectionEvent::State(state) = &value {
        eprintln!("stream {phase}: {state}");
        assert!(
            !state.contains("rejected the subscription"),
            "unexpected stream protocol rejection during {phase}"
        );
    }
    value
}
fn assert_no_token(directory: &Path, token: &[u8]) {
    for entry in fs::read_dir(directory).unwrap() {
        let path = entry.unwrap().path();
        if path.is_dir() {
            assert_no_token(&path, token);
        } else {
            let bytes = fs::read(&path).unwrap();
            assert!(
                !bytes.windows(token.len()).any(|window| window == token),
                "peering key persisted at {}",
                path.display()
            );
        }
    }
}

#[test]
#[ignore = "isolated subprocess worker, invoked by the parent contract"]
fn transport_fixture_worker() {
    let root =
        PathBuf::from(std::env::var_os("AMUX_TRANSPORT_FIXTURE").expect("parent fixture required"));
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let secure_port = free_port();
        let mut discovery_port = free_port();
        while discovery_port == secure_port { discovery_port = free_port(); }
        let config = ServerPreferences { secure_port, discovery_port, ..ServerPreferences::default() };
        let state = root.join("host");
        let server = server_controller::ensure_local_with_executable(state.clone(), config.clone(), PathBuf::from(env!("CARGO_BIN_EXE_amux"))).await.unwrap();
        assert!(server.owns_process());
        let admin = server.connection();
        let status = server_controller::status(admin.clone()).await.unwrap();
        assert!(status["process_id"].is_u64());
        let first_nonce = status["process_nonce"].as_str().unwrap().to_owned();
        let attached = server_controller::ensure_local_with_executable(state.clone(), config.clone(), PathBuf::from(env!("CARGO_BIN_EXE_amux"))).await.unwrap();
        assert!(!attached.owns_process());
        attached.stop().await.unwrap();
        assert!(server_controller::status(admin.clone()).await.is_ok());
        let mut incompatible = config.clone(); incompatible.discovery_port = free_port();
        assert!(server_controller::ensure_local_with_executable(state.clone(), incompatible, PathBuf::from(env!("CARGO_BIN_EXE_amux"))).await.is_err());
        assert!(server_controller::status(admin.clone()).await.is_ok());

        let invitation = server_controller::invitation(admin.clone(), true).await.unwrap();
        let copied = Command::new(env!("CARGO_BIN_EXE_amux")).args(["server", "peering-key", "--copy", "--profile"])
            .arg(state.join("server")).output().unwrap();
        assert!(copied.status.success(), "{}", String::from_utf8_lossy(&copied.stderr));
        assert!(String::from_utf8(copied.stdout).unwrap().trim() == invitation.invitation, "operator copy returned a different invitation");
        let mut wrong: Vec<_> = invitation.invitation.split('.').map(str::to_owned).collect();
        wrong[2] = "0".repeat(64);
        assert!(server_controller::pair(root.join("bad-pin"), admin.url.clone(), wrong.join(".")).await.is_err());
        let one = server_controller::pair(root.join("peer-one"), admin.url.clone(), invitation.invitation.clone()).await.unwrap();
        let two = server_controller::pair(root.join("peer-two"), admin.url.clone(), invitation.invitation.clone()).await.unwrap();
        assert_ne!(one.node_id, two.node_id);
        let profile = TransportProfile::enrolled(&one).unwrap();
        let namespace = profile.namespace();
        let other = TransportProfile::enrolled(&two).unwrap();
        let gateway = Gateway::from_profile(profile.clone()).unwrap();
        let second = Gateway::from_profile(other.clone()).unwrap();
        let admin_snapshot = TransportProfile::enrolled(&admin).unwrap().snapshot().unwrap();
        let response = admin_snapshot.request(Method::POST, admin_snapshot.base.join("v1/projects").unwrap())
            .json(&json!({"ident":"transport-fixture"})).send().await.unwrap();
        assert!(response.status().is_success());
        assert!(gateway.projects().await.is_ok());
        assert!(second.projects().await.is_ok());
        let snapshot = profile.snapshot().unwrap();
        assert_eq!(snapshot.request(Method::GET, snapshot.base.join("v1/users").unwrap()).send().await.unwrap().status(), 403);
        let local = LocalProfile::load(&one.profile_path).unwrap();
        assert!(!local.certificate_pem_path.file_name().unwrap().to_string_lossy().contains(':'));
        let root_pem = security::read_private(&local.root_pem_path).unwrap();
        let anonymous = reqwest::Client::builder().no_proxy().tls_built_in_root_certs(false)
            .add_root_certificate(reqwest::Certificate::from_pem(&root_pem).unwrap()).build().unwrap();
        for path in ["v1/projects", "unknown-operational-route"] {
            assert_eq!(anonymous.get(snapshot.base.join(path).unwrap()).bearer_auth("isolated-old-bearer-key").send().await.unwrap().status(), 401);
        }
        let mut identity = security::read_private(&local.private_key_pem_path).unwrap();
        identity.extend(security::read_private(&local.certificate_pem_path).unwrap());
        let wrong_name = reqwest::Client::builder().no_proxy().tls_built_in_root_certs(false)
            .add_root_certificate(reqwest::Certificate::from_pem(&root_pem).unwrap())
            .identity(reqwest::Identity::from_pem(&identity).unwrap())
            .resolve("wrong.invalid", ([127,0,0,1],secure_port).into()).build().unwrap();
        assert!(wrong_name.get(format!("https://wrong.invalid:{secure_port}/v1/projects")).send().await.is_err());

        let consumer = Uuid::new_v4();
        let (commands, incoming) = mpsc::channel(64);
        let (outgoing, mut events) = mpsc::channel(64);
        let stream = tokio::spawn(delegation_connection::run_profile(profile.clone(), consumer, None, incoming, outgoing));
        let initial_connection = loop { if let ConnectionEvent::Subscribed { connection, .. } = event(&mut events, "initial subscription").await { break connection; } };
        let caller = actor(&root);
        let task = gateway.request(Method::POST, "transport-fixture", &[], Some(json!({"title":"Real mTLS task"})), Some(&caller)).await.unwrap();
        let received_id = loop { if let ConnectionEvent::Event(item) = event(&mut events, "task event").await { break item.id; } };
        commands.send(StreamCommand { connection: initial_connection, frame: json!({"type":"ack","event_id":received_id,"status":"skipped","message":"isolated transport fixture"}) }).await.unwrap();
        let cursor = loop { if let ConnectionEvent::Recorded { cursor, .. } = event(&mut events, "receipt").await { break cursor; } };
        assert!(cursor >= received_id);
        assert!(second.request(Method::POST, "transport-fixture", &[task["id"].as_str().unwrap(), "comments"], Some(json!({"content":"spoofed native execution"})), Some(&caller)).await.is_err());
        let mut stolen = amux::gateway_stream::connect_profile_with_clock(&other, consumer, Some(cursor)).await.unwrap().0;
        assert_eq!(amux::gateway_stream::next_json(&mut stolen).await.unwrap()["type"], "error");

        let before = LocalProfile::load(&one.profile_path).unwrap();
        server_controller::renew_now(one.clone()).await.unwrap();
        let after = LocalProfile::load(&one.profile_path).unwrap();
        assert_eq!(before.node_id, after.node_id);
        assert_ne!(before.serial, after.serial);
        assert_ne!(before.private_key_pem_path, after.private_key_pem_path);
        assert_eq!(profile.namespace(), namespace);
        assert!(gateway.projects().await.is_ok());
        loop {
            if let ConnectionEvent::Subscribed { connection, cursor: restored, .. } = event(&mut events, "rotated subscription").await
                && connection > initial_connection { assert_eq!(restored, cursor); break; }
        }
        // A second rotation is deferred while the previous generation remains
        // live. Its durable retry must not block ordinary authenticated work.
        assert!(server_controller::renew_now(one.clone()).await.is_err());
        assert!(gateway.projects().await.is_ok());
        assert_eq!(LocalProfile::load(&one.profile_path).unwrap().serial, after.serial);
        let current = profile.current().await.unwrap();
        let memory: Value = current.request(Method::POST, current.base.join("v1/projects/transport-fixture/memories/push").unwrap())
            .json(&json!({"memories":[{"content":"mTLS memory fixture","memory_type":"project"}]}))
            .send().await.unwrap().json().await.unwrap();
        assert_eq!(memory["results"][0]["action"], "created");
        let source = task["id"].as_str().unwrap();
        gateway.claim("transport-fixture", source, &caller).await.unwrap();
        gateway.request(Method::PATCH, "transport-fixture", &[source], Some(json!({"status":"done"})), Some(&caller)).await.unwrap();
        let review = gateway.enqueue_completion_review("transport-fixture", Uuid::parse_str(source).unwrap()).await.unwrap().unwrap();
        let review_id = review["id"].as_str().unwrap();
        assert_eq!(review["status"], "todo");
        assert!(review["owner_origin"].is_null() && review["owner_agent_id"].is_null());
        assert_eq!(gateway.enqueue_completion_review("transport-fixture", Uuid::parse_str(source).unwrap()).await.unwrap().unwrap()["id"], review["id"]);
        assert!(gateway.claim("transport-fixture", review_id, &caller).await.is_err());
        assert!(gateway.request(Method::PATCH, "transport-fixture", &[review_id], Some(json!({"labels":[]})), Some(&caller)).await.is_err());
        let reviewer = actor(&root);
        second.claim("transport-fixture", review_id, &reviewer).await.unwrap();
        second.request(Method::PATCH, "transport-fixture", &[review_id], Some(json!({"status":"done"})), Some(&reviewer)).await.unwrap();
        assert!(second.enqueue_completion_review("transport-fixture", Uuid::parse_str(review_id).unwrap()).await.unwrap().is_none());
        let replacement = server_controller::invitation(admin.clone(), true).await.unwrap();
        assert!(replacement.invitation != invitation.invitation);
        assert!(server_controller::pair(root.join("old-invite"), admin.url.clone(), invitation.invitation.clone()).await.is_err());
        assert!(gateway.projects().await.is_ok());
        #[cfg(windows)]
        assert!(fs::read(&after.private_key_pem_path).unwrap().starts_with(b"AMUX-DPAPI-1\0"));
        stream.abort(); let _ = stream.await; drop(stolen);
        server.stop().await.unwrap();
        let next = server_controller::ensure_local_with_executable(state.clone(), config, PathBuf::from(env!("CARGO_BIN_EXE_amux"))).await.unwrap();
        let next_admin = next.connection();
        assert_eq!(admin.cluster_id, next_admin.cluster_id);
        assert_ne!(server_controller::status(next_admin.clone()).await.unwrap()["process_nonce"], first_nonce);
        server.stop().await.unwrap(); // A stale handle must not stop its successor.
        assert!(server_controller::status(next_admin.clone()).await.is_ok());
        assert!(server_controller::invitation(next_admin, false).await.is_err());
        assert!(gateway.projects().await.is_ok());
        next.stop().await.unwrap();
        // Windows file locks are mandatory. Scan the durable files only after
        // the child and both ownership guards have released their handles.
        drop(next);
        drop(server);
        drop(attached);
        assert_no_token(&state, invitation.invitation.as_bytes());
        assert_no_token(&root.join("peer-one"), invitation.invitation.as_bytes());
    });
}
