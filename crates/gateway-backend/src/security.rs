//! Installation credentials and enrollment state. Never included in application snapshots.
use anyhow::{ensure, Context, Result};

/// Public, non-secret reasons a peering request can be rejected.
#[derive(Debug)]
pub enum EnrollmentRejection {
    Unavailable,
    Expired,
    Invalid,
    Conflict,
    AlreadyEnrolled,
}
impl EnrollmentRejection {
    pub fn code(&self) -> &'static str {
        match self {
            Self::Unavailable => "peering_key_unavailable",
            Self::Expired => "peering_key_expired",
            Self::Invalid => "peering_key_invalid",
            Self::Conflict => "enrollment_conflict",
            Self::AlreadyEnrolled => "installation_already_enrolled",
        }
    }
    pub fn from_code(code: &str) -> Option<Self> {
        Some(match code {
            "peering_key_unavailable" => Self::Unavailable,
            "peering_key_expired" => Self::Expired,
            "peering_key_invalid" => Self::Invalid,
            "enrollment_conflict" => Self::Conflict,
            "installation_already_enrolled" => Self::AlreadyEnrolled,
            _ => return None,
        })
    }
}
impl std::fmt::Display for EnrollmentRejection {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Unavailable => "No active peering key. Generate a new key on the server; restarting the server clears existing keys",
            Self::Expired => "Peering key expired. Generate a new key on the server and copy it again",
            Self::Invalid => "Peering key is invalid or was replaced. Copy the current key from the intended server",
            Self::Conflict => "Enrollment request is already bound to different credentials; retry the original request",
            Self::AlreadyEnrolled => "This installation is already enrolled. Use its existing connection or recover it through the server administrator",
        })
    }
}
impl std::error::Error for EnrollmentRejection {}
use rand_core::{OsRng, RngCore};
use rcgen::{
    BasicConstraints, CertificateParams, CertificateSigningRequestParams, DistinguishedName,
    DnType, ExtendedKeyUsagePurpose, IsCa, Issuer, KeyPair, KeyUsagePurpose,
};
use rusqlite::{params, Connection, OptionalExtension};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    collections::{HashMap, HashSet},
    io::Write,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};
use subtle::ConstantTimeEq;

const LEAF_MS: i64 = 30 * 86_400_000;
const OVERLAP_MS: i64 = 7 * 86_400_000;
pub fn now_ms() -> i64 {
    crate::db::now_ms()
}
fn random() -> String {
    let mut bytes = [0; 32];
    OsRng.fill_bytes(&mut bytes);
    hex::encode(bytes)
}
fn digest(bytes: &[u8]) -> String {
    hex::encode(Sha256::digest(bytes))
}

/// Atomic owner-only credential storage. No credential text is passed to a process.
pub fn write_private(path: &Path, bytes: &[u8]) -> Result<()> {
    let parent = path.parent().context("credential has no parent")?;
    private_directory(parent)?;
    let mut file = tempfile::NamedTempFile::new_in(parent)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        file.as_file()
            .set_permissions(std::fs::Permissions::from_mode(0o600))?;
    }
    #[cfg(windows)]
    let encrypted;
    #[cfg(windows)]
    let bytes = if path
        .file_name()
        .and_then(|n| n.to_str())
        .is_some_and(|n| n.contains("key"))
        || bytes.starts_with(b"-----BEGIN PRIVATE KEY-----")
        || bytes.starts_with(b"-----BEGIN EC PRIVATE KEY-----")
        || bytes.starts_with(b"-----BEGIN RSA PRIVATE KEY-----")
    {
        encrypted = dpapi(bytes, true)?;
        encrypted.as_slice()
    } else {
        bytes
    };
    file.write_all(bytes)?;
    file.as_file().sync_all()?;
    file.persist(path).map_err(|error| error.error)?;
    Ok(())
}
pub fn private_directory(path: &Path) -> Result<()> {
    std::fs::create_dir_all(path)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))?;
    }

    Ok(())
}
/// Read a private key, transparently unwrapping current-user DPAPI on Windows.
pub fn read_private(path: &Path) -> Result<Vec<u8>> {
    let bytes = std::fs::read(path)?;
    #[cfg(windows)]
    if bytes.starts_with(b"AMUX-DPAPI-1\0") {
        return dpapi(&bytes[13..], false);
    }
    Ok(bytes)
}
#[cfg(windows)]
fn dpapi(bytes: &[u8], encrypt: bool) -> Result<Vec<u8>> {
    use windows_sys::Win32::{
        Foundation::LocalFree,
        Security::Cryptography::{
            CryptProtectData, CryptUnprotectData, CRYPTPROTECT_UI_FORBIDDEN, CRYPT_INTEGER_BLOB,
        },
    };
    let input = CRYPT_INTEGER_BLOB {
        cbData: bytes.len().try_into()?,
        pbData: bytes.as_ptr() as *mut u8,
    };
    let mut output = CRYPT_INTEGER_BLOB {
        cbData: 0,
        pbData: std::ptr::null_mut(),
    };
    let result = unsafe {
        if encrypt {
            CryptProtectData(
                &input,
                std::ptr::null(),
                std::ptr::null(),
                std::ptr::null(),
                std::ptr::null(),
                CRYPTPROTECT_UI_FORBIDDEN,
                &mut output,
            )
        } else {
            CryptUnprotectData(
                &input,
                std::ptr::null_mut(),
                std::ptr::null(),
                std::ptr::null(),
                std::ptr::null(),
                CRYPTPROTECT_UI_FORBIDDEN,
                &mut output,
            )
        }
    };
    ensure!(result != 0, "current-user credential protection failed");
    let mut result = if encrypt {
        b"AMUX-DPAPI-1\0".to_vec()
    } else {
        Vec::new()
    };
    unsafe {
        result.extend_from_slice(std::slice::from_raw_parts(
            output.pbData,
            output.cbData as usize,
        ));
        LocalFree(output.pbData.cast());
    }
    Ok(result)
}

#[derive(Clone, Serialize, Deserialize)]
pub struct LocalProfile {
    pub version: u32,
    pub cluster_id: String,
    pub node_id: String,
    pub instance_id: String,
    pub url: String,
    pub root_pem_path: PathBuf,
    pub certificate_pem_path: PathBuf,
    pub private_key_pem_path: PathBuf,
    pub serial: String,
    pub expires_at: i64,
}
impl LocalProfile {
    pub fn load(path: &Path) -> Result<Self> {
        Ok(serde_json::from_slice(&read_private(path)?)?)
    }
    pub fn save(&self, path: &Path) -> Result<()> {
        write_private(path, &serde_json::to_vec_pretty(self)?)
    }
}
pub fn local_profile_path(profile: &Path) -> PathBuf {
    profile.join("local-client.json")
}
pub struct ClientIdentity {
    pub private_key_pem: String,
    pub csr_pem: String,
}
impl ClientIdentity {
    pub fn generate() -> Result<Self> {
        let key = KeyPair::generate()?;
        let params = CertificateParams::default();
        let csr = params.serialize_request(&key)?;
        Ok(Self {
            private_key_pem: key.serialize_pem(),
            csr_pem: csr.pem()?,
        })
    }
}
#[derive(Clone, Serialize, Deserialize)]
pub struct Discovery {
    pub version: u32,
    pub cluster_id: String,
    pub root_sha256: String,
    pub root_pem: String,
    pub secure_port: u16,
}
#[derive(Serialize, Deserialize)]
pub struct EnrollmentRequest {
    pub request_id: String,
    pub invitation: String,
    pub csr_pem: String,
    pub instance_id: String,
}
#[derive(Serialize, Deserialize)]
pub struct RenewalRequest {
    pub request_id: String,
    pub csr_pem: String,
}
#[derive(Clone, Serialize, Deserialize)]
pub struct EnrollmentResponse {
    pub cluster_id: String,
    pub node_id: String,
    pub certificate_pem: String,
    pub root_pem: String,
    pub expires_at: i64,
    pub serial: String,
}
#[derive(Serialize, Deserialize)]
pub struct Invitation {
    pub invitation: String,
    pub expires_at: i64,
}
#[derive(Clone, Serialize, Deserialize)]
pub struct NodeStatus {
    pub node_id: String,
    pub instance_id: String,
    pub role: String,
    pub current_serial: String,
    pub previous_serial: Option<String>,
    pub previous_until: Option<i64>,
    pub pending_serial: Option<String>,
    pub revoked: bool,
    pub last_seen: Option<i64>,
}
struct ActiveInvitation {
    id: String,
    raw: String,
    deadline: Instant,
}
struct Inner {
    db: Connection,
    active: Option<ActiveInvitation>,
}
pub struct SecurityStore {
    profile: PathBuf,
    pub cluster_id: String,
    pub root_pem: String,
    pub root_sha256: String,
    inner: Mutex<Inner>,
    connections: Mutex<HashMap<String, usize>>,
    observed_servers: Mutex<HashSet<String>>,
    _owner: std::fs::File,
    stopping: std::sync::atomic::AtomicBool,
    discovery_port: std::sync::atomic::AtomicU16,
}
impl SecurityStore {
    pub fn open(profile: &Path, names: &[String]) -> Result<Arc<Self>> {
        private_directory(profile)?;
        let profile = profile.canonicalize()?;
        let owner = std::fs::OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(profile.join(".security-owner.lock"))?;
        owner.try_lock().context("security profile already owned")?;
        let db_path = profile.join("security.sqlite");
        if !db_path.exists() {
            write_private(&db_path, &[])?;
        }
        let db = Connection::open(db_path)?;
        db.execute_batch("PRAGMA foreign_keys=ON; PRAGMA journal_mode=DELETE;
         CREATE TABLE IF NOT EXISTS installation(id INTEGER PRIMARY KEY CHECK(id=1),cluster_id TEXT NOT NULL);
         CREATE TABLE IF NOT EXISTS invitations(id TEXT PRIMARY KEY, verifier TEXT NOT NULL,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,revoked INTEGER NOT NULL DEFAULT 0);
         CREATE TABLE IF NOT EXISTS nodes(id TEXT PRIMARY KEY,instance_id TEXT NOT NULL UNIQUE,role TEXT NOT NULL,current_serial TEXT NOT NULL,previous_serial TEXT,previous_until INTEGER,pending_serial TEXT,revoked INTEGER NOT NULL DEFAULT 0,last_seen INTEGER);
         CREATE TABLE IF NOT EXISTS certificates(serial TEXT PRIMARY KEY,node_id TEXT NOT NULL REFERENCES nodes(id),certificate_pem TEXT NOT NULL,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,activated INTEGER NOT NULL DEFAULT 0);
         CREATE TABLE IF NOT EXISTS issuance(request_id TEXT PRIMARY KEY,binding TEXT NOT NULL,response TEXT NOT NULL);
         CREATE TABLE IF NOT EXISTS consumers(id TEXT PRIMARY KEY,node_id TEXT NOT NULL REFERENCES nodes(id));
         CREATE TABLE IF NOT EXISTS native_instances(id TEXT PRIMARY KEY,node_id TEXT NOT NULL REFERENCES nodes(id));")?;
        db.execute("UPDATE invitations SET revoked=1", [])?;
        let cluster_id = match db
            .query_row("SELECT cluster_id FROM installation WHERE id=1", [], |r| {
                r.get::<_, String>(0)
            })
            .optional()?
        {
            Some(id) => id,
            None => {
                let id = uuid::Uuid::now_v7().to_string();
                db.execute("INSERT INTO installation VALUES(1,?1)", [&id])?;
                id
            }
        };
        let ca_path = profile.join("root.pem");
        let key_path = profile.join("root-key.pem");
        if !ca_path.exists() {
            ensure!(
                !key_path.exists(),
                "incomplete installation root; refusing replacement"
            );
            let key = KeyPair::generate()?;
            let mut params = CertificateParams::default();
            params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
            params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
            params
                .distinguished_name
                .push(DnType::CommonName, format!("amux {cluster_id}"));
            let cert = params.self_signed(&key)?;
            write_private(&key_path, key.serialize_pem().as_bytes())?;
            write_private(&ca_path, cert.pem().as_bytes())?;
        }
        let root_pem = std::fs::read_to_string(ca_path)?;
        let der = rustls_pemfile::certs(&mut root_pem.as_bytes())
            .next()
            .context("root certificate absent")??;
        let root_sha256 = digest(der.as_ref());
        let store = Arc::new(Self {
            profile: profile.to_owned(),
            cluster_id,
            root_pem,
            root_sha256,
            inner: Mutex::new(Inner { db, active: None }),
            connections: Mutex::new(HashMap::new()),
            observed_servers: Mutex::new(HashSet::new()),
            _owner: owner,
            stopping: std::sync::atomic::AtomicBool::new(false),
            discovery_port: std::sync::atomic::AtomicU16::new(24181),
        });
        store.ensure_server_identity(names)?;
        Ok(store)
    }
    pub fn set_discovery_port(&self, port: u16) {
        self.discovery_port
            .store(port, std::sync::atomic::Ordering::Release);
    }
    pub fn discovery(&self, secure_port: u16) -> Discovery {
        Discovery {
            version: 1,
            cluster_id: self.cluster_id.clone(),
            root_sha256: self.root_sha256.clone(),
            root_pem: self.root_pem.clone(),
            secure_port,
        }
    }
    fn issuer(&self) -> Result<Issuer<'static, KeyPair>> {
        let key = KeyPair::from_pem(&String::from_utf8(read_private(
            &self.profile.join("root-key.pem"),
        )?)?)?;
        Ok(Issuer::from_ca_cert_pem(&self.root_pem, key)?)
    }
    fn sign(&self, csr_pem: &str, node: &str) -> Result<(String, String, i64)> {
        ensure!(csr_pem.len() <= 16_384, "CSR exceeds limit");
        let mut csr = CertificateSigningRequestParams::from_pem(csr_pem)?;
        let now = now_ms();
        let expires = now / 1000 * 1000 + LEAF_MS;
        csr.params = CertificateParams::default();
        csr.params.distinguished_name = DistinguishedName::new();
        csr.params.distinguished_name.push(DnType::CommonName, node);
        csr.params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
        csr.params.key_usages = vec![KeyUsagePurpose::DigitalSignature];
        csr.params.use_authority_key_identifier_extension = true;
        csr.params.not_before = rcgen_time(now - 300_000)?;
        csr.params.not_after = rcgen_time(expires)?;
        let serial = random();
        csr.params.serial_number = Some(rcgen::SerialNumber::from_slice(&hex::decode(
            &serial[..32],
        )?));
        let cert = csr.signed_by(&self.issuer()?)?;
        let pem = cert.pem();
        let serial = certificate_serial(cert.der().as_ref())?;
        Ok((pem, serial, expires))
    }
    pub fn generate_invitation(&self) -> Result<Invitation> {
        let mut inner = self.inner.lock().unwrap();
        let id = uuid::Uuid::now_v7().to_string();
        let secret = random();
        let expires = now_ms() + 600_000;
        let deadline = Instant::now() + Duration::from_secs(600);
        let raw = format!(
            "amux1.{}.{}.{}.{}.{}",
            self.cluster_id,
            self.root_sha256,
            id,
            secret,
            self.discovery_port
                .load(std::sync::atomic::Ordering::Acquire)
        );
        let tx = inner.db.transaction()?;
        tx.execute("UPDATE invitations SET revoked=1", [])?;
        tx.execute(
            "INSERT INTO invitations(id,verifier,created_at,expires_at) VALUES(?1,?2,?3,?4)",
            params![id, digest(raw.as_bytes()), now_ms(), expires],
        )?;
        tx.commit()?;
        inner.active = Some(ActiveInvitation {
            id,
            raw: raw.clone(),
            deadline,
        });
        Ok(Invitation {
            invitation: raw,
            expires_at: expires,
        })
    }
    pub fn copy_invitation(&self) -> Result<Invitation> {
        let mut inner = self.inner.lock().unwrap();
        let a = inner
            .active
            .as_ref()
            .ok_or(EnrollmentRejection::Unavailable)?;
        let expires: i64 = inner.db.query_row(
            "SELECT expires_at FROM invitations WHERE id=?1 AND revoked=0",
            [&a.id],
            |r| r.get(0),
        )?;
        if Instant::now() >= a.deadline || now_ms() >= expires {
            inner.active = None;
            inner.db.execute("UPDATE invitations SET revoked=1", [])?;
            anyhow::bail!(EnrollmentRejection::Expired);
        }
        Ok(Invitation {
            invitation: inner.active.as_ref().unwrap().raw.clone(),
            expires_at: expires,
        })
    }
    pub fn begin_shutdown(&self) -> Result<()> {
        self.stopping
            .store(true, std::sync::atomic::Ordering::Release);
        self.invalidate_invitation()
    }
    pub fn invalidate_invitation(&self) -> Result<()> {
        let mut inner = self.inner.lock().unwrap();
        inner.db.execute("UPDATE invitations SET revoked=1", [])?;
        inner.active = None;
        Ok(())
    }
    pub fn issuance_response(&self, request_id: &str) -> Result<Option<EnrollmentResponse>> {
        uuid::Uuid::parse_str(request_id)?;
        let inner = self.inner.lock().unwrap();
        let response = inner
            .db
            .query_row(
                "SELECT response FROM issuance WHERE request_id=?1",
                [request_id],
                |r| r.get::<_, String>(0),
            )
            .optional()?;
        response
            .map(|response| serde_json::from_str(&response).map_err(Into::into))
            .transpose()
    }
    pub fn enroll(&self, request: &EnrollmentRequest) -> Result<EnrollmentResponse> {
        uuid::Uuid::parse_str(&request.request_id)?;
        uuid::Uuid::parse_str(&request.instance_id)?;
        let binding = digest(
            format!(
                "{}:{}:{}",
                request.invitation, request.csr_pem, request.instance_id
            )
            .as_bytes(),
        );
        let mut inner = self.inner.lock().unwrap();
        if let Some((stored, response)) = inner
            .db
            .query_row(
                "SELECT binding,response FROM issuance WHERE request_id=?1",
                [&request.request_id],
                |r| Ok((r.get::<_, String>(0)?, r.get::<_, String>(1)?)),
            )
            .optional()?
        {
            ensure!(stored == binding, EnrollmentRejection::Conflict);
            return Ok(serde_json::from_str(&response)?);
        }
        let active = inner
            .active
            .as_ref()
            .ok_or(EnrollmentRejection::Unavailable)?;
        ensure!(
            Instant::now() < active.deadline,
            EnrollmentRejection::Expired
        );
        let (verifier, expires, revoked): (String, i64, bool) = inner.db.query_row(
            "SELECT verifier,expires_at,revoked FROM invitations WHERE id=?1",
            [&active.id],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
        )?;
        ensure!(now_ms() < expires, EnrollmentRejection::Expired);
        ensure!(
            !revoked
                && bool::from(
                    verifier
                        .as_bytes()
                        .ct_eq(digest(request.invitation.as_bytes()).as_bytes())
                ),
            EnrollmentRejection::Invalid
        );
        let existing=inner.db.query_row("SELECT n.id,n.role,n.revoked,MAX(c.expires_at) FROM nodes n JOIN certificates c ON c.node_id=n.id WHERE n.instance_id=?1 GROUP BY n.id",[&request.instance_id],|r|Ok((r.get::<_,String>(0)?,r.get::<_,String>(1)?,r.get::<_,bool>(2)?,r.get::<_,i64>(3)?))).optional()?;
        if let Some((_, role, revoked, expires)) = &existing {
            ensure!(
                role == "client" && !*revoked && *expires <= now_ms(),
                EnrollmentRejection::AlreadyEnrolled
            );
        }
        let node = existing
            .as_ref()
            .map(|(id, _, _, _)| id.clone())
            .unwrap_or_else(|| uuid::Uuid::now_v7().to_string());
        let (pem, serial, until) = self.sign(&request.csr_pem, &node)?;
        // Signing and replacement share the same lock; recheck at issuance commit.
        ensure!(
            Instant::now() < inner.active.as_ref().unwrap().deadline && now_ms() < expires,
            EnrollmentRejection::Expired
        );
        let response = EnrollmentResponse {
            cluster_id: self.cluster_id.clone(),
            node_id: node.clone(),
            certificate_pem: pem.clone(),
            root_pem: self.root_pem.clone(),
            expires_at: until,
            serial: serial.clone(),
        };
        let tx = inner.db.transaction()?;
        if existing.is_some() {
            tx.execute("UPDATE nodes SET current_serial=?2,previous_serial=NULL,previous_until=NULL,pending_serial=NULL WHERE id=?1",params![node,serial])?;
        } else {
            tx.execute(
                "INSERT INTO nodes(id,instance_id,role,current_serial) VALUES(?1,?2,'client',?3)",
                params![node, request.instance_id, serial],
            )?;
        }
        tx.execute("INSERT INTO certificates(serial,node_id,certificate_pem,created_at,expires_at) VALUES(?1,?2,?3,?4,?5)",params![serial,node,pem,now_ms(),until])?;
        tx.execute(
            "INSERT INTO issuance VALUES(?1,?2,?3)",
            params![
                request.request_id,
                binding,
                serde_json::to_string(&response)?
            ],
        )?;
        tx.commit()?;
        Ok(response)
    }
    pub fn provision_local(&self, url: &str, instance_id: &str) -> Result<LocalProfile> {
        let path = local_profile_path(&self.profile);
        if path.exists() {
            let mut p = LocalProfile::load(&path)?;
            ensure!(
                p.instance_id == instance_id,
                "local instance binding changed"
            );
            let mut changed = p.url != url;
            p.url = url.to_owned();
            if p.expires_at - now_ms() <= LEAF_MS / 3 {
                changed = true;
                // The owning local process has the installation issuer and may
                // recover its reserved administration identity after offline expiry.
                let identity = ClientIdentity::generate()?;
                let (pem, serial, expires) = self.sign(&identity.csr_pem, &p.node_id)?;
                let generation = uuid::Uuid::now_v7().to_string();
                let key_path = self.profile.join(format!("local-{generation}-key.pem"));
                let cert_path = self.profile.join(format!("local-{generation}.pem"));
                write_private(&key_path, identity.private_key_pem.as_bytes())?;
                write_private(&cert_path, pem.as_bytes())?;
                let mut inner = self.inner.lock().unwrap();
                let tx = inner.db.transaction()?;
                let role: String = tx.query_row(
                    "SELECT role FROM nodes WHERE id=?1 AND revoked=0",
                    [&p.node_id],
                    |r| r.get(0),
                )?;
                ensure!(
                    role == "administrator",
                    "local administration identity unavailable"
                );
                tx.execute(
                    "INSERT INTO certificates VALUES(?1,?2,?3,?4,?5,1)",
                    params![serial, p.node_id, pem, now_ms(), expires],
                )?;
                tx.execute("UPDATE nodes SET previous_serial=current_serial,previous_until=?2,current_serial=?3,pending_serial=NULL WHERE id=?1",params![p.node_id,now_ms()+OVERLAP_MS,serial])?;
                tx.commit()?;
                let previous = PreviousServerIdentity {
                    serial: p.serial.clone(),
                    certificate: p.certificate_pem_path.clone(),
                    private_key: p.private_key_pem_path.clone(),
                    until: (now_ms() + OVERLAP_MS).min(p.expires_at),
                };
                write_private(
                    &self.profile.join("local-previous.json"),
                    &serde_json::to_vec_pretty(&previous)?,
                )?;
                p.serial = serial;
                p.expires_at = expires;
                p.private_key_pem_path = key_path;
                p.certificate_pem_path = cert_path;
            }
            if changed {
                p.save(&path)?;
            }
            return Ok(p);
        }
        let identity = ClientIdentity::generate()?;
        let node = uuid::Uuid::now_v7().to_string();
        let (pem, serial, expires) = self.sign(&identity.csr_pem, &node)?;
        let mut inner = self.inner.lock().unwrap();
        let tx = inner.db.transaction()?;
        tx.execute("INSERT INTO nodes(id,instance_id,role,current_serial) VALUES(?1,?2,'administrator',?3)",params![node,instance_id,serial])?;
        tx.execute(
            "INSERT INTO certificates VALUES(?1,?2,?3,?4,?5,1)",
            params![serial, node, pem, now_ms(), expires],
        )?;
        tx.commit()?;
        let p = LocalProfile {
            version: 1,
            cluster_id: self.cluster_id.clone(),
            node_id: node.clone(),
            instance_id: instance_id.to_owned(),
            url: url.to_owned(),
            root_pem_path: self.profile.join("local-root.pem"),
            certificate_pem_path: self.profile.join(format!("local-{node}.pem")),
            private_key_pem_path: self.profile.join(format!("local-{node}-key.pem")),
            serial: serial.clone(),
            expires_at: expires,
        };
        write_private(&p.root_pem_path, self.root_pem.as_bytes())?;
        write_private(&p.private_key_pem_path, identity.private_key_pem.as_bytes())?;
        write_private(&p.certificate_pem_path, pem.as_bytes())?;
        p.save(&path)?;
        Ok(p)
    }
    pub fn authorize(&self, serial: &str) -> Result<NodeStatus> {
        ensure!(
            !self.stopping.load(std::sync::atomic::Ordering::Acquire),
            "server stopping"
        );
        let inner = self.inner.lock().unwrap();
        let n = authorized_node(&inner.db, serial)?;
        if n.last_seen.unwrap_or(0) < now_ms() - 60_000 {
            inner.db.execute(
                "UPDATE nodes SET last_seen=?2 WHERE id=?1",
                params![n.node_id, now_ms()],
            )?;
        }
        Ok(n)
    }
    pub fn activate(&self, serial: &str) -> Result<NodeStatus> {
        self.authorize(serial)?;
        let mut inner = self.inner.lock().unwrap();
        let tx = inner.db.transaction()?;
        let n = authorized_node(&tx, serial)?;
        if n.pending_serial.as_deref() == Some(serial) {
            ensure!(
                n.previous_serial.is_none(),
                "previous credential has not retired"
            );
            let old_expiry: i64 = tx.query_row(
                "SELECT expires_at FROM certificates WHERE serial=?1",
                [&n.current_serial],
                |r| r.get(0),
            )?;
            tx.execute("UPDATE nodes SET previous_serial=current_serial,previous_until=?2,current_serial=?3,pending_serial=NULL WHERE id=?1",params![n.node_id,(now_ms()+OVERLAP_MS).min(old_expiry),serial])?;
        }
        tx.execute(
            "UPDATE certificates SET activated=1 WHERE serial=?1",
            [serial],
        )?;
        tx.commit()?;
        drop(inner);
        self.authorize(serial)
    }
    pub fn renew(&self, serial: &str, request: &RenewalRequest) -> Result<EnrollmentResponse> {
        let node = self.authorize(serial)?;
        uuid::Uuid::parse_str(&request.request_id)?;
        let binding = digest(format!("renew:{}:{}", node.node_id, request.csr_pem).as_bytes());
        let mut inner = self.inner.lock().unwrap();
        let node = authorized_node(&inner.db, serial)?;
        if let Some((old, response)) = inner
            .db
            .query_row(
                "SELECT binding,response FROM issuance WHERE request_id=?1",
                [&request.request_id],
                |r| Ok((r.get::<_, String>(0)?, r.get::<_, String>(1)?)),
            )
            .optional()?
        {
            ensure!(old == binding, "request ID already bound");
            return Ok(serde_json::from_str(&response)?);
        }
        ensure!(
            node.pending_serial.is_none(),
            "renewal pending; activate or recover it first"
        );
        ensure!(
            node.previous_serial.is_none(),
            "previous credential has not retired"
        );
        let (pem, new_serial, expires) = self.sign(&request.csr_pem, &node.node_id)?;
        let response = EnrollmentResponse {
            cluster_id: self.cluster_id.clone(),
            node_id: node.node_id.clone(),
            certificate_pem: pem.clone(),
            root_pem: self.root_pem.clone(),
            expires_at: expires,
            serial: new_serial.clone(),
        };
        let tx = inner.db.transaction()?;
        tx.execute(
            "INSERT INTO certificates VALUES(?1,?2,?3,?4,?5,0)",
            params![new_serial, node.node_id, pem, now_ms(), expires],
        )?;
        tx.execute(
            "UPDATE nodes SET pending_serial=?2 WHERE id=?1",
            params![node.node_id, new_serial],
        )?;
        tx.execute(
            "INSERT INTO issuance VALUES(?1,?2,?3)",
            params![
                request.request_id,
                binding,
                serde_json::to_string(&response)?
            ],
        )?;
        tx.commit()?;
        Ok(response)
    }
    pub fn bind_instance(&self, node: &str, instance: &str) -> Result<()> {
        let id = uuid::Uuid::parse_str(instance)?;
        ensure!(
            !id.is_nil() && id.to_string() == instance,
            "invalid native instance"
        );
        let inner = self.inner.lock().unwrap();
        inner.db.execute(
            "INSERT OR IGNORE INTO native_instances VALUES(?1,?2)",
            params![instance, node],
        )?;
        let owner: String = inner.db.query_row(
            "SELECT node_id FROM native_instances WHERE id=?1",
            [instance],
            |r| r.get(0),
        )?;
        ensure!(owner == node, "native instance belongs to another node");
        Ok(())
    }
    pub fn bind_consumer(&self, node: &str, consumer: &str) -> Result<()> {
        ensure!(
            !consumer.is_empty() && consumer.len() <= 128,
            "invalid consumer"
        );
        let inner = self.inner.lock().unwrap();
        inner.db.execute(
            "INSERT OR IGNORE INTO consumers VALUES(?1,?2)",
            params![consumer, node],
        )?;
        let owner: String = inner.db.query_row(
            "SELECT node_id FROM consumers WHERE id=?1",
            [consumer],
            |r| r.get(0),
        )?;
        ensure!(owner == node, "consumer belongs to another node");
        Ok(())
    }
    pub fn nodes(&self) -> Result<Vec<NodeStatus>> {
        let inner = self.inner.lock().unwrap();
        let mut stmt=inner.db.prepare("SELECT id,instance_id,role,current_serial,previous_serial,previous_until,pending_serial,revoked,last_seen FROM nodes ORDER BY id")?;
        let rows = stmt
            .query_map([], |r| {
                Ok(NodeStatus {
                    node_id: r.get(0)?,
                    instance_id: r.get(1)?,
                    role: r.get(2)?,
                    current_serial: r.get(3)?,
                    previous_serial: r.get(4)?,
                    previous_until: r.get(5)?,
                    pending_serial: r.get(6)?,
                    revoked: r.get(7)?,
                    last_seen: r.get(8)?,
                })
            })?
            .collect::<rusqlite::Result<Vec<_>>>()?;
        Ok(rows)
    }
    pub fn revoke(&self, node: &str) -> Result<()> {
        let inner = self.inner.lock().unwrap();
        ensure!(
            inner
                .db
                .execute("UPDATE nodes SET revoked=1 WHERE id=?1", [node])?
                == 1,
            "node not found"
        );
        Ok(())
    }
    pub fn observe_server(&self, serial: &str) {
        self.observed_servers
            .lock()
            .unwrap()
            .insert(serial.to_owned());
    }
    pub fn connection_open(&self, serial: &str) {
        *self
            .connections
            .lock()
            .unwrap()
            .entry(serial.to_owned())
            .or_default() += 1;
    }
    pub fn connection_closed(&self, serial: &str) {
        if let Some(n) = self.connections.lock().unwrap().get_mut(serial) {
            *n = n.saturating_sub(1);
        }
    }
    pub fn retire_drained(&self) -> Result<()> {
        let mut inner = self.inner.lock().unwrap();
        if inner
            .active
            .as_ref()
            .is_some_and(|a| Instant::now() >= a.deadline)
        {
            inner.active = None;
            inner.db.execute("UPDATE invitations SET revoked=1", [])?;
        }
        let connections = self.connections.lock().unwrap();
        let rows=inner.db.prepare("SELECT id,previous_serial,previous_until FROM nodes WHERE previous_serial IS NOT NULL")?.query_map([],|r|Ok((r.get::<_,String>(0)?,r.get::<_,String>(1)?,r.get::<_,i64>(2)?)))?.collect::<rusqlite::Result<Vec<_>>>()?;
        for (node, serial, until) in rows {
            if until <= now_ms() || connections.get(&serial).copied().unwrap_or(0) == 0 {
                inner.db.execute(
                    "UPDATE nodes SET previous_serial=NULL,previous_until=NULL WHERE id=?1",
                    [node],
                )?;
            }
        }
        Ok(())
    }
    pub fn ensure_server_identity(&self, names: &[String]) -> Result<()> {
        let manifest = self.profile.join("server-identity.json");
        let mut old = if manifest.exists() {
            Some(serde_json::from_slice::<ServerIdentity>(&read_private(
                &manifest,
            )?)?)
        } else {
            None
        };
        if old.as_ref().is_some_and(|old| {
            old.expires_at - now_ms() > LEAF_MS / 3
                && names.iter().collect::<HashSet<_>>() == old.names.iter().collect::<HashSet<_>>()
        }) {
            return Ok(());
        }
        if let Some(identity) = old.as_mut() {
            if let Some(previous) = identity.previous.as_ref() {
                let connections = self.connections.lock().unwrap();
                ensure!(
                    connections
                        .get(&format!("server:{}", previous.serial))
                        .copied()
                        .unwrap_or(0)
                        == 0
                        && connections
                            .get(&format!("server:{}", identity.serial))
                            .copied()
                            .unwrap_or(0)
                            == 0,
                    "previous server generation has not drained"
                );
                drop(connections);
                let previous = identity.previous.take().unwrap();
                write_private(&manifest, &serde_json::to_vec_pretty(identity)?)?;
                for path in [previous.private_key, previous.certificate] {
                    if path.exists() {
                        std::fs::remove_file(path)?;
                    }
                }
            }
        }
        ensure!(
            !names.is_empty(),
            "server certificate requires DNS/IP names"
        );
        let key = KeyPair::generate()?;
        let mut params = CertificateParams::new(names.to_vec())?;
        params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
        params.key_usages = vec![KeyUsagePurpose::DigitalSignature];
        params.use_authority_key_identifier_extension = true;
        params.not_before = rcgen_time(now_ms() - 300_000)?;
        let expires = now_ms() / 1000 * 1000 + LEAF_MS;
        params.not_after = rcgen_time(expires)?;
        let cert = params.signed_by(&key, &self.issuer()?)?;
        let serial = certificate_serial(cert.der().as_ref())?;
        let generation = uuid::Uuid::now_v7().to_string();
        let certificate = self.profile.join(format!("server-{generation}.pem"));
        let private_key = self.profile.join(format!("server-{generation}-key.pem"));
        write_private(&private_key, key.serialize_pem().as_bytes())?;
        write_private(&certificate, cert.pem().as_bytes())?;
        let previous = old.map(|old| PreviousServerIdentity {
            serial: old.serial,
            certificate: old.certificate,
            private_key: old.private_key,
            until: (now_ms() + OVERLAP_MS).min(old.expires_at),
        });
        let identity = ServerIdentity {
            serial,
            certificate,
            private_key,
            expires_at: expires,
            names: names.to_vec(),
            previous,
        };
        write_private(&manifest, &serde_json::to_vec_pretty(&identity)?)?;
        Ok(())
    }
    pub fn server_identity(&self) -> Result<ServerIdentity> {
        Ok(serde_json::from_slice(&read_private(
            &self.profile.join("server-identity.json"),
        )?)?)
    }
    pub fn server_serial_valid(&self, serial: &str) -> bool {
        self.server_identity().is_ok_and(|identity| {
            (identity.serial == serial && now_ms() < identity.expires_at)
                || identity
                    .previous
                    .is_some_and(|previous| previous.serial == serial && now_ms() < previous.until)
        })
    }
    pub fn server_paths(&self) -> Result<(PathBuf, PathBuf)> {
        let identity = self.server_identity()?;
        Ok((identity.certificate, identity.private_key))
    }
    pub fn retire_local_previous(&self) -> Result<()> {
        let manifest = self.profile.join("local-previous.json");
        if manifest.exists() {
            let previous: PreviousServerIdentity =
                serde_json::from_slice(&read_private(&manifest)?)?;
            if now_ms() >= previous.until {
                for path in [&previous.private_key, &previous.certificate] {
                    if path.exists() {
                        std::fs::remove_file(path)?;
                    }
                }
                std::fs::remove_file(manifest)?;
            }
        }
        Ok(())
    }
    pub fn retire_server_previous(&self) -> Result<()> {
        let mut identity = self.server_identity()?;
        if let Some(previous) = identity.previous.as_ref() {
            if now_ms() >= previous.until
                || (self
                    .observed_servers
                    .lock()
                    .unwrap()
                    .contains(&identity.serial)
                    && self
                        .connections
                        .lock()
                        .unwrap()
                        .get(&format!("server:{}", previous.serial))
                        .copied()
                        .unwrap_or(0)
                        == 0)
            {
                let previous = identity.previous.take().unwrap();
                write_private(
                    &self.profile.join("server-identity.json"),
                    &serde_json::to_vec_pretty(&identity)?,
                )?;
                std::fs::remove_file(previous.private_key)?;
                std::fs::remove_file(previous.certificate)?;
            }
        }
        Ok(())
    }
}
#[derive(Clone, Serialize, Deserialize)]
pub struct ServerIdentity {
    pub serial: String,
    pub certificate: PathBuf,
    pub private_key: PathBuf,
    pub expires_at: i64,
    pub names: Vec<String>,
    pub previous: Option<PreviousServerIdentity>,
}
#[derive(Clone, Serialize, Deserialize)]
pub struct PreviousServerIdentity {
    pub serial: String,
    pub certificate: PathBuf,
    pub private_key: PathBuf,
    pub until: i64,
}
fn authorized_node(conn: &Connection, serial: &str) -> Result<NodeStatus> {
    let n:NodeStatus=conn.query_row("SELECT n.id,n.instance_id,n.role,n.current_serial,n.previous_serial,n.previous_until,n.pending_serial,n.revoked,n.last_seen FROM nodes n JOIN certificates c ON c.node_id=n.id WHERE c.serial=?1 AND c.expires_at>?2",params![serial,now_ms()],|r|Ok(NodeStatus{node_id:r.get(0)?,instance_id:r.get(1)?,role:r.get(2)?,current_serial:r.get(3)?,previous_serial:r.get(4)?,previous_until:r.get(5)?,pending_serial:r.get(6)?,revoked:r.get(7)?,last_seen:r.get(8)?}))?;
    ensure!(!n.revoked, "node revoked");
    ensure!(
        n.current_serial == serial
            || n.pending_serial.as_deref() == Some(serial)
            || (n.previous_serial.as_deref() == Some(serial)
                && n.previous_until.unwrap_or(0) > now_ms()),
        "credential retired"
    );
    Ok(n)
}
fn rcgen_time(ms: i64) -> Result<time::OffsetDateTime> {
    Ok(time::OffsetDateTime::from_unix_timestamp(ms / 1000)?)
}
pub fn certificate_serial(der: &[u8]) -> Result<String> {
    let (_, cert) = x509_parser::parse_x509_certificate(der)
        .map_err(|_| anyhow::anyhow!("invalid X509 certificate"))?;
    Ok(cert.raw_serial_as_string())
}

#[derive(Clone, Serialize, Deserialize)]
pub struct CertificateInfo {
    pub serial: String,
    pub node_id: String,
    pub expires_at: i64,
}
pub fn certificate_info(pem: &str) -> Result<CertificateInfo> {
    let der = rustls_pemfile::certs(&mut pem.as_bytes())
        .next()
        .context("certificate absent")??;
    let (_, cert) = x509_parser::parse_x509_certificate(der.as_ref())
        .map_err(|_| anyhow::anyhow!("invalid certificate"))?;
    let node_id = cert
        .subject()
        .iter_common_name()
        .next()
        .context("certificate node absent")?
        .as_str()?
        .to_owned();
    Ok(CertificateInfo {
        serial: cert.raw_serial_as_string(),
        node_id,
        expires_at: cert.validity().not_after.timestamp() * 1000,
    })
}

impl std::fmt::Debug for Invitation {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Invitation")
            .field("invitation", &"[redacted]")
            .field("expires_at", &self.expires_at)
            .finish()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn profile() -> tempfile::TempDir {
        let base = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../target/test-fixtures");
        std::fs::create_dir_all(&base).unwrap();
        tempfile::tempdir_in(base).unwrap()
    }
    fn request(invitation: String, identity: &ClientIdentity) -> EnrollmentRequest {
        EnrollmentRequest {
            request_id: uuid::Uuid::now_v7().to_string(),
            invitation,
            csr_pem: identity.csr_pem.clone(),
            instance_id: uuid::Uuid::now_v7().to_string(),
        }
    }
    #[test]
    fn invitation_replacement_restart_and_idempotent_proof() {
        let dir = profile();
        let store = SecurityStore::open(dir.path(), &["localhost".into()]).unwrap();
        let old = store.generate_invitation().unwrap();
        let current = store.generate_invitation().unwrap();
        assert!(current.expires_at - now_ms() <= 600_000);
        let identity = ClientIdentity::generate().unwrap();
        assert!(store.enroll(&request(old.invitation, &identity)).is_err());
        let req = request(current.invitation.clone(), &identity);
        let response = store.enroll(&req).unwrap();
        assert_eq!(store.enroll(&req).unwrap().serial, response.serial);
        let mut reused = request(current.invitation.clone(), &identity);
        reused.request_id = req.request_id.clone();
        assert!(store.enroll(&reused).is_err());
        assert!(store.authorize(&response.serial).is_ok());
        let db = store.inner.lock().unwrap();
        let stored: String = db
            .db
            .query_row(
                "SELECT verifier FROM invitations WHERE revoked=0",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert!(!stored.contains(&current.invitation));
        drop(db);
        drop(store);
        let store = SecurityStore::open(dir.path(), &["localhost".into()]).unwrap();
        assert!(store
            .enroll(&request(current.invitation, &identity))
            .is_err());
        assert!(store.authorize(&response.serial).is_ok());
    }
    #[test]
    fn renewal_activation_revocation_and_consumer_binding() {
        let dir = profile();
        let store = SecurityStore::open(dir.path(), &["localhost".into()]).unwrap();
        let identity = ClientIdentity::generate().unwrap();
        let first = store
            .enroll(&request(
                store.generate_invitation().unwrap().invitation,
                &identity,
            ))
            .unwrap();
        let new = ClientIdentity::generate().unwrap();
        let req = RenewalRequest {
            request_id: uuid::Uuid::now_v7().to_string(),
            csr_pem: new.csr_pem,
        };
        let second = store.renew(&first.serial, &req).unwrap();
        assert_ne!(first.serial, second.serial);
        assert_eq!(
            store.renew(&first.serial, &req).unwrap().serial,
            second.serial
        );
        store.connection_open(&first.serial);
        let node = store.activate(&second.serial).unwrap();
        assert_eq!(node.previous_serial.as_deref(), Some(first.serial.as_str()));
        store.retire_drained().unwrap();
        assert!(store.authorize(&first.serial).is_ok());
        store.connection_closed(&first.serial);
        store.retire_drained().unwrap();
        assert!(store.authorize(&first.serial).is_err());
        store
            .bind_consumer(&node.node_id, "fixture-consumer")
            .unwrap();
        assert!(store
            .bind_consumer("another-node", "fixture-consumer")
            .is_err());
        store.revoke(&node.node_id).unwrap();
        assert!(store.authorize(&second.serial).is_err());
    }
    #[test]
    fn proof_expiry_and_restart_owner_lock() {
        let dir = profile();
        let store = SecurityStore::open(dir.path(), &["localhost".into()]).unwrap();
        assert!(SecurityStore::open(dir.path(), &["localhost".into()]).is_err());
        let invitation = store.generate_invitation().unwrap();
        let identity = ClientIdentity::generate().unwrap();
        let mut req = request(invitation.invitation, &identity);
        req.csr_pem = "invalid CSR".into();
        assert!(store.enroll(&req).is_err());
        req.csr_pem = identity.csr_pem;
        store
            .inner
            .lock()
            .unwrap()
            .active
            .as_mut()
            .unwrap()
            .deadline = Instant::now() - Duration::from_secs(1);
        assert!(store.enroll(&req).is_err());
    }
    #[test]
    fn server_rotation_retains_active_generation_then_deletes_drained_keys() {
        let dir = profile();
        let store = SecurityStore::open(dir.path(), &["localhost".into()]).unwrap();
        let root = store.root_sha256.clone();
        let mut first = store.server_identity().unwrap();
        store.connection_open(&format!("server:{}", first.serial));
        first.expires_at = now_ms() + 86_400_000;
        write_private(
            &dir.path().join("server-identity.json"),
            &serde_json::to_vec(&first).unwrap(),
        )
        .unwrap();
        store.ensure_server_identity(&["localhost".into()]).unwrap();
        let second = store.server_identity().unwrap();
        assert_ne!(first.serial, second.serial);
        assert_eq!(root, store.root_sha256);
        assert!(second.previous.as_ref().unwrap().until <= first.expires_at);
        store.observe_server(&second.serial);
        store.retire_server_previous().unwrap();
        assert!(first.private_key.exists());
        store.connection_closed(&format!("server:{}", first.serial));
        store.retire_server_previous().unwrap();
        assert!(!first.private_key.exists());
        assert!(!first.certificate.exists());
        assert!(store.server_identity().unwrap().previous.is_none());
        // SAN additions AND removals reissue, including another configuration
        // change after restart-like zero active connections during overlap.
        store
            .ensure_server_identity(&["localhost".into(), "fixture.example".into()])
            .unwrap();
        let changed = store.server_identity().unwrap();
        assert!(changed.names.contains(&"fixture.example".into()));
        assert_ne!(changed.serial, second.serial);
        store.ensure_server_identity(&["localhost".into()]).unwrap();
        let removed = store.server_identity().unwrap();
        assert!(!removed.names.contains(&"fixture.example".into()));
        assert_ne!(removed.serial, changed.serial);
    }
    #[test]
    fn local_recovery_and_private_storage_are_atomic_and_owner_protected() {
        let dir = profile();
        let store = SecurityStore::open(dir.path(), &["localhost".into()]).unwrap();
        let instance = uuid::Uuid::now_v7().to_string();
        let mut first = store
            .provision_local("https://localhost:24182", &instance)
            .unwrap();
        let original = first.serial.clone();
        first.expires_at = now_ms() - 1;
        first.save(&local_profile_path(dir.path())).unwrap();
        let second = store
            .provision_local("https://localhost:24182", &instance)
            .unwrap();
        assert_eq!(first.node_id, second.node_id);
        assert_ne!(original, second.serial);
        assert_ne!(first.private_key_pem_path, second.private_key_pem_path);
        assert_eq!(
            certificate_info(&std::fs::read_to_string(&second.certificate_pem_path).unwrap())
                .unwrap()
                .expires_at,
            second.expires_at
        );
        assert_eq!(
            LocalProfile::load(&local_profile_path(dir.path()))
                .unwrap()
                .serial,
            second.serial
        );
        store.retire_local_previous().unwrap();
        assert!(!first.private_key_pem_path.exists());
        assert!(second.private_key_pem_path.exists());
        let key = dir.path().join("private-key-test");
        write_private(&key, b"fixture-only-secret").unwrap();
        assert_eq!(read_private(&key).unwrap(), b"fixture-only-secret");
        #[cfg(windows)]
        assert!(std::fs::read(&key).unwrap().starts_with(b"AMUX-DPAPI-1\0"));
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            assert_eq!(
                std::fs::metadata(&key).unwrap().permissions().mode() & 0o777,
                0o600
            );
            assert_eq!(
                std::fs::metadata(dir.path()).unwrap().permissions().mode() & 0o777,
                0o700
            );
        }
        let invitation = store.generate_invitation().unwrap();
        assert!(!format!("{invitation:?}").contains(&invitation.invitation));
    }
    #[test]
    fn concurrent_activation_preserves_previous_and_new_renewal_waits_for_drain() {
        let dir = profile();
        let store = SecurityStore::open(dir.path(), &["localhost".into()]).unwrap();
        let first = store
            .enroll(&request(
                store.generate_invitation().unwrap().invitation,
                &ClientIdentity::generate().unwrap(),
            ))
            .unwrap();
        store.connection_open(&first.serial);
        let request = RenewalRequest {
            request_id: uuid::Uuid::now_v7().to_string(),
            csr_pem: ClientIdentity::generate().unwrap().csr_pem,
        };
        let second = store.renew(&first.serial, &request).unwrap();
        let barrier = Arc::new(std::sync::Barrier::new(9));
        std::thread::scope(|scope| {
            for _ in 0..8 {
                let store = store.clone();
                let barrier = barrier.clone();
                let serial = &second.serial;
                let previous = &first.serial;
                scope.spawn(move || {
                    barrier.wait();
                    let node = store.activate(serial).unwrap();
                    assert_eq!(node.current_serial, *serial);
                    assert_eq!(node.previous_serial.as_deref(), Some(previous.as_str()));
                });
            }
            barrier.wait();
        });
        let node = store.authorize(&second.serial).unwrap();
        assert_eq!(node.previous_serial.as_deref(), Some(first.serial.as_str()));
        assert_eq!(
            store.renew(&second.serial, &request).unwrap().serial,
            second.serial
        );
        let third = RenewalRequest {
            request_id: uuid::Uuid::now_v7().to_string(),
            csr_pem: ClientIdentity::generate().unwrap().csr_pem,
        };
        assert!(store.renew(&second.serial, &third).is_err());
        let unchanged = store.authorize(&second.serial).unwrap();
        assert_eq!(unchanged.previous_serial, node.previous_serial);
        assert_eq!(unchanged.previous_until, node.previous_until);
        assert!(unchanged.pending_serial.is_none());
        store.connection_closed(&first.serial);
        store.retire_drained().unwrap();
        assert!(store.renew(&second.serial, &third).is_ok());
    }
    #[test]
    fn server_and_client_leaf_identify_issuer_and_allow_signatures() {
        use x509_parser::extensions::ParsedExtension;
        let dir = profile();
        let store = SecurityStore::open(dir.path(), &["localhost".into()]).unwrap();
        let root_der = rustls_pemfile::certs(&mut store.root_pem.as_bytes())
            .next()
            .unwrap()
            .unwrap();
        let (_, root) = x509_parser::parse_x509_certificate(root_der.as_ref()).unwrap();
        let root_key_id = root
            .extensions()
            .iter()
            .find_map(|extension| match extension.parsed_extension() {
                ParsedExtension::SubjectKeyIdentifier(id) => Some(id.0.to_vec()),
                _ => None,
            })
            .expect("installation CA must identify its signing key");
        let client = store
            .enroll(&request(
                store.generate_invitation().unwrap().invitation,
                &ClientIdentity::generate().unwrap(),
            ))
            .unwrap();
        let server = std::fs::read_to_string(store.server_paths().unwrap().0).unwrap();
        for (pem, server_leaf) in [(&server, true), (&client.certificate_pem, false)] {
            let der = rustls_pemfile::certs(&mut pem.as_bytes())
                .next()
                .unwrap()
                .unwrap();
            let (_, certificate) = x509_parser::parse_x509_certificate(der.as_ref()).unwrap();
            let authority = certificate
                .extensions()
                .iter()
                .find_map(|extension| match extension.parsed_extension() {
                    ParsedExtension::AuthorityKeyIdentifier(id) => {
                        id.key_identifier.as_ref().map(|id| id.0.to_vec())
                    }
                    _ => None,
                })
                .expect("leaf must identify its issuer for strict X509 verification");
            assert_eq!(authority, root_key_id);
            assert!(certificate
                .key_usage()
                .unwrap()
                .unwrap()
                .value
                .digital_signature());
            let purpose = certificate.extended_key_usage().unwrap().unwrap();
            assert_eq!(purpose.value.server_auth, server_leaf);
            assert_eq!(purpose.value.client_auth, !server_leaf);
        }
    }
}
