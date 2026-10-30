//! Native server ownership, pinned enrollment and recoverable client renewal.
use crate::{
    gateway_preferences::{EnrolledConnection, ServerPreferences},
    gateway_transport::{TransportProfile, origin},
};
use amux_gateway_backend::security::{
    self, ClientIdentity, Discovery, EnrollmentRequest, EnrollmentResponse, Invitation,
    LocalProfile, NodeStatus, RenewalRequest,
};
use anyhow::{Context, Result, ensure};
use reqwest::{Method, Url};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::{
    fmt, fs,
    io::Cursor,
    path::{Path, PathBuf},
    process::Stdio,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::process::{Child, Command};
use uuid::Uuid;

struct Process {
    child: Mutex<Option<Child>>,
    nonce: String,
    _parent_lease: fs::File,
    lease_path: PathBuf,
    #[cfg(windows)]
    _job: std::os::windows::io::OwnedHandle,
}
impl Drop for Process {
    fn drop(&mut self) {
        if let Ok(child) = self.child.get_mut()
            && let Some(mut child) = child.take()
        {
            let _ = child.start_kill();
        }
        let _ = fs::remove_file(&self.lease_path);
    }
}

#[derive(Clone)]
pub struct LocalServer {
    connection: EnrolledConnection,
    process: Option<Arc<Process>>,
}
impl fmt::Debug for LocalServer {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("LocalServer")
            .field("owned", &self.owns_process())
            .finish()
    }
}
impl LocalServer {
    pub fn connection(&self) -> EnrolledConnection {
        self.connection.clone()
    }
    pub fn owns_process(&self) -> bool {
        self.process.is_some()
    }
    pub async fn stop(&self) -> Result<()> {
        if self.process.is_none() {
            return Ok(());
        }
        let mut child = self
            .process
            .as_ref()
            .unwrap()
            .child
            .lock()
            .map_err(|_| anyhow::anyhow!("server ownership unavailable"))?
            .take();
        if let Some(child) = child.as_mut() {
            if child.try_wait()?.is_some() {
                return Ok(());
            }
            let shutdown = exchange::<Value>(&self.connection, Method::POST, "/v1/server/shutdown",
                Some(serde_json::json!({"expected_process_nonce":self.process.as_ref().unwrap().nonce}))).await;
            if shutdown.is_ok()
                && matches!(
                    tokio::time::timeout(Duration::from_secs(8), child.wait()).await,
                    Ok(Ok(_))
                )
            {
                return Ok(());
            }
            child.start_kill().ok();
            child
                .wait()
                .await
                .context("wait for owned server to stop")?;
        }
        Ok(())
    }
}

fn connection(profile: &LocalProfile, path: &Path) -> EnrolledConnection {
    EnrolledConnection {
        url: profile.url.clone(),
        cluster_id: profile.cluster_id.clone(),
        node_id: profile.node_id.clone(),
        profile_path: path.to_owned(),
    }
}

fn installation_id(state: &Path) -> Result<String> {
    fs::create_dir_all(state)?;
    let lock = fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(state.join("installation-id.lock"))?;
    lock.try_lock()
        .context("installation identity is being initialized")?;
    let path = state.join("installation-id");
    let value = if path.exists() {
        String::from_utf8(security::read_private(&path)?)?
    } else {
        let value = Uuid::new_v4().to_string();
        security::write_private(&path, value.as_bytes())?;
        value
    };
    let id = Uuid::parse_str(&value).context("invalid installation identity")?;
    ensure!(
        !id.is_nil() && id.to_string() == value,
        "invalid installation identity"
    );
    Ok(value)
}

pub async fn ensure_local(state_dir: PathBuf, config: ServerPreferences) -> Result<LocalServer> {
    let cli = crate::cli_path::cli_beside(&std::env::current_exe()?)?;
    ensure_local_with_executable(state_dir, config, cli).await
}

/// Explicit executable injection also lets lifecycle contracts use an isolated
/// built CLI rather than launching anything from an installed application.
pub async fn ensure_local_with_executable(
    state_dir: PathBuf,
    config: ServerPreferences,
    executable: PathBuf,
) -> Result<LocalServer> {
    ensure!(
        config.secure_port != 0
            && config.discovery_port != 0
            && config.secure_port != config.discovery_port,
        "choose different nonzero secure and discovery ports"
    );
    ensure!(
        !config.server_name.trim().is_empty()
            && config.server_name.len() <= 253
            && !config.server_name.chars().any(char::is_control),
        "enter a valid server name or IP address"
    );
    fs::create_dir_all(&state_dir)?;
    let state_dir = state_dir.canonicalize()?;
    let profile_dir = state_dir.join("server");
    fs::create_dir_all(&profile_dir)?;
    let manifest = security::local_profile_path(&profile_dir);
    if let Ok(profile) = LocalProfile::load(&manifest) {
        let connection = connection(&profile, &manifest);
        if let Ok(status) = probe(&connection).await {
            verify_hosting(&status, &config)?;
            return Ok(LocalServer {
                connection,
                process: None,
            });
        }
    }
    let instance = installation_id(&state_dir)?;
    let lease_path = profile_dir.join(format!("parent-{}.lock", Uuid::new_v4()));
    let parent_lease = fs::OpenOptions::new()
        .create_new(true)
        .read(true)
        .write(true)
        .open(&lease_path)?;
    parent_lease
        .try_lock()
        .context("hold native server ownership")?;
    let bind = if config.network_hosting {
        "0.0.0.0"
    } else {
        "127.0.0.1"
    };
    let log = fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(profile_dir.join("server.log"))?;
    let mut command = Command::new(executable);
    command
        .args(["server", "secure", "--profile"])
        .arg(&profile_dir)
        .arg("--parent-lease")
        .arg(&lease_path)
        .args([
            "--instance-id",
            &instance,
            "--bind",
            &format!("{bind}:{}", config.secure_port),
            "--discovery-bind",
            &format!("{bind}:{}", config.discovery_port),
            "--name",
            config.server_name.trim(),
        ])
        .current_dir(&profile_dir)
        .env("AMUX_STATE_DIR", &state_dir)
        .stdin(Stdio::null())
        .stdout(Stdio::from(log.try_clone()?))
        .stderr(Stdio::from(log))
        .kill_on_drop(true);
    #[cfg(target_os = "linux")]
    {
        let parent = std::process::id();
        // SAFETY: only async-signal-safe operations execute after fork.
        unsafe {
            command.pre_exec(move || {
                if libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGKILL) == -1 {
                    return Err(std::io::Error::last_os_error());
                }
                if libc::getppid() as u32 != parent {
                    return Err(std::io::Error::other("server owner exited during startup"));
                }
                Ok(())
            });
        }
    }
    #[cfg(windows)]
    command.creation_flags(windows_sys::Win32::System::Threading::CREATE_NO_WINDOW);
    let child = command.spawn().context("start the bundled amux server")?;
    #[cfg(windows)]
    let job = owned_job(&child)?;
    let child_id = child.id().context("server process identity unavailable")?;
    // The temporary child guard kills an unsuccessful start, including a
    // competing process which lost the profile lock. It never owns an older host.
    let mut child = Some(child);
    let deadline = tokio::time::Instant::now() + Duration::from_secs(30);
    loop {
        let exited = child
            .as_mut()
            .context("server process unavailable")?
            .try_wait()?;
        ensure!(
            exited.is_none(),
            "local server exited during startup; check its server.log"
        );
        if let Ok(profile) = LocalProfile::load(&manifest) {
            let connection = connection(&profile, &manifest);
            if let Ok(status) = probe(&connection).await
                && status["process_id"].as_u64() == Some(u64::from(child_id))
            {
                verify_hosting(&status, &config)?;
                let nonce = status["process_nonce"]
                    .as_str()
                    .context("server process identity unavailable")?
                    .to_owned();
                Uuid::parse_str(&nonce).context("invalid server process identity")?;
                let process = Arc::new(Process {
                    child: Mutex::new(child.take()),
                    nonce,
                    _parent_lease: parent_lease,
                    lease_path,
                    #[cfg(windows)]
                    _job: job,
                });
                return Ok(LocalServer {
                    connection,
                    process: Some(process),
                });
            }
        }
        ensure!(
            tokio::time::Instant::now() < deadline,
            "local server did not become ready; check its bind addresses and server.log"
        );
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

#[cfg(windows)]
fn owned_job(child: &Child) -> Result<std::os::windows::io::OwnedHandle> {
    use std::os::windows::io::FromRawHandle;
    use windows_sys::Win32::System::JobObjects::*;
    // SAFETY: handles originate in Windows; the owned handle closes once, and
    // the size and pointer describe the initialized information structure.
    unsafe {
        let job = CreateJobObjectW(std::ptr::null(), std::ptr::null());
        ensure!(!job.is_null(), "create server ownership job");
        let owned = std::os::windows::io::OwnedHandle::from_raw_handle(job);
        let mut limits: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = std::mem::zeroed();
        limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
        ensure!(
            SetInformationJobObject(
                job,
                JobObjectExtendedLimitInformation,
                (&limits as *const JOBOBJECT_EXTENDED_LIMIT_INFORMATION).cast(),
                std::mem::size_of_val(&limits) as u32
            ) != 0,
            "set server ownership job limits"
        );
        ensure!(
            AssignProcessToJobObject(
                job,
                child
                    .raw_handle()
                    .context("server process handle unavailable")?
                    .cast()
            ) != 0,
            "assign owned server process"
        );
        Ok(owned)
    }
}

fn verify_hosting(status: &Value, config: &ServerPreferences) -> Result<()> {
    let secure: std::net::SocketAddr = status["secure_bind"]
        .as_str()
        .context("server bind information unavailable")?
        .parse()?;
    let discovery: std::net::SocketAddr = status["discovery_bind"]
        .as_str()
        .context("server discovery information unavailable")?
        .parse()?;
    let matches = secure.port() == config.secure_port
        && discovery.port() == config.discovery_port
        && if config.network_hosting {
            secure.ip().is_unspecified() && discovery.ip().is_unspecified()
        } else {
            secure.ip().is_loopback() && discovery.ip().is_loopback()
        };
    let expected: std::collections::BTreeSet<_> =
        [config.server_name.trim(), "localhost", "127.0.0.1", "::1"]
            .into_iter()
            .collect();
    let actual: std::collections::BTreeSet<_> = status["server_names"]
        .as_array()
        .context("server names unavailable")?
        .iter()
        .filter_map(Value::as_str)
        .collect();
    ensure!(
        matches && expected == actual,
        "an existing server owns this profile with different hosting settings; stop its owning process before changing them"
    );
    Ok(())
}

async fn probe(connection: &EnrolledConnection) -> Result<Value> {
    let profile = TransportProfile::enrolled(connection)?;
    let snapshot = profile.snapshot()?;
    let response = snapshot
        .request(Method::GET, snapshot.base.join("v1/server/status")?)
        .timeout(Duration::from_millis(600))
        .send()
        .await?;
    let value: Value = bounded_json(response).await?;
    ensure!(
        value["cluster_id"] == connection.cluster_id && value["running"] == true,
        "local server identity is unconfirmed"
    );
    Ok(value)
}

async fn bounded_json<T: DeserializeOwned>(mut response: reqwest::Response) -> Result<T> {
    let status = response.status();
    let mut body = Vec::new();
    while let Some(chunk) = response.chunk().await? {
        ensure!(
            body.len() + chunk.len() <= 64 * 1024,
            "server control response exceeds limit"
        );
        body.extend_from_slice(&chunk);
    }
    if !status.is_success() {
        let reason = serde_json::from_slice::<Value>(&body)
            .ok()
            .and_then(|value| {
                value["error"]
                    .as_str()
                    .and_then(security::EnrollmentRejection::from_code)
            });
        if let Some(reason) = reason {
            anyhow::bail!(
                "server rejected request (HTTP {}): {reason}",
                status.as_u16()
            );
        }
        if matches!(status.as_u16(), 401 | 403) {
            anyhow::bail!(
                "server denied request (HTTP {}); check the server endpoint and access policy",
                status.as_u16()
            );
        }
        anyhow::bail!("server returned {status}; operation is unconfirmed");
    }
    if body.is_empty() {
        body.extend_from_slice(b"null");
    }
    serde_json::from_slice(&body).context("invalid server control response")
}

async fn pairing_json<T: DeserializeOwned>(response: reqwest::Response, stage: &str) -> Result<T> {
    bounded_json(response)
        .await
        .map_err(|error| anyhow::anyhow!("{stage}: {error}"))
}

async fn exchange<T: DeserializeOwned>(
    connection: &EnrolledConnection,
    method: Method,
    path: &str,
    body: Option<Value>,
) -> Result<T> {
    let snapshot = TransportProfile::enrolled(connection)?.snapshot()?;
    let mut request = snapshot.request(method, snapshot.base.join(path.trim_start_matches('/'))?);
    if let Some(body) = body {
        request = request.json(&body);
    }
    bounded_json(request.send().await.context("server control unavailable")?).await
}

pub async fn invitation(connection: EnrolledConnection, generate: bool) -> Result<Invitation> {
    exchange(
        &connection,
        if generate { Method::POST } else { Method::GET },
        "/v1/server/invitation",
        None,
    )
    .await
}
pub async fn status(connection: EnrolledConnection) -> Result<Value> {
    exchange(&connection, Method::GET, "/v1/server/status", None).await
}

fn root_fingerprint(pem: &str) -> Result<String> {
    let certs =
        rustls_pemfile::certs(&mut Cursor::new(pem)).collect::<std::io::Result<Vec<_>>>()?;
    ensure!(
        certs.len() == 1,
        "discovery must provide one installation root"
    );
    Ok(format!("{:x}", Sha256::digest(certs[0].as_ref())))
}

fn invitation_metadata(value: &str) -> Result<(&str, &str, &str, u16)> {
    ensure!(value.len() <= 1024, "invalid peering key");
    let parts: Vec<_> = value.split('.').collect();
    ensure!(
        matches!(parts.len(), 5 | 6) && parts[0] == "amux1",
        "invalid peering key"
    );
    for part in [parts[1], parts[3]] {
        let id = Uuid::parse_str(part).context("invalid peering key")?;
        ensure!(
            !id.is_nil() && id.to_string() == part,
            "invalid peering key"
        );
    }
    for part in [parts[2], parts[4]] {
        ensure!(
            part.len() == 64 && part.bytes().all(|b| b.is_ascii_hexdigit()),
            "invalid peering key"
        );
    }
    let port = if parts.len() == 6 {
        parts[5].parse::<u16>().context("invalid discovery port")?
    } else {
        24181
    };
    ensure!(port != 0, "invalid discovery port");
    Ok((parts[1], parts[2], parts[3], port))
}

#[derive(Serialize, Deserialize)]
struct Pending {
    request_id: String,
    csr_pem: String,
    key_path: PathBuf,
    root_path: PathBuf,
    invitation_id: Option<String>,
    response: Option<EnrollmentResponse>,
}

fn save_pending(path: &Path, pending: &Pending) -> Result<()> {
    security::write_private(path, &serde_json::to_vec(pending)?)
}
fn load_pending(path: &Path) -> Result<Pending> {
    serde_json::from_slice(&security::read_private(path)?)
        .context("invalid pending credential operation")
}
fn new_pending(directory: &Path, invitation_id: Option<String>) -> Result<Pending> {
    let request_id = Uuid::new_v4().to_string();
    let identity = ClientIdentity::generate()?;
    let key_path = directory.join(format!("client-key-{request_id}.pem"));
    security::write_private(&key_path, identity.private_key_pem.as_bytes())?;
    Ok(Pending {
        request_id,
        csr_pem: identity.csr_pem,
        key_path,
        root_path: directory.join("root.pem"),
        invitation_id,
        response: None,
    })
}

fn pinned_client(root_pem: &str) -> Result<reqwest::Client> {
    let builder = reqwest::Client::builder()
        .no_proxy()
        .timeout(Duration::from_secs(15))
        .redirect(reqwest::redirect::Policy::none())
        .tls_built_in_root_certs(false)
        .add_root_certificate(reqwest::Certificate::from_pem(root_pem.as_bytes())?);
    Ok(builder.build()?)
}

async fn activate_pending(
    base: &Url,
    cluster: &str,
    instance: &str,
    pending: &Pending,
    active_path: &Path,
) -> Result<EnrolledConnection> {
    let response = pending
        .response
        .as_ref()
        .context("certificate issuance is unconfirmed")?;
    ensure!(
        response.cluster_id == cluster,
        "certificate belongs to another server"
    );
    let info = security::certificate_info(&response.certificate_pem)?;
    ensure!(
        info.node_id == response.node_id
            && info.serial == response.serial
            && info.expires_at == response.expires_at,
        "issued certificate identity is unconfirmed"
    );
    let node = Uuid::parse_str(&response.node_id).context("invalid issued node identity")?;
    ensure!(
        !node.is_nil() && node.to_string() == response.node_id,
        "invalid issued node identity"
    );
    let cert_path = active_path
        .parent()
        .context("credential profile directory absent")?
        .join(format!(
            "client-{:x}.pem",
            Sha256::digest(response.serial.as_bytes())
        ));
    security::write_private(&cert_path, response.certificate_pem.as_bytes())?;
    let profile = LocalProfile {
        version: 1,
        cluster_id: cluster.into(),
        node_id: response.node_id.clone(),
        instance_id: instance.into(),
        url: base.to_string(),
        root_pem_path: pending.root_path.clone(),
        certificate_pem_path: cert_path,
        private_key_pem_path: pending.key_path.clone(),
        serial: response.serial.clone(),
        expires_at: response.expires_at,
    };
    let staged_path = active_path.with_extension("staged.json");
    profile.save(&staged_path)?;
    let staged = connection(&profile, &staged_path);
    let activated: NodeStatus = exchange(&staged, Method::POST, "/v1/enrollment/activate", None)
        .await
        .map_err(|error| anyhow::anyhow!("Certificate activation failed: {error}"))?;
    ensure!(
        activated.node_id == response.node_id
            && activated.current_serial == response.serial
            && !activated.revoked,
        "certificate activation is unconfirmed"
    );
    profile.save(active_path)?;
    let _ = fs::remove_file(staged_path);
    Ok(connection(&profile, active_path))
}

fn pairing_base(url: &str) -> Result<Url> {
    let mut base = origin(url, true)?;
    let explicit_port = url
        .trim()
        .split_once("://")
        .and_then(|(_, authority)| authority.trim_end_matches('/').rsplit_once(':'))
        .and_then(|(_, port)| port.parse::<u16>().ok());
    let port = explicit_port.unwrap_or(24182);
    ensure!(port != 0, "invalid server port");
    base.set_port(Some(port))
        .map_err(|_| anyhow::anyhow!("invalid server port"))?;
    Ok(base)
}

pub async fn pair(
    state_dir: PathBuf,
    url: String,
    invitation: String,
) -> Result<EnrolledConnection> {
    let (cluster, pin, invitation_id, discovery_port) = invitation_metadata(invitation.trim())?;
    let base = pairing_base(&url)?;
    let mut discovery_url = base.clone();
    discovery_url
        .set_scheme("http")
        .map_err(|_| anyhow::anyhow!("invalid discovery address"))?;
    discovery_url
        .set_port(Some(discovery_port))
        .map_err(|_| anyhow::anyhow!("invalid discovery port"))?;
    discovery_url.set_path("/v1/discovery");
    let builder = reqwest::Client::builder()
        .no_proxy()
        .timeout(Duration::from_secs(10))
        .redirect(reqwest::redirect::Policy::none());
    let public = builder.build()?;
    let discovery: Discovery = pairing_json(
        public
            .get(discovery_url)
            .send()
            .await
            .context("Server discovery unavailable; check the address and discovery port")?,
        "Server discovery failed",
    )
    .await?;
    ensure!(
        discovery.version == 1
            && discovery.cluster_id == cluster
            && discovery.root_sha256 == pin
            && root_fingerprint(&discovery.root_pem)? == pin
            && base.port_or_known_default() == Some(discovery.secure_port),
        "server discovery does not match the peering key"
    );
    fs::create_dir_all(&state_dir)?;
    let state_dir = state_dir.canonicalize()?;
    let directory = state_dir.join("peers").join(cluster);
    fs::create_dir_all(&directory)?;
    let lock = fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(directory.join("pair.lock"))?;
    lock.try_lock().context("pairing already in progress")?;
    let path = directory.join("pair-pending.json");
    let mut pending = if path.exists() {
        load_pending(&path)?
    } else {
        new_pending(&directory, Some(invitation_id.into()))?
    };
    if pending
        .response
        .as_ref()
        .is_some_and(|response| response.expires_at <= security::now_ms())
    {
        ensure!(
            pending.invitation_id.as_deref() != Some(invitation_id),
            "pairing certificate expired; generate a new peering key"
        );
        pending = new_pending(&directory, Some(invitation_id.into()))?;
    }
    security::write_private(&pending.root_path, discovery.root_pem.as_bytes())?;
    let client = pinned_client(&discovery.root_pem)?;
    if pending.response.is_none() {
        let receipt = client
            .get(base.join(&format!("v1/enrollment/requests/{}", pending.request_id))?)
            .send()
            .await?;
        if receipt.status().is_success() {
            pending.response = Some(pairing_json(receipt, "Enrollment receipt failed").await?);
        } else {
            ensure!(
                receipt.status() == reqwest::StatusCode::NOT_FOUND,
                "Enrollment receipt failed (HTTP {}); existing pairing state was retained",
                receipt.status().as_u16()
            );
            if pending.invitation_id.as_deref() != Some(invitation_id) {
                pending = new_pending(&directory, Some(invitation_id.into()))?;
                security::write_private(&pending.root_path, discovery.root_pem.as_bytes())?;
            }
            save_pending(&path, &pending)?;
            let request = EnrollmentRequest {
                request_id: pending.request_id.clone(),
                invitation: invitation.trim().into(),
                csr_pem: pending.csr_pem.clone(),
                instance_id: installation_id(&state_dir)?,
            };
            pending.response = Some(
                pairing_json(
                    client
                        .post(base.join("v1/enrollment/requests")?)
                        .json(&request)
                        .send()
                        .await
                        .context("Enrollment request unavailable; check the secure port")?,
                    "Enrollment failed",
                )
                .await?,
            );
        }
    }
    let response = pending.response.as_ref().unwrap();
    ensure!(
        root_fingerprint(&response.root_pem)? == pin,
        "issued certificate root differs from the peering key"
    );
    save_pending(&path, &pending)?;
    let result = activate_pending(
        &base,
        cluster,
        &installation_id(&state_dir)?,
        &pending,
        &directory.join("client-profile.json"),
    )
    .await?;
    let _ = fs::remove_file(path);
    Ok(result)
}

#[cfg(test)]
mod pairing_address_tests {
    use super::pairing_base;

    #[test]
    fn explicit_default_port_and_omitted_port_produce_the_same_pairing_url() {
        for host in ["10.0.1.101", "[2001:db8::1]"] {
            let implicit = pairing_base(&format!("https://{host}")).unwrap();
            assert_eq!(implicit.port(), Some(24182));
            assert_eq!(
                implicit,
                pairing_base(&format!("https://{host}:24182")).unwrap()
            );
            assert_eq!(
                implicit,
                pairing_base(&format!("https://{host}:24182/")).unwrap()
            );
        }
        assert_eq!(
            pairing_base("https://10.0.1.101:443")
                .unwrap()
                .port_or_known_default(),
            Some(443)
        );
    }
}

pub async fn renew_if_due(connection: EnrolledConnection) -> Result<()> {
    renew(connection, false).await
}

/// Explicit renewal shares the automatic driver's durable issuance/activation path.
pub async fn renew_now(connection: EnrolledConnection) -> Result<()> {
    renew(connection, true).await
}

#[derive(Serialize, Deserialize)]
struct PreviousIdentity {
    profile: LocalProfile,
    retire_at: i64,
}

fn retire_previous(directory: &Path, current: &LocalProfile) -> Result<()> {
    let path = directory.join("previous-profile.json");
    if !path.exists() {
        return Ok(());
    }
    let previous: PreviousIdentity = serde_json::from_slice(&security::read_private(&path)?)?;
    if previous.retire_at > security::now_ms() && previous.profile.expires_at > security::now_ms() {
        return Ok(());
    }
    let owner = directory.canonicalize()?;
    for old in [
        &previous.profile.private_key_pem_path,
        &previous.profile.certificate_pem_path,
    ] {
        if old != &current.private_key_pem_path
            && old != &current.certificate_pem_path
            && old.exists()
        {
            ensure!(
                old.canonicalize()?.parent() == Some(owner.as_path()),
                "previous credential is outside its owned profile"
            );
            fs::remove_file(old)?;
        }
    }
    fs::remove_file(path)?;
    Ok(())
}

async fn renew(connection: EnrolledConnection, force: bool) -> Result<()> {
    // The server is the sole writer of its reserved local administrator identity.
    if connection
        .profile_path
        .file_name()
        .is_some_and(|name| name == "local-client.json")
    {
        return Ok(());
    }
    let directory = connection
        .profile_path
        .parent()
        .context("credential directory absent")?;
    let lock = fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(directory.join("renew.lock"))?;
    if lock.try_lock().is_err() {
        return Ok(());
    }
    let active = LocalProfile::load(&connection.profile_path)?;
    retire_previous(directory, &active)?;
    let pending_path = directory.join("renew-pending.json");
    if !force
        && !pending_path.exists()
        && active.expires_at.saturating_sub(security::now_ms()) > 10 * 86_400_000
    {
        return Ok(());
    }
    let mut pending = if pending_path.exists() {
        load_pending(&pending_path)?
    } else {
        new_pending(directory, None)?
    };
    pending.root_path = active.root_pem_path.clone();
    save_pending(&pending_path, &pending)?;
    if pending.response.is_none() {
        let base = origin(&connection.url, true)?;
        let root = String::from_utf8(security::read_private(&active.root_pem_path)?)?;
        let receipt = pinned_client(&root)?
            .get(base.join(&format!("v1/enrollment/requests/{}", pending.request_id))?)
            .send()
            .await?;
        if receipt.status().is_success() {
            pending.response = Some(bounded_json(receipt).await?);
        } else {
            ensure!(
                receipt.status() == reqwest::StatusCode::NOT_FOUND,
                "renewal receipt could not be confirmed"
            );
            let request = RenewalRequest {
                request_id: pending.request_id.clone(),
                csr_pem: pending.csr_pem.clone(),
            };
            pending.response = Some(
                exchange(
                    &connection,
                    Method::POST,
                    "/v1/enrollment/renew",
                    Some(serde_json::to_value(request)?),
                )
                .await?,
            );
        }
        save_pending(&pending_path, &pending)?;
    }
    let base = origin(&connection.url, true)?;
    // Persist the recovery reference before activation can change server state.
    let previous = directory.join("previous-profile.json");
    security::write_private(
        &previous,
        &serde_json::to_vec(&PreviousIdentity {
            retire_at: (security::now_ms() + 7 * 86_400_000).min(active.expires_at),
            profile: active,
        })?,
    )?;
    let previous: PreviousIdentity = serde_json::from_slice(&security::read_private(&previous)?)?;
    activate_pending(
        &base,
        &previous.profile.cluster_id,
        &previous.profile.instance_id,
        &pending,
        &connection.profile_path,
    )
    .await?;
    let _ = fs::remove_file(pending_path);
    Ok(())
}
