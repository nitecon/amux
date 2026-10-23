//! Install-aware update checks and the verified direct-download updater.
//!
//! Only a direct-download bundle is ever written: the executable's directory
//! must hold `BUILD.json` whose CLI and GUI SHA-256 match the files on disk.
//! Homebrew (`Cellar`/`Caskroom`) and every other install get guidance only.
//!
//! Preview bundles (`candidate_only`) locate their own published release by
//! revision and only offer releases published later. Stable installs compare
//! `v<major>.<minor>.<patch>` tags and ignore prereleases. Revisions and
//! hashes are never ordered.
//!
//! Updates never stop running processes. A running GUI keeps its old image
//! until the user restarts it: Unix replaces by rename, Windows renames the
//! locked image aside (`*.amux-old`, removed by [`cleanup_backups`] once it is
//! no longer running). The updating CLI replaces itself with `self_replace`.

use anyhow::{Context, Result, anyhow, bail, ensure};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::{
    collections::HashSet,
    fs::{self, File},
    io::{self, Cursor, Read},
    path::{Component, Path, PathBuf},
    time::{Duration, SystemTime},
};

pub const RELEASES_PAGE: &str = "https://github.com/nitecon/amux/releases";
pub const UPDATE_COMMAND: &str = "amux --update";
const BUILD_FILE: &str = "BUILD.json";
const SUMS_FILE: &str = "SHA256SUMS";
const BACKUP_SUFFIX: &str = ".amux-old";
const MAX_RELEASE_LIST: u64 = 8 << 20;
const MAX_SUMS: u64 = 64 << 10;
const MAX_ARCHIVE: u64 = 256 << 20;
const MAX_ENTRY: u64 = 128 << 20;
const MAX_UNPACKED: u64 = 256 << 20;
const MAX_ENTRIES: usize = 64;
const DOCUMENTS: [&str; 4] = [
    "LICENSE",
    "ICED_TERM_LICENSE",
    "REVIEW.md",
    "notification-validation.md",
];

/// Where release metadata and assets come from. Production uses [`ReleaseSource::github`];
/// assets must be served exactly under `download_prefix` + tag.
#[derive(Clone, Debug)]
pub struct ReleaseSource {
    pub api_url: String,
    pub download_prefix: String,
}

impl ReleaseSource {
    pub fn github() -> Self {
        Self {
            api_url: "https://api.github.com/repos/nitecon/amux/releases?per_page=100".into(),
            download_prefix: "https://github.com/nitecon/amux/releases/download/".into(),
        }
    }
}

/// The executable this process started from. Capture once at startup so a
/// later on-disk replacement is visible as [`Restart::required`].
#[derive(Clone, Debug)]
pub struct Running {
    pub exe: PathBuf,
    stamp: Option<Stamp>,
}

#[derive(Clone, Debug, PartialEq)]
struct Stamp {
    len: u64,
    modified: Option<SystemTime>,
}

fn stamp(path: &Path) -> Option<Stamp> {
    let meta = fs::metadata(path).ok()?;
    Some(Stamp {
        len: meta.len(),
        modified: meta.modified().ok(),
    })
}

impl Running {
    pub fn capture() -> io::Result<Self> {
        Ok(Self::at(std::env::current_exe()?))
    }

    pub fn at(exe: PathBuf) -> Self {
        let stamp = stamp(&exe);
        Self { exe, stamp }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Channel {
    Preview,
    Stable,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum InstallMethod {
    /// Verified bundle; `revision` is what is installed on disk now.
    Direct {
        dir: PathBuf,
        revision: String,
    },
    Homebrew {
        prefix: PathBuf,
        cask: bool,
    },
    /// Package manager, source build or modified bundle: never overwritten.
    Unmanaged,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Release {
    pub tag: String,
    pub name: String,
    pub published_at: String,
    pub html_url: String,
    pub prerelease: bool,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Guidance {
    CliUpdate { command: String },
    Homebrew { command: String },
    Manual { url: String },
}

/// `path` is the program to start after the user confirms a restart.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Restart {
    pub required: bool,
    pub path: Option<PathBuf>,
}

#[derive(Clone, Debug)]
pub struct UpdateCheck {
    pub method: InstallMethod,
    pub channel: Channel,
    pub installed: Option<Release>,
    pub available: Option<Release>,
    pub guidance: Guidance,
    pub restart: Restart,
    pub note: Option<String>,
}

/// Fetch release metadata and describe the install, any applicable newer
/// release and whether a restart would pick up a different executable.
pub async fn check(running: &Running, source: &ReleaseSource) -> Result<UpdateCheck> {
    let probe = running.clone();
    let (detected, restart) =
        tokio::task::spawn_blocking(move || (detect(&probe.exe), restart_status(&probe))).await?;
    let releases = fetch_releases(source).await?;
    let selection = select(&releases, &detected, source);
    let guidance = match &detected.method {
        InstallMethod::Direct { .. } => Guidance::CliUpdate {
            command: UPDATE_COMMAND.into(),
        },
        InstallMethod::Homebrew { cask, .. } => Guidance::Homebrew {
            command: brew_command(*cask),
        },
        InstallMethod::Unmanaged => Guidance::Manual {
            url: selection
                .available
                .map_or(RELEASES_PAGE.into(), |r| r.html_url.clone()),
        },
    };
    Ok(UpdateCheck {
        method: detected.method,
        channel: detected.channel,
        installed: selection.installed.map(public),
        available: selection.available.map(public),
        guidance,
        restart,
        note: detected.note.or(selection.note),
    })
}

/// Cheap, synchronous: did the executable this process started from change?
pub fn restart_status(running: &Running) -> Restart {
    let changed = stamp(&running.exe) != running.stamp;
    let canonical = fs::canonicalize(&running.exe).unwrap_or_else(|_| running.exe.clone());
    if let Some((prefix, cask, package)) = homebrew(&canonical) {
        if cask {
            return Restart {
                required: changed,
                path: None,
            };
        }
        let file = canonical.file_name().unwrap_or_default();
        let opt = prefix.join("opt").join(package).join("bin").join(file);
        let moved = fs::canonicalize(&opt).is_ok_and(|target| target != canonical);
        return Restart {
            required: changed || moved,
            path: opt.exists().then_some(opt),
        };
    }
    Restart {
        required: changed,
        path: running.exe.exists().then(|| running.exe.clone()),
    }
}

/// Entry point for `amux --update`. `Ok` is the summary to print.
pub fn cli_update() -> Result<String> {
    let running = Running::capture().context("locating the running amux executable")?;
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()?
        .block_on(update(&running, &ReleaseSource::github()))
}

/// Update the verified bundle containing `running.exe` to the selected
/// release. Installation failures restore the previous bundle where possible;
/// incomplete rollback is reported explicitly.
pub async fn update(running: &Running, source: &ReleaseSource) -> Result<String> {
    let detected = detect(&running.exe);
    let dir = match &detected.method {
        InstallMethod::Direct { dir, .. } => dir.clone(),
        InstallMethod::Homebrew { cask, .. } => bail!(
            "amux is managed by Homebrew; run `{}` instead. Nothing was changed.",
            brew_command(*cask)
        ),
        InstallMethod::Unmanaged => bail!(
            "{} is not a verified amux direct-download bundle ({}). Update it with the tool that \
             installed it or download a bundle from {RELEASES_PAGE}. Nothing was changed.",
            running.exe.display(),
            detected.note.as_deref().unwrap_or("no matching BUILD.json")
        ),
    };
    // Keep one transaction per install, including cleanup and provenance checks.
    // Leave the lock inode in place after unlock so another process cannot lock
    // a newly-created file while the old inode is still held.
    let lock = fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .open(dir.join(".amux-update.lock"))
        .context("cannot open update lock; nothing was changed")?;
    lock.try_lock().map_err(|error| {
        anyhow!("another amux update may be running; nothing was changed: {error}")
    })?;
    let detected = detect(&running.exe);
    ensure!(
        matches!(&detected.method, InstallMethod::Direct { dir: current, .. } if current == &dir),
        "installation changed before update lock was acquired; nothing was changed"
    );
    let platform = Platform::current().context("unsupported platform")?;
    let stale = cleanup_backups(&dir);
    let releases = fetch_releases(source).await?;
    let selection = select(&releases, &detected, source);
    let Some(release) = selection.available else {
        return Ok(match selection.note {
            Some(note) => format!("No update applied: {note}"),
            None => "amux is up to date.".into(),
        });
    };
    let (archive, sums) = bundle_assets(release, &platform, source).context("release assets")?;

    let client = client(source)?;
    let sums_body = download(&client, &sums.browser_download_url, MAX_SUMS).await?;
    ensure!(
        archive.size <= MAX_ARCHIVE,
        "{} is larger than {MAX_ARCHIVE} bytes",
        archive.name
    );
    let bytes = download(&client, &archive.browser_download_url, archive.size).await?;
    ensure!(
        bytes.len() as u64 == archive.size,
        "{} size differs from release metadata",
        archive.name
    );
    let digest = sha256(&bytes);
    for (asset, actual) in [(archive, &digest), (sums, &sha256(&sums_body))] {
        if let Some(published) = &asset.digest {
            ensure!(
                published.strip_prefix("sha256:") == Some(actual.as_str()),
                "{} does not match the published digest",
                asset.name
            );
        }
    }
    let listed = listed_hash(&String::from_utf8_lossy(&sums_body), &archive.name)?;
    ensure!(
        listed == digest,
        "{} does not match {SUMS_FILE}",
        archive.name
    );

    let staging = tempfile::Builder::new()
        .prefix(".amux-update-")
        .tempdir_in(&dir)
        .with_context(|| format!("cannot write to {}", dir.display()))?;
    let names = extract(&bytes, &platform, staging.path())?;
    let build = verify_staged(staging.path(), &names, &platform, release)?;
    if let InstallMethod::Direct { revision, .. } = &detected.method {
        ensure!(
            &build.revision != revision,
            "{} contains the installed revision",
            release.tag_name
        );
    }

    let cli_is_self = same_file(&dir.join(&platform.cli), std::env::current_exe().ok());
    let leftovers = install(&dir, staging.path(), &names, &platform, cli_is_self)?;
    let mut summary = format!(
        "Updated amux in {} to {} ({}). Restart {} to use it; running terminals and agents keep \
         the previous version until you restart.",
        dir.display(),
        release.tag_name,
        &build.revision[..12],
        platform.gui
    );
    let leftovers: Vec<_> = stale.into_iter().chain(leftovers).collect();
    if !leftovers.is_empty() {
        summary.push_str(&format!(
            " Files still in use were kept as *{BACKUP_SUFFIX} and are removed by a later \
             update once nothing runs them: {}",
            leftovers
                .iter()
                .map(|p| p.display().to_string())
                .collect::<Vec<_>>()
                .join(", ")
        ));
    }
    Ok(summary)
}

/// Remove `*.amux-old` files left by an update. Returns those still in use.
pub fn cleanup_backups(dir: &Path) -> Vec<PathBuf> {
    let Some(platform) = Platform::current() else {
        return Vec::new();
    };
    let Ok(entries) = fs::read_dir(dir) else {
        return Vec::new();
    };
    entries
        .flatten()
        .map(|e| e.path())
        .filter(|p| {
            p.is_file()
                && p.file_name()
                    .and_then(|name| name.to_str())
                    .is_some_and(|name| {
                        platform.payload().iter().any(|payload| {
                            name.strip_prefix(&format!("{payload}."))
                                .and_then(|rest| rest.strip_suffix(BACKUP_SUFFIX))
                                .is_some_and(|nonce| {
                                    nonce.len() == 8 && nonce.bytes().all(|c| c.is_ascii_hexdigit())
                                })
                        })
                    })
        })
        .filter(|p| fs::remove_file(p).is_err())
        .collect()
}

struct Platform {
    target: &'static str,
    os: &'static str,
    extension: &'static str,
    cli: String,
    gui: String,
}

impl Platform {
    fn current() -> Option<Self> {
        let (target, os, extension) = match (std::env::consts::OS, std::env::consts::ARCH) {
            ("linux", "x86_64") => ("x86_64-unknown-linux-gnu", "linux", ".tar.gz"),
            ("windows", "x86_64") => ("x86_64-pc-windows-msvc", "windows", ".zip"),
            _ => return None,
        };
        let suffix = std::env::consts::EXE_SUFFIX;
        Some(Self {
            target,
            os,
            extension,
            cli: format!("amux{suffix}"),
            gui: format!("amux-desktop{suffix}"),
        })
    }

    fn payload(&self) -> Vec<&str> {
        let mut names = vec![self.gui.as_str(), BUILD_FILE, SUMS_FILE];
        names.extend(DOCUMENTS);
        names.push(self.cli.as_str());
        names
    }
}

#[derive(Deserialize)]
struct Build {
    revision: String,
    target: String,
    binary_sha256: String,
    gui_binary: String,
    gui_binary_sha256: String,
    #[serde(default)]
    candidate_only: bool,
}

fn read_build(path: &Path) -> Result<Build> {
    let mut text = String::new();
    File::open(path)?.take(MAX_SUMS).read_to_string(&mut text)?;
    Ok(serde_json::from_str(text.trim_start_matches('\u{feff}'))?)
}

struct Detected {
    method: InstallMethod,
    channel: Channel,
    note: Option<String>,
}

fn detect(exe: &Path) -> Detected {
    let canonical = fs::canonicalize(exe).unwrap_or_else(|_| exe.to_path_buf());
    if let Some((prefix, cask, _)) = homebrew(&canonical) {
        return Detected {
            method: InstallMethod::Homebrew { prefix, cask },
            channel: Channel::Stable,
            note: None,
        };
    }
    match verified_bundle(&canonical) {
        Ok((dir, build)) => Detected {
            method: InstallMethod::Direct {
                dir,
                revision: build.revision,
            },
            channel: if build.candidate_only {
                Channel::Preview
            } else {
                Channel::Stable
            },
            note: None,
        },
        Err(error) => Detected {
            method: InstallMethod::Unmanaged,
            channel: Channel::Stable,
            note: Some(format!("{error:#}")),
        },
    }
}

fn verified_bundle(exe: &Path) -> Result<(PathBuf, Build)> {
    let platform = Platform::current().context("no direct-download bundle for this platform")?;
    let dir = exe.parent().context("executable has no directory")?;
    let build = read_build(&dir.join(BUILD_FILE)).context("reading BUILD.json")?;
    ensure!(build.target == platform.target, "BUILD.json target differs");
    ensure!(
        build.gui_binary == platform.gui,
        "BUILD.json GUI name differs"
    );
    ensure!(
        is_revision(&build.revision),
        "BUILD.json revision is invalid"
    );
    for (name, expected) in [
        (&platform.cli, &build.binary_sha256),
        (&platform.gui, &build.gui_binary_sha256),
    ] {
        let actual = sha256_file(&dir.join(name)).with_context(|| format!("reading {name}"))?;
        ensure!(&actual == expected, "{name} does not match BUILD.json");
    }
    Ok((dir.to_path_buf(), build))
}

/// `(prefix, cask, package)` for executables inside a Homebrew keg or cask.
fn homebrew(path: &Path) -> Option<(PathBuf, bool, String)> {
    let parts: Vec<_> = path.components().collect();
    let at = parts
        .iter()
        .position(|c| matches!(c.as_os_str().to_str(), Some("Cellar" | "Caskroom")))?;
    let package = parts
        .get(at + 1)?
        .as_os_str()
        .to_string_lossy()
        .into_owned();
    let cask = parts[at].as_os_str() == "Caskroom";
    Some((parts[..at].iter().collect(), cask, package))
}

fn brew_command(cask: bool) -> String {
    if cask {
        "brew update && brew upgrade --cask amux".into()
    } else {
        "brew update && brew upgrade amux".into()
    }
}

#[derive(Deserialize)]
struct ApiRelease {
    tag_name: String,
    #[serde(default)]
    name: Option<String>,
    html_url: String,
    #[serde(default)]
    draft: bool,
    #[serde(default)]
    prerelease: bool,
    published_at: Option<String>,
    #[serde(default)]
    target_commitish: String,
    #[serde(default)]
    assets: Vec<ApiAsset>,
}

#[derive(Deserialize)]
struct ApiAsset {
    name: String,
    size: u64,
    state: String,
    digest: Option<String>,
    browser_download_url: String,
}

fn public(release: &ApiRelease) -> Release {
    Release {
        tag: release.tag_name.clone(),
        name: release.name.clone().unwrap_or_default(),
        published_at: release.published_at.clone().unwrap_or_default(),
        html_url: release.html_url.clone(),
        prerelease: release.prerelease,
    }
}

enum Tag<'a> {
    Review(&'a str),
    Stable([u64; 3]),
}

fn tag(name: &str) -> Option<Tag<'_>> {
    if let Some(short) = name.strip_prefix("review-") {
        let valid = (7..=40).contains(&short.len())
            && short
                .bytes()
                .all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'));
        return valid.then_some(Tag::Review(short));
    }
    version(name.strip_prefix('v')?).map(Tag::Stable)
}

fn version(text: &str) -> Option<[u64; 3]> {
    let mut parts = text.split('.');
    let mut out = [0; 3];
    for slot in &mut out {
        let part = parts.next()?;
        if part.is_empty() || !part.bytes().all(|b| b.is_ascii_digit()) {
            return None;
        }
        *slot = part.parse().ok()?;
    }
    parts.next().is_none().then_some(out)
}

fn is_revision(text: &str) -> bool {
    text.len() == 40 && text.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
}

/// `YYYY-MM-DDTHH:MM:SSZ`, which GitHub always returns; it orders as text.
fn published(release: &ApiRelease) -> Option<&str> {
    let text = release.published_at.as_deref()?;
    let shape = text.len() == 20
        && text.bytes().enumerate().all(|(i, b)| match i {
            4 | 7 => b == b'-',
            10 => b == b'T',
            13 | 16 => b == b':',
            19 => b == b'Z',
            _ => b.is_ascii_digit(),
        });
    shape.then_some(text)
}

fn is_release_of(release: &ApiRelease, revision: &str) -> bool {
    match tag(&release.tag_name) {
        Some(Tag::Review(short)) => revision.starts_with(short),
        _ => release.target_commitish == revision,
    }
}

struct Selection<'a> {
    installed: Option<&'a ApiRelease>,
    available: Option<&'a ApiRelease>,
    note: Option<String>,
}

fn select<'a>(
    releases: &'a [ApiRelease],
    detected: &Detected,
    source: &ReleaseSource,
) -> Selection<'a> {
    let platform = Platform::current();
    let direct = match &detected.method {
        InstallMethod::Direct { revision, .. } => Some(revision.as_str()),
        _ => None,
    };
    // Guidance-only installs do not need our bundle assets on the release.
    let applicable = |r: &ApiRelease| {
        !r.draft
            && published(r).is_some()
            && tag(&r.tag_name).is_some()
            && (direct.is_none()
                || platform
                    .as_ref()
                    .is_some_and(|p| bundle_assets(r, p, source).is_ok()))
    };
    let installed = match direct {
        Some(revision) => releases
            .iter()
            .find(|r| !r.draft && published(r).is_some() && is_release_of(r, revision)),
        None => releases
            .iter()
            .find(|r| !r.draft && r.tag_name == format!("v{}", env!("CARGO_PKG_VERSION"))),
    };
    let mut note = None;
    let available = match detected.channel {
        Channel::Preview => match installed {
            Some(current) => releases
                .iter()
                .filter(|r| applicable(r) && published(r) > published(current))
                .filter(|r| direct.is_none_or(|rev| !is_release_of(r, rev)))
                .max_by_key(|r| published(r)),
            None => {
                note = Some(format!(
                    "installed revision {} is not a published release, so newer releases \
                     cannot be determined",
                    direct.map_or("unknown", |rev| &rev[..12])
                ));
                None
            }
        },
        Channel::Stable => {
            let current = installed
                .and_then(|r| match tag(&r.tag_name) {
                    Some(Tag::Stable(v)) => Some(v),
                    _ => None,
                })
                .or_else(|| version(env!("CARGO_PKG_VERSION")))
                .unwrap_or_default();
            releases
                .iter()
                .filter(|r| applicable(r) && !r.prerelease)
                .filter_map(|r| match tag(&r.tag_name) {
                    Some(Tag::Stable(v)) if v > current => Some((v, r)),
                    _ => None,
                })
                .max_by_key(|(v, _)| *v)
                .map(|(_, r)| r)
        }
    };
    Selection {
        installed,
        available,
        note,
    }
}

fn bundle_assets<'a>(
    release: &'a ApiRelease,
    platform: &Platform,
    source: &ReleaseSource,
) -> Result<(&'a ApiAsset, &'a ApiAsset)> {
    let prefix = format!("amux-{}-x86_64-", platform.os);
    let mut archives = release
        .assets
        .iter()
        .filter(|a| a.name.starts_with(&prefix) && a.name.ends_with(platform.extension));
    let archive = archives.next().with_context(|| {
        format!(
            "{} has no {prefix}*{} asset",
            release.tag_name, platform.extension
        )
    })?;
    ensure!(
        archives.next().is_none(),
        "{} has several platform archives",
        release.tag_name
    );
    let sums = release
        .assets
        .iter()
        .find(|a| a.name == SUMS_FILE)
        .with_context(|| format!("{} has no {SUMS_FILE}", release.tag_name))?;
    for asset in [archive, sums] {
        let expected = format!(
            "{}{}/{}",
            source.download_prefix, release.tag_name, asset.name
        );
        ensure!(
            asset.state == "uploaded" && asset.browser_download_url == expected,
            "{} is not an official uploaded asset of {}",
            asset.name,
            release.tag_name
        );
    }
    Ok((archive, sums))
}

fn client(source: &ReleaseSource) -> Result<reqwest::Client> {
    Ok(reqwest::Client::builder()
        .user_agent(concat!("amux/", env!("CARGO_PKG_VERSION")))
        .https_only(source.api_url.starts_with("https://"))
        .connect_timeout(Duration::from_secs(20))
        .timeout(Duration::from_secs(600))
        .build()?)
}

async fn fetch_releases(source: &ReleaseSource) -> Result<Vec<ApiRelease>> {
    let body = download(&client(source)?, &source.api_url, MAX_RELEASE_LIST).await?;
    serde_json::from_slice(&body).context("parsing release metadata")
}

async fn download(client: &reqwest::Client, url: &str, max: u64) -> Result<Vec<u8>> {
    let mut response = client
        .get(url)
        .header(reqwest::header::ACCEPT, "application/vnd.github+json, */*")
        .send()
        .await
        .with_context(|| format!("requesting {url}"))?
        .error_for_status()?;
    ensure!(
        response.content_length().is_none_or(|n| n <= max),
        "{url} is larger than {max} bytes"
    );
    let mut body = Vec::new();
    while let Some(chunk) = response.chunk().await? {
        ensure!(
            body.len() as u64 + chunk.len() as u64 <= max,
            "{url} is larger than {max} bytes"
        );
        body.extend_from_slice(&chunk);
    }
    Ok(body)
}

fn listed_hash(sums: &str, name: &str) -> Result<String> {
    let mut found = sums.lines().filter_map(|line| {
        let (hash, file) = line.trim().split_once(char::is_whitespace)?;
        (file.trim_start().trim_start_matches('*') == name).then(|| hash.to_ascii_lowercase())
    });
    let hash = found
        .next()
        .with_context(|| format!("{SUMS_FILE} does not list {name}"))?;
    ensure!(found.next().is_none(), "{SUMS_FILE} lists {name} twice");
    Ok(hash)
}

fn sha256(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn sha256_file(path: &Path) -> io::Result<String> {
    let mut hasher = Sha256::new();
    io::copy(&mut File::open(path)?, &mut hasher)?;
    Ok(format!("{:x}", hasher.finalize()))
}

/// Flat archive name: one normal component, optionally prefixed by `./`.
fn entry_name(raw: &str) -> Result<String> {
    let mut parts = Path::new(raw)
        .components()
        .filter(|c| !matches!(c, Component::CurDir));
    match (parts.next(), parts.next()) {
        (Some(Component::Normal(name)), None) if !raw.contains('\\') => name
            .to_str()
            .map(str::to_owned)
            .ok_or_else(|| anyhow!("archive entry {raw:?} is not UTF-8")),
        _ => bail!("archive entry {raw:?} is not a plain file name"),
    }
}

/// Extract known payload files only, refusing links, directories, nested or
/// duplicate names and oversized entries. Unknown plain files are skipped.
fn extract(bytes: &[u8], platform: &Platform, out: &Path) -> Result<HashSet<String>> {
    let wanted = platform.payload();
    let mut seen = HashSet::new();
    let mut keep = |raw: &str, regular: bool, reader: &mut dyn Read| -> Result<()> {
        let name = entry_name(raw)?;
        ensure!(regular, "archive entry {name} is not a regular file");
        ensure!(seen.insert(name.clone()), "archive repeats {name}");
        ensure!(seen.len() <= MAX_ENTRIES, "archive has too many entries");
        if !wanted.contains(&name.as_str()) {
            return Ok(());
        }
        let mut file = create(
            &out.join(&name),
            name == platform.cli || name == platform.gui,
        )?;
        let copied = io::copy(&mut Read::take(reader, MAX_ENTRY + 1), &mut file)?;
        ensure!(copied <= MAX_ENTRY, "archive entry {name} is too large");
        file.sync_all()?;
        Ok(())
    };
    if platform.extension == ".zip" {
        let mut archive = zip::ZipArchive::new(Cursor::new(bytes))?;
        ensure!(archive.len() <= MAX_ENTRIES, "archive has too many entries");
        let mut unpacked = 0u64;
        for index in 0..archive.len() {
            let mut entry = archive.by_index(index)?;
            ensure!(entry.size() <= MAX_ENTRY, "archive entry is too large");
            unpacked = unpacked
                .checked_add(entry.size())
                .context("archive size overflow")?;
            ensure!(
                unpacked <= MAX_UNPACKED,
                "archive expands beyond its size limit"
            );
            let raw = entry.name()?.into_owned();
            let regular = entry.is_file() && !entry.is_symlink();
            keep(&raw, regular, &mut entry)?;
        }
    } else {
        // Bound decompression before tar processes PAX/GNU extension headers,
        // which its iterator reads internally before yielding an entry.
        let expanded = expand_tar(bytes, MAX_UNPACKED)?;
        let mut archive = tar::Archive::new(Cursor::new(expanded));
        for entry in archive.entries()? {
            let mut entry = entry?;
            let raw = String::from_utf8(entry.path_bytes().into_owned())
                .map_err(|_| anyhow!("archive entry name is not UTF-8"))?;
            let regular = entry.header().entry_type() == tar::EntryType::Regular;
            keep(&raw, regular, &mut entry)?;
        }
    }
    Ok(seen)
}

fn expand_tar(bytes: &[u8], limit: u64) -> Result<Vec<u8>> {
    let mut expanded = Vec::new();
    flate2::read::GzDecoder::new(bytes)
        .take(limit + 1)
        .read_to_end(&mut expanded)?;
    ensure!(
        expanded.len() as u64 <= limit,
        "archive expands beyond its size limit"
    );
    Ok(expanded)
}

fn create(path: &Path, executable: bool) -> io::Result<File> {
    let mut options = fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(if executable { 0o755 } else { 0o644 });
    }
    #[cfg(not(unix))]
    let _ = executable;
    options.open(path)
}

fn verify_staged(
    dir: &Path,
    names: &HashSet<String>,
    platform: &Platform,
    release: &ApiRelease,
) -> Result<Build> {
    for required in [BUILD_FILE, platform.cli.as_str(), platform.gui.as_str()] {
        ensure!(names.contains(required), "bundle is missing {required}");
    }
    let build = read_build(&dir.join(BUILD_FILE)).context("bundle BUILD.json")?;
    ensure!(
        build.target == platform.target,
        "bundle target {} is not {}",
        build.target,
        platform.target
    );
    ensure!(build.gui_binary == platform.gui, "bundle GUI name differs");
    ensure!(is_revision(&build.revision), "bundle revision is invalid");
    if is_revision(&release.target_commitish) {
        ensure!(
            release.target_commitish == build.revision,
            "bundle revision {} differs from release commit",
            build.revision
        );
    }
    if let Some(Tag::Review(short)) = tag(&release.tag_name) {
        ensure!(
            build.revision.starts_with(short),
            "bundle revision {} differs from {}",
            build.revision,
            release.tag_name
        );
    }
    for (name, expected) in [
        (&platform.cli, &build.binary_sha256),
        (&platform.gui, &build.gui_binary_sha256),
    ] {
        ensure!(
            &sha256_file(&dir.join(name))? == expected,
            "bundle {name} does not match its BUILD.json"
        );
    }
    Ok(build)
}

fn same_file(path: &Path, other: Option<PathBuf>) -> bool {
    match (fs::canonicalize(path), other.map(fs::canonicalize)) {
        (Ok(a), Some(Ok(b))) => a == b,
        _ => false,
    }
}

struct Swapped {
    dest: PathBuf,
    backup: Option<PathBuf>,
}

/// Move each staged file into `dir` (CLI last), keeping the originals as
/// backups until every move succeeded. Returns backups that are still in use.
fn install(
    dir: &Path,
    staged: &Path,
    names: &HashSet<String>,
    platform: &Platform,
    cli_is_self: bool,
) -> Result<Vec<PathBuf>> {
    let mut done = Vec::new();
    for name in platform.payload() {
        if !names.contains(name) {
            continue;
        }
        let source = staged.join(name);
        let dest = dir.join(name);
        let result = if name == platform.cli && cli_is_self {
            replace_self(dir, name, &source, dest)
        } else {
            swap(dir, name, &source, dest)
        };
        match result {
            Ok(swapped) => done.push(swapped),
            Err(error) => {
                let rollback = rollback(done);
                return Err(error.context(format!(
                    "installing {name} failed; {}",
                    match rollback {
                        Ok(()) => "the previous bundle was restored".to_owned(),
                        Err(e) => format!(
                            "ROLLBACK INCOMPLETE ({e:#}); restore *{BACKUP_SUFFIX} files manually"
                        ),
                    }
                )));
            }
        }
    }
    Ok(done
        .into_iter()
        .filter_map(|s| s.backup)
        .filter(|b| fs::remove_file(b).is_err())
        .collect())
}

/// Windows cannot overwrite the running image; self_replace moves it aside
/// and deletes it after this process exits. Its final step is an atomic
/// rename, so on failure `dest` is either the original or missing.
fn replace_self(dir: &Path, name: &str, source: &Path, dest: PathBuf) -> Result<Swapped> {
    let backup = backup_path(dir, name);
    fs::copy(&dest, &backup).context("copying the running CLI")?;
    if let Err(error) = self_replace::self_replace(source) {
        let _ = if dest.exists() {
            fs::remove_file(&backup)
        } else {
            fs::rename(&backup, &dest)
        };
        return Err(error).context("replacing the running CLI");
    }
    Ok(Swapped {
        dest,
        backup: Some(backup),
    })
}

fn backup_path(dir: &Path, name: &str) -> PathBuf {
    let tag = uuid::Uuid::new_v4().simple().to_string();
    dir.join(format!("{name}.{}{BACKUP_SUFFIX}", &tag[..8]))
}

fn swap(dir: &Path, name: &str, source: &Path, dest: PathBuf) -> Result<Swapped> {
    let backup = if dest.exists() {
        let backup = backup_path(dir, name);
        fs::rename(&dest, &backup).with_context(|| format!("moving {} aside", dest.display()))?;
        Some(backup)
    } else {
        None
    };
    if let Err(error) = fs::rename(source, &dest) {
        if let Some(backup) = &backup {
            let _ = fs::rename(backup, &dest);
        }
        return Err(error).with_context(|| format!("placing {}", dest.display()));
    }
    Ok(Swapped { dest, backup })
}

fn rollback(done: Vec<Swapped>) -> Result<()> {
    let mut failed = Vec::new();
    for swapped in done.into_iter().rev() {
        let restored = match &swapped.backup {
            Some(backup) => fs::rename(backup, &swapped.dest),
            None => fs::remove_file(&swapped.dest),
        };
        if restored.is_err() {
            failed.push(swapped.dest.display().to_string());
        }
    }
    ensure!(failed.is_empty(), "could not restore {}", failed.join(", "));
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn compressed_metadata_cannot_bypass_the_unpacked_byte_limit() {
        use std::io::Write;
        let mut gzip = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::fast());
        gzip.write_all(&[0; 4096]).unwrap();
        let bytes = gzip.finish().unwrap();
        assert!(bytes.len() < 1024);
        let error = expand_tar(&bytes, 1024).unwrap_err();
        assert!(error.to_string().contains("expands beyond"));
        assert_eq!(expand_tar(&bytes, 4096).unwrap().len(), 4096);
    }
}
