//! Local operator CLI and process ownership against isolated real SQLite files.
//! No installed state, inherited gateway credentials, or external plugins.
use amux_gateway_backend::{db, task_events, ui_auth};
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::{
    fs,
    io::Write,
    path::{Path, PathBuf},
    process::{Child, Command, Output, Stdio},
    sync::atomic::{AtomicUsize, Ordering},
    time::{Duration, Instant},
};
use uuid::Uuid;

const KEY: &str = "isolated-operator-fixture-api-key";
const PROJECT: &str = "operator-fixture";

struct Fixture {
    database_path: PathBuf,
    sequence: AtomicUsize,
    instance_id: String,
    root: tempfile::TempDir,
}

impl Fixture {
    fn new() -> Self {
        let fixtures = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        fs::create_dir_all(&fixtures).unwrap();
        let root = tempfile::tempdir_in(fixtures).unwrap();
        // Stop dotenv's ancestor search before it can read a checkout/user .env.
        fs::write(root.path().join(".env"), "# isolated operator fixture\n").unwrap();
        fs::create_dir(root.path().join("tmp")).unwrap();
        fs::create_dir(root.path().join("profile")).unwrap();
        Self {
            database_path: root.path().join("profile/application.sqlite"),
            sequence: AtomicUsize::new(0),
            instance_id: Uuid::new_v4().to_string(),
            root,
        }
    }

    fn command(&self, args: &[&str]) -> Command {
        let mut command = Command::new(env!("CARGO_BIN_EXE_amux"));
        command.args(args).current_dir(self.root.path()).env_clear();
        for name in ["PATH", "SystemRoot", "WINDIR"] {
            if let Some(value) = std::env::var_os(name) {
                command.env(name, value);
            }
        }
        command
            .env("TMPDIR", self.root.path().join("tmp"))
            .env("TMP", self.root.path().join("tmp"))
            .env("TEMP", self.root.path().join("tmp"))
            .env("DATABASE_BACKEND", "sqlite")
            .env("DATABASE_PATH", &self.database_path)
            .env("GATEWAY_API_KEY", KEY)
            .env("GATEWAY_HOST", "127.0.0.1")
            .env("GATEWAY_PORT", "0")
            .env("RUST_LOG", "warn");
        command
    }

    fn secure_command(&self, profile: &Path) -> Command {
        let mut command = self.command(&["server", "secure"]);
        command
            .arg("--profile")
            .arg(profile)
            .args([
                "--bind",
                "127.0.0.1:0",
                "--discovery-bind",
                "127.0.0.1:0",
                "--name",
                "127.0.0.1",
                "--instance-id",
            ])
            .arg(&self.instance_id);
        command
    }

    fn database(&self) -> db::Db {
        fs::create_dir_all(self.database_path.parent().unwrap()).unwrap();
        db::open_config(&db::DatabaseConfig::sqlite(
            self.database_path.to_string_lossy(),
        ))
        .unwrap()
    }

    fn seed_project(&self) {
        let database = self.database();
        db::insert_project(
            &database.lock().unwrap(),
            &db::Project {
                ident: PROJECT.into(),
                channel_name: "discord".into(),
                room_id: String::new(),
                last_msg_id: None,
                created_at: db::now_ms(),
                repo_provider: None,
                repo_namespace: None,
                repo_name: None,
                repo_full_name: None,
                kind: db::PROJECT_KIND_ADHOC.into(),
                archived_at: None,
                canonical_remote: None,
            },
        )
        .unwrap();
    }
}

fn output(command: &mut Command, input: Option<&[u8]>) -> Output {
    let mut child = command
        .stdin(if input.is_some() {
            Stdio::piped()
        } else {
            Stdio::null()
        })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    if let Some(input) = input {
        child.stdin.take().unwrap().write_all(input).unwrap();
    }
    let deadline = Instant::now() + Duration::from_secs(20);
    loop {
        if child.try_wait().unwrap().is_some() {
            return child.wait_with_output().unwrap();
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            let _ = child.wait();
            panic!("isolated operator command did not exit within twenty seconds");
        }
        std::thread::sleep(Duration::from_millis(10));
    }
}

fn success(output: &Output) -> String {
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout.clone()).unwrap()
}

struct RunningServer {
    child: Child,
    stdout: PathBuf,
    stderr: PathBuf,
}

impl RunningServer {
    fn start(fixture: &Fixture) -> Self {
        let sequence = fixture.sequence.fetch_add(1, Ordering::SeqCst);
        let stdout = fixture
            .root
            .path()
            .join(format!("server-{sequence}.stdout"));
        let stderr = fixture
            .root
            .path()
            .join(format!("server-{sequence}.stderr"));
        let child = fixture
            .secure_command(&fixture.root.path().join("profile"))
            .stdin(Stdio::null())
            .stdout(Stdio::from(fs::File::create(&stdout).unwrap()))
            .stderr(Stdio::from(fs::File::create(&stderr).unwrap()))
            .spawn()
            .unwrap();
        Self {
            child,
            stdout,
            stderr,
        }
    }

    async fn ready(&mut self) -> String {
        let deadline = Instant::now() + Duration::from_secs(15);
        let profile_path = self
            .stdout
            .parent()
            .unwrap()
            .join("profile/local-client.json");
        loop {
            let logs = fs::read_to_string(&self.stdout).unwrap();
            if let Ok(local) = amux_gateway_backend::security::LocalProfile::load(&profile_path) {
                let connection = amux::gateway_preferences::EnrolledConnection {
                    url: local.url.clone(),
                    cluster_id: local.cluster_id,
                    node_id: local.node_id,
                    profile_path: profile_path.clone(),
                };
                if let Ok(profile) =
                    amux::gateway_transport::TransportProfile::enrolled(&connection)
                    && let Ok(gateway) = amux::gateway::Gateway::from_profile(profile)
                    && gateway.projects().await.is_ok()
                {
                    return local.url;
                }
            }
            assert!(
                self.child.try_wait().unwrap().is_none(),
                "isolated server exited: {}",
                fs::read_to_string(&self.stderr).unwrap()
            );
            assert!(
                Instant::now() < deadline,
                "isolated server never became ready: {logs}"
            );
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    }

    fn stop(&mut self) {
        let _ = self.child.kill();
        self.child.wait().unwrap();
    }
}

impl Drop for RunningServer {
    fn drop(&mut self) {
        self.stop();
    }
}

#[tokio::test]
async fn profile_lock_rejects_duplicate_and_alias_servers_then_releases_after_process_exit() {
    let fixture = Fixture::new();
    let mut first = RunningServer::start(&fixture);
    first.ready().await;
    let duplicate = output(
        &mut fixture.secure_command(&fixture.root.path().join("profile")),
        None,
    );
    assert!(!duplicate.status.success());
    assert!(String::from_utf8_lossy(&duplicate.stderr).contains("profile is already owned"));
    #[cfg(unix)]
    {
        let alias = fixture.root.path().join("same-profile");
        std::os::unix::fs::symlink(fixture.root.path().join("profile"), &alias).unwrap();
        let duplicate = output(&mut fixture.secure_command(&alias), None);
        assert!(!duplicate.status.success());
        assert!(String::from_utf8_lossy(&duplicate.stderr).contains("profile is already owned"));
    }
    first.stop();
    let mut lock = fixture.database_path.as_os_str().to_owned();
    lock.push(".amux-server.lock");
    assert!(
        PathBuf::from(lock).exists(),
        "stale sidecar existence is not live ownership"
    );
    let mut reopened = RunningServer::start(&fixture);
    reopened.ready().await;
    reopened.stop();
}

#[test]
fn machine_operator_issues_replaces_and_revokes_only_hashed_credentials() {
    let fixture = Fixture::new();
    fixture.seed_project();
    let instance = Uuid::from_u128(0x1ca806da_9fcf_413f_8765_843533f30001).to_string();
    let issue = || {
        output(
            &mut fixture.command(&[
                "server",
                "task-machine",
                "issue",
                PROJECT,
                &instance,
                std::env::consts::OS,
            ]),
            None,
        )
    };
    let first = success(&issue()).trim().to_owned();
    assert_eq!(first.len(), 64);
    assert!(first.bytes().all(|byte| byte.is_ascii_hexdigit()));
    let second = success(&issue()).trim().to_owned();
    assert_ne!(first, second);
    let database = fixture.database();
    {
        let conn = database.lock().unwrap();
        let (hash, revoked): (String,bool) = conn.query_row(
            "SELECT key_hash,revoked FROM task_machines WHERE project_ident=?1 AND instance_id=?2",
            (PROJECT,instance.as_str()), |row| Ok((row.get(0)?,row.get(1)?))
        ).unwrap();
        assert_eq!(hash, format!("{:x}", Sha256::digest(second.as_bytes())));
        assert_ne!(hash, second);
        assert!(!revoked);
    }
    success(&output(
        &mut fixture.command(&["server", "task-machine", "revoke", PROJECT, &instance]),
        None,
    ));
    let revoked: bool = database
        .lock()
        .unwrap()
        .query_row(
            "SELECT revoked FROM task_machines WHERE project_ident=?1 AND instance_id=?2",
            (PROJECT, instance.as_str()),
            |row| row.get(0),
        )
        .unwrap();
    assert!(revoked);
}

#[test]
fn task_operator_inspects_and_recovers_exact_claim_with_a_durable_reason() {
    let fixture = Fixture::new();
    fixture.seed_project();
    let database = fixture.database();
    let task = {
        let conn = database.lock().unwrap();
        let task = db::insert_task(
            &conn,
            PROJECT,
            "Orphaned fixture",
            None,
            None,
            &[],
            None,
            "fixture-executor",
        )
        .unwrap();
        let origin = task_events::AgentOrigin {
            session_id: Uuid::from_u128(0x1ca806da_9fcf_413f_8765_843533f30002).to_string(),
            instance_id: Uuid::from_u128(0x1ca806da_9fcf_413f_8765_843533f30003).to_string(),
            provider: "codex".into(),
            os: std::env::consts::OS.into(),
        };
        task_events::mutation(&conn, Some(&origin), |conn| {
            db::update_task(
                conn,
                PROJECT,
                &task.id,
                &db::TaskUpdate {
                    goal: None,
                    status: Some("in_progress"),
                    owner_agent_id: Some(Some("fixture-executor")),
                    rank: None,
                    title: None,
                    description: None,
                    details: None,
                    labels: None,
                    hostname: None,
                },
                Some("fixture-executor"),
            )
        })
        .unwrap()
        .unwrap()
    };
    let inspected: Value = serde_json::from_str(&success(&output(
        &mut fixture.command(&["server", "task", "claim-info", PROJECT, &task.id]),
        None,
    )))
    .unwrap();
    assert_eq!(inspected["id"], task.id);
    assert_eq!(inspected["status"], "in_progress");
    assert_eq!(
        inspected["claim_token"],
        db::task_claim_token(&task).unwrap()
    );
    let rejected = output(
        &mut fixture.command(&[
            "server",
            "task",
            "recover",
            PROJECT,
            &task.id,
            "--expected-claim",
            "stale-token",
            "--reason",
            "inspect first",
        ]),
        None,
    );
    assert!(!rejected.status.success());
    assert!(String::from_utf8_lossy(&rejected.stderr).contains("claim changed"));
    let recovered: Value = serde_json::from_str(&success(&output(
        &mut fixture.command(&[
            "server",
            "task",
            "recover",
            PROJECT,
            &task.id,
            "--expected-claim",
            inspected["claim_token"].as_str().unwrap(),
            "--reason",
            "verified original executor is gone",
        ]),
        None,
    )))
    .unwrap();
    assert_eq!(recovered["status"], "todo");
    assert_eq!(recovered["owner_agent_id"], Value::Null);
    let stored = db::get_task_detail(&database.lock().unwrap(), PROJECT, &task.id)
        .unwrap()
        .unwrap();
    assert!(stored.task.owner_origin.is_none());
    assert!(stored.task.started_at.is_none());
    assert!(stored.comments.iter().any(|comment| {
        comment.author == "gateway-admin"
            && comment
                .content
                .contains("verified original executor is gone")
    }));
}

#[test]
fn user_operator_preserves_env_stdin_passwords_roles_and_session_invalidation() {
    let fixture = Fixture::new();
    let first_password = "first-fixture-password";
    let second_password = "second-fixture-password";
    let added = output(
        fixture
            .command(&[
                "server",
                "user",
                "add",
                "fixtureadmin",
                "--display-name",
                "Fixture Admin",
                "--admin",
            ])
            .env("GATEWAY_USER_PASSWORD", first_password),
        None,
    );
    assert!(success(&added).contains("created fixtureadmin (Fixture Admin) as admin"));
    assert!(!success(&added).contains(first_password));
    let member = output(
        &mut fixture.command(&["server", "user", "add", "fixturemember"]),
        Some(b"stdin-fixture-password\r\n"),
    );
    assert!(success(&member).contains("as member"));
    let listing = success(&output(
        &mut fixture.command(&["server", "user", "list"]),
        None,
    ));
    assert!(listing.contains("fixtureadmin") && listing.contains("Fixture Admin"));
    assert!(listing.contains("fixturemember"));
    let database = fixture.database();
    let before = db::get_user_by_username(&database.lock().unwrap(), "fixtureadmin")
        .unwrap()
        .unwrap();
    assert!(before.is_admin());
    assert!(ui_auth::verify_password(
        first_password,
        &before.password_hash
    ));
    let member = db::get_user_by_username(&database.lock().unwrap(), "fixturemember")
        .unwrap()
        .unwrap();
    assert!(ui_auth::verify_password(
        "stdin-fixture-password",
        &member.password_hash
    ));
    let now = db::now_ms();
    let old_token = ui_auth::session_token(KEY, &before, now + 60_000);
    success(&output(
        fixture
            .command(&["server", "user", "reset-password", "fixtureadmin"])
            .env("GATEWAY_USER_PASSWORD", second_password),
        None,
    ));
    let after = db::get_user_by_username(&database.lock().unwrap(), "fixtureadmin")
        .unwrap()
        .unwrap();
    assert_eq!(after.session_epoch, before.session_epoch + 1);
    assert!(ui_auth::verify_password(
        second_password,
        &after.password_hash
    ));
    assert!(!ui_auth::verify_password(
        first_password,
        &after.password_hash
    ));
    assert!(!ui_auth::verify_session_token(KEY, &after, &old_token, now));
    let rejected = output(
        fixture
            .command(&["server", "user", "add", "invalidpassword"])
            .env("GATEWAY_USER_PASSWORD", "short"),
        None,
    );
    assert!(!rejected.status.success());
    assert!(
        db::get_user_by_username(&database.lock().unwrap(), "invalidpassword")
            .unwrap()
            .is_none()
    );
}
