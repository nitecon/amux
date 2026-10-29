//! Operator migration of a complete gateway database, never a project/client replica.
use amux_gateway_backend::{db, security};
use anyhow::{Context, Result, bail, ensure};
use rusqlite::{
    Connection, OpenFlags,
    backup::{Backup, StepResult},
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeMap,
    fs,
    io::{Read, Write},
    path::{Path, PathBuf},
    sync::Arc,
    time::{Duration, Instant},
};

const MAX_DOWNLOAD: u64 = 2 * 1024 * 1024 * 1024;
const REQUIRED: &[&str] = &[
    "projects",
    "tasks",
    "messages",
    "patterns",
    "skills",
    "gateway_memories",
];

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Inventory {
    pub source: String,
    pub source_sha256: String,
    pub bytes: u64,
    pub sha256: String,
    pub tables: BTreeMap<String, u64>,
    pub schema_notes: Vec<String>,
}

#[derive(Debug)]
struct Staged {
    _directory: tempfile::TempDir,
    database: PathBuf,
    original: PathBuf,
}

#[derive(Debug, Clone)]
pub struct Preview {
    staged: Arc<Staged>,
    pub inventory: Inventory,
}

impl Preview {
    pub fn summary(&self) -> String {
        let count = |table: &str| self.inventory.tables.get(table).copied().unwrap_or(0);
        format!(
            "{} tables · {} projects · {} tasks · {} comments · {} memories · {} patterns · {} skills · {} bytes. SQLite integrity and foreign keys verified.",
            self.inventory.tables.len(),
            count("projects"),
            count("tasks"),
            count("task_comments"),
            count("gateway_memories"),
            count("patterns"),
            count("skills"),
            self.inventory.bytes
        )
    }
}

#[derive(Debug, Clone)]
pub struct Installed {
    pub database: PathBuf,
    pub receipt: PathBuf,
}

fn read_only(path: &Path) -> Result<Connection> {
    Connection::open_with_flags(
        path,
        OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NO_MUTEX,
    )
    .context("Cannot read gateway SQLite database; choose a readable database or exported snapshot")
}

fn inventory(conn: &Connection) -> Result<BTreeMap<String, u64>> {
    let names = conn.prepare("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")?
        .query_map([], |row| row.get::<_, String>(0))?.collect::<rusqlite::Result<Vec<_>>>()?;
    names
        .into_iter()
        .map(|name| {
            let quoted = name.replace('"', "\"\"");
            let count =
                conn.query_row(&format!("SELECT COUNT(*) FROM \"{quoted}\""), [], |row| {
                    row.get::<_, i64>(0)
                })?;
            Ok((name, u64::try_from(count)?))
        })
        .collect()
}

fn unused_database(conn: &Connection) -> Result<bool> {
    // These singletons contain only transaction context or controller time.
    // Events, queued input, identities, settings and unknown tables remain
    // protected even when the application has no projects or tasks yet.
    Ok(inventory(conn)?.iter().all(|(name, count)| {
        *count == 0
            || (*count == 1
                && matches!(
                    name.as_str(),
                    "task_mutation_context" | "presence_clock" | "retention_runs"
                ))
    }))
}

fn retired_projection(name: &str) -> Option<&'static str> {
    match name {
        // Presence::initialize migrates accepted reports into the canonical
        // directory; stale observations cannot override newer advertisements.
        "presence_agents" => Some("accepted identity reports merged into task_target_presence"),
        // Notifier and Reclaim now read the canonical Presence directory.
        "notifier_agents" | "reclaim_busy" => Some("obsolete copy of Presence state removed"),
        _ => None,
    }
}

fn verify(conn: &Connection) -> Result<BTreeMap<String, u64>> {
    let integrity: String = conn.query_row("PRAGMA integrity_check", [], |row| row.get(0))?;
    ensure!(
        integrity == "ok",
        "SQLite integrity check failed; source was not imported"
    );
    ensure!(
        !conn.prepare("PRAGMA foreign_key_check")?.exists([])?,
        "SQLite contains broken foreign keys; source was not imported"
    );
    let tables = inventory(conn)?;
    ensure!(
        REQUIRED.iter().all(|name| tables.contains_key(*name)),
        "This is not a complete gateway application database"
    );
    Ok(tables)
}

fn backup(source: &Path, destination: &Path) -> Result<()> {
    let source = read_only(source)?;
    let mut destination = Connection::open(destination)?;
    destination.busy_timeout(Duration::from_secs(5))?;
    let backup = Backup::new(&source, &mut destination)?;
    let deadline = Instant::now() + Duration::from_secs(120);
    loop {
        ensure!(
            Instant::now() < deadline,
            "Snapshot timed out; source remains unchanged. Retry when gateway writes are quieter"
        );
        match backup.step(256)? {
            StepResult::Done => break,
            StepResult::More => {}
            StepResult::Busy | StepResult::Locked => std::thread::sleep(Duration::from_millis(20)),
            _ => bail!("Unexpected SQLite backup result"),
        }
    }
    Ok(())
}

fn digest(path: &Path) -> Result<String> {
    let mut file = fs::File::open(path)?;
    let mut hash = Sha256::new();
    let mut block = [0u8; 65536];
    loop {
        let count = file.read(&mut block)?;
        if count == 0 {
            break;
        }
        hash.update(&block[..count]);
    }
    Ok(format!("{:x}", hash.finalize()))
}

pub fn prepare_local(source: &Path, state: &Path) -> Result<Preview> {
    ensure!(
        source.is_file(),
        "Choose the gateway SQLite file or an exported SQLite snapshot"
    );
    let source = source.canonicalize()?;
    let directory = state.join("server");
    security::private_directory(&directory)?;
    let target = directory.join("application.sqlite");
    ensure!(
        !target.exists() || source != target.canonicalize()?,
        "Source is already this server's database"
    );
    let staged = tempfile::Builder::new()
        .prefix("migration-")
        .tempdir_in(&directory)?;
    let database = staged.path().join("application.sqlite");
    backup(&source, &database)?;
    validate_stage(staged, database, source.display().to_string())
}

fn validate_stage(
    directory: tempfile::TempDir,
    database: PathBuf,
    source: String,
) -> Result<Preview> {
    let before = verify(&read_only(&database)?)?;
    let source_sha256 = digest(&database)?;
    let original = directory.path().join("original.sqlite");
    fs::copy(&database, &original)?;
    // Exercise real backend schema upgrades on the private copy only. Keep every
    // source records except explicitly retired derived projections; the original
    // snapshot is retained separately. Reject other losses before touching target.
    let upgraded = db::open(database.to_str().context("Database path is not UTF8")?)?;
    let tables = verify(&upgraded.lock().unwrap())?;
    let lost = before
        .iter()
        .filter(|(name, count)| match tables.get(*name) {
            Some(after) => after < *count,
            None => retired_projection(name).is_none(),
        })
        .map(|(name, count)| format!("{name}: {count} → {:?}", tables.get(name)))
        .collect::<Vec<_>>();
    ensure!(
        lost.is_empty(),
        "Schema upgrade removed source rows; migration stopped for inspection: {}",
        lost.join(", ")
    );
    let schema_notes = before
        .iter()
        .filter_map(|(name, count)| match tables.get(name) {
            Some(after) if *after > *count => Some(format!(
                    "{name}: {count} source records preserved; {} compatibility records added",
                    after - count
                )),
            None => retired_projection(name).map(|explanation| {
                format!("{name}: {count} legacy records; {explanation}. Complete original snapshot retained, including stale or conflicting reports")
            }),
            _ => None,
        })
        .collect();
    upgraded
        .lock()
        .unwrap()
        .execute_batch("PRAGMA wal_checkpoint(TRUNCATE); PRAGMA journal_mode=DELETE;")?;
    drop(upgraded);
    let bytes = fs::metadata(&database)?.len();
    let sha256 = digest(&database)?;
    Ok(Preview {
        staged: Arc::new(Staged {
            _directory: directory,
            database,
            original,
        }),
        inventory: Inventory {
            source,
            source_sha256,
            bytes,
            sha256,
            tables,
            schema_notes,
        },
    })
}

/// Uses only the configured operator key, never a URL-provided credential or redirect.
pub async fn prepare_remote(url: &str, key: &str, state: &Path) -> Result<Preview> {
    let mut base = crate::gateway_transport::origin(url, false)?;
    ensure!(
        !key.trim().is_empty(),
        "Enter the legacy gateway API key first"
    );
    ensure!(
        base.scheme() == "https"
            || matches!(base.host_str(), Some("localhost" | "127.0.0.1" | "::1")),
        "Remote database migration requires HTTPS"
    );
    base.set_path("/v1/admin/migration/snapshot");
    let client = reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .connect_timeout(Duration::from_secs(10))
        .timeout(Duration::from_secs(180))
        .build()?;
    let mut response = client
        .get(base)
        .bearer_auth(key)
        .send()
        .await
        .map_err(|_| {
            anyhow::anyhow!(
                "Cannot reach gateway migration export; check network and gateway address"
            )
        })?;
    if response.status() == reqwest::StatusCode::NOT_FOUND {
        bail!(
            "This gateway does not provide migration exports yet. Choose its local SQLite file, or export a consistent SQLite backup on the gateway host and select that file here"
        );
    }
    ensure!(
        response.status().is_success(),
        "Gateway refused the migration export (HTTP {}); verify operator access",
        response.status().as_u16()
    );
    let expected = response
        .headers()
        .get("X-Amux-Snapshot-Sha256")
        .and_then(|value| value.to_str().ok())
        .filter(|value| value.len() == 64 && value.bytes().all(|byte| byte.is_ascii_hexdigit()))
        .context("Gateway snapshot is missing its SHA-256 checksum")?
        .to_ascii_lowercase();
    let length = response
        .content_length()
        .filter(|size| *size > 0 && *size <= MAX_DOWNLOAD)
        .context("Gateway snapshot size is missing or exceeds 2 GiB")?;
    let folder = state.join("server");
    security::private_directory(&folder)?;
    let directory = tempfile::Builder::new()
        .prefix("migration-")
        .tempdir_in(folder)?;
    let database = directory.path().join("application.sqlite");
    let mut file = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&database)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        file.set_permissions(fs::Permissions::from_mode(0o600))?;
    }
    let mut received = 0;
    while let Some(chunk) = response.chunk().await.map_err(|_| {
        anyhow::anyhow!("Gateway snapshot download interrupted; target remains unchanged")
    })? {
        received += chunk.len() as u64;
        ensure!(
            received <= length && received <= MAX_DOWNLOAD,
            "Gateway snapshot exceeds its declared size"
        );
        file.write_all(&chunk)?;
    }
    file.sync_all()?;
    drop(file);
    ensure!(
        received == length && digest(&database)? == expected,
        "Gateway snapshot size/checksum mismatch; target remains unchanged"
    );
    let source = url.to_owned();
    tokio::task::spawn_blocking(move || validate_stage(directory, database, source)).await?
}

pub fn install(preview: &Preview, state: &Path) -> Result<Installed> {
    let directory = state.join("server");
    security::private_directory(&directory)?;
    let target = directory.join("application.sqlite");
    let config = db::DatabaseConfig::sqlite(target.to_str().context("Database path is not UTF8")?);
    let _lock =
        crate::server::profile_lock(&config).context("Stop the local server before importing")?;
    ensure!(
        digest(&preview.staged.database)? == preview.inventory.sha256,
        "Prepared snapshot changed; prepare the migration again"
    );
    ensure!(
        digest(&preview.staged.original)? == preview.inventory.source_sha256,
        "Original snapshot changed; prepare the migration again"
    );
    let receipt = directory.join("migration.json");
    if target.exists() {
        let mut wal = target.as_os_str().to_owned();
        wal.push("-wal");
        let has_wal = fs::metadata(PathBuf::from(wal)).is_ok_and(|metadata| metadata.len() > 0);
        let current_hash = digest(&target)?;
        // A retry of an installed snapshot is a no-op, even if the previous UI
        // missed its completion. Never replace a populated server or merge IDs.
        if !has_wal && current_hash == preview.inventory.sha256 {
            security::write_private(&receipt, &serde_json::to_vec_pretty(&preview.inventory)?)?;
            return Ok(Installed {
                database: target,
                receipt,
            });
        }
        let previous: Option<Inventory> = fs::read(&receipt)
            .ok()
            .and_then(|bytes| serde_json::from_slice(&bytes).ok());
        let untouched_import = !has_wal
            && previous.as_ref().is_some_and(|previous| {
                previous.sha256 == current_hash && previous.source == preview.inventory.source
            });
        let conn = read_only(&target)?;
        ensure!(
            untouched_import || unused_database(&conn)?,
            "Local server already contains data or has changed since import. Migration requires an unused server; existing data was not overwritten"
        );
        drop(conn);
        backup(
            &target,
            &directory.join(format!("before-migration-{}.sqlite", uuid::Uuid::new_v4())),
        )?;
        let conn = Connection::open(&target)?;
        conn.execute_batch("PRAGMA wal_checkpoint(TRUNCATE); PRAGMA journal_mode=DELETE;")?;
    }
    let mut file = tempfile::NamedTempFile::new_in(&directory)?;
    std::io::copy(&mut fs::File::open(&preview.staged.database)?, &mut file)?;
    file.as_file().sync_all()?;
    let original_backup = directory.join(format!(
        "legacy-source-{}.sqlite",
        preview.inventory.source_sha256
    ));
    if !original_backup.exists() {
        let mut original = tempfile::NamedTempFile::new_in(&directory)?;
        std::io::copy(
            &mut fs::File::open(&preview.staged.original)?,
            &mut original,
        )?;
        original.as_file().sync_all()?;
        original
            .persist_noclobber(&original_backup)
            .map_err(|error| error.error)?;
    }
    file.persist(&target).map_err(|error| error.error)?;
    security::write_private(&receipt, &serde_json::to_vec_pretty(&preview.inventory)?)
        .context("Database installed, but saving the migration receipt failed; retry this same snapshot before switching")?;
    #[cfg(unix)]
    fs::File::open(&directory)?.sync_all()?;
    Ok(Installed {
        database: target,
        receipt,
    })
}
