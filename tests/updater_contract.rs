//! Updater contracts against a local fixture release server and fixture
//! bundles under the cargo target directory. Child processes only run this
//! repo-built test executable.
#![cfg(all(target_arch = "x86_64", any(target_os = "linux", windows)))]

use amux::updates::{self, Channel, Guidance, InstallMethod, ReleaseSource, Running};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use std::{
    collections::{BTreeMap, HashMap},
    fs,
    io::{Read, Write},
    net::TcpListener,
    path::{Path, PathBuf},
    process::{Child, Command},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

#[cfg(windows)]
const P: (&str, &str, &str, &str, &str) = (
    "x86_64-pc-windows-msvc",
    "windows",
    ".zip",
    "amux.exe",
    "amux-desktop.exe",
);
#[cfg(not(windows))]
const P: (&str, &str, &str, &str, &str) = (
    "x86_64-unknown-linux-gnu",
    "linux",
    ".tar.gz",
    "amux",
    "amux-desktop",
);
const TARGET: &str = P.0;
const CLI: &str = P.3;
const GUI: &str = P.4;

const INSTALLED: &str = "bbbbbbb000000000000000000000000000000000";
const NEWER: &str = "aaaaaaa000000000000000000000000000000000";
const OLDER: &str = "ccccccc000000000000000000000000000000000";

fn sha(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

// ---------- fixture release server ----------

struct Server {
    url: String,
    routes: Arc<Mutex<HashMap<String, Vec<u8>>>>,
    requests: Arc<std::sync::atomic::AtomicUsize>,
}

impl Server {
    fn start() -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let url = format!("http://{}", listener.local_addr().unwrap());
        let routes: Arc<Mutex<HashMap<String, Vec<u8>>>> = Arc::default();
        let shared = routes.clone();
        let requests = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let observed = requests.clone();
        std::thread::spawn(move || {
            for stream in listener.incoming() {
                let Ok(mut stream) = stream else { continue };
                let mut request = Vec::new();
                let mut buf = [0; 1024];
                while !request.windows(4).any(|w| w == b"\r\n\r\n") {
                    match stream.read(&mut buf) {
                        Ok(0) | Err(_) => break,
                        Ok(n) => request.extend_from_slice(&buf[..n]),
                    }
                }
                let text = String::from_utf8_lossy(&request);
                let path = text.split_whitespace().nth(1).unwrap_or("").to_owned();
                observed.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                let body = shared.lock().unwrap().get(&path).cloned();
                let (status, body) = match body {
                    Some(body) => ("200 OK", body),
                    None => ("404 Not Found", Vec::new()),
                };
                let head = format!(
                    "HTTP/1.1 {status}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                let _ = stream.write_all(head.as_bytes());
                let _ = stream.write_all(&body);
            }
        });
        Self {
            url,
            routes,
            requests,
        }
    }

    fn source(&self) -> ReleaseSource {
        ReleaseSource {
            api_url: format!("{}/releases", self.url),
            download_prefix: format!("{}/download/", self.url),
        }
    }

    fn put(&self, path: &str, body: Vec<u8>) {
        self.routes.lock().unwrap().insert(path.into(), body);
    }

    fn publish(&self, releases: &[Value]) {
        self.put("/releases", serde_json::to_vec(releases).unwrap());
    }
}

// ---------- fixture bundles ----------

#[derive(Clone)]
enum Entry {
    File(Vec<u8>),
    Symlink,
    Dir,
}

struct Bundle {
    revision: String,
    target: String,
    candidate: bool,
    cli: Vec<u8>,
    gui: Vec<u8>,
    /// Bundled tool executables (`crate::mcp`), listed in BUILD.json.
    tools: Vec<(String, Vec<u8>)>,
}

fn tool(name: &str) -> String {
    format!("{name}{}", std::env::consts::EXE_SUFFIX)
}

impl Bundle {
    fn new(revision: &str) -> Self {
        Self {
            revision: revision.into(),
            target: TARGET.into(),
            candidate: true,
            cli: format!("cli {revision}").into_bytes(),
            gui: format!("gui {revision}").into_bytes(),
            tools: Vec::new(),
        }
    }

    fn with_tools(mut self) -> Self {
        self.tools = ["agent-tools", "memory"]
            .map(|name| (tool(name), format!("{name} {}", self.revision).into_bytes()))
            .to_vec();
        self
    }

    fn build_json(&self) -> Vec<u8> {
        serde_json::to_vec_pretty(&json!({
            "revision": self.revision,
            "target": self.target,
            "binary_sha256": sha(&self.cli),
            "gui_binary": GUI,
            "gui_binary_sha256": sha(&self.gui),
            "candidate_only": self.candidate,
            "runner": "fixture",
            "bundled_sha256": self.tools.iter()
                .map(|(name, bytes)| (name.clone(), json!(sha(bytes))))
                .collect::<serde_json::Map<_, _>>(),
        }))
        .unwrap()
    }

    fn entries(&self) -> Vec<(String, Entry)> {
        vec![
            ("BUILD.json".into(), Entry::File(self.build_json())),
            ("LICENSE".into(), Entry::File(b"license".to_vec())),
            (CLI.into(), Entry::File(self.cli.clone())),
            (GUI.into(), Entry::File(self.gui.clone())),
        ]
        .into_iter()
        .chain(
            self.tools
                .iter()
                .map(|(name, bytes)| (name.clone(), Entry::File(bytes.clone()))),
        )
        .collect()
    }

    fn install(&self, dir: &Path) {
        fs::create_dir_all(dir).unwrap();
        fs::write(dir.join(CLI), &self.cli).unwrap();
        fs::write(dir.join(GUI), &self.gui).unwrap();
        fs::write(dir.join("BUILD.json"), self.build_json()).unwrap();
        fs::write(dir.join("LICENSE"), b"old license").unwrap();
    }
}

#[cfg(not(windows))]
fn archive(entries: &[(String, Entry)]) -> Vec<u8> {
    let encoder = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::fast());
    let mut builder = tar::Builder::new(encoder);
    for (name, entry) in entries {
        let mut header = tar::Header::new_gnu();
        // Raw name bytes so hostile names reach the extractor unchanged.
        header.as_old_mut().name[..name.len()].copy_from_slice(name.as_bytes());
        header.set_mode(0o755);
        let data: &[u8] = match entry {
            Entry::File(bytes) => {
                header.set_entry_type(tar::EntryType::Regular);
                bytes
            }
            Entry::Symlink => {
                header.set_entry_type(tar::EntryType::Symlink);
                header.set_link_name("/etc/passwd").unwrap();
                &[]
            }
            Entry::Dir => {
                header.set_entry_type(tar::EntryType::Directory);
                &[]
            }
        };
        header.set_size(data.len() as u64);
        header.set_cksum();
        builder.append(&header, data).unwrap();
    }
    builder.into_inner().unwrap().finish().unwrap()
}

#[cfg(windows)]
fn archive(entries: &[(String, Entry)]) -> Vec<u8> {
    use zip::write::SimpleFileOptions;
    let mut writer = zip::ZipWriter::new(std::io::Cursor::new(Vec::new()));
    let options = SimpleFileOptions::default();
    for (name, entry) in entries {
        match entry {
            Entry::File(bytes) => {
                writer.start_file(name.as_str(), options).unwrap();
                writer.write_all(bytes).unwrap();
            }
            Entry::Symlink => writer
                .add_symlink(name.as_str(), "C:\\Windows", options)
                .unwrap(),
            Entry::Dir => writer.add_directory(name.as_str(), options).unwrap(),
        }
    }
    writer.finish().unwrap().into_inner()
}

struct Spec<'a> {
    tag: &'a str,
    revision: &'a str,
    published: &'a str,
    prerelease: bool,
    draft: bool,
    archive: Option<Vec<u8>>,
}

fn review(revision: &str, published: &str) -> Spec<'static> {
    let tag: &'static str = Box::leak(format!("review-{}", &revision[..7]).into_boxed_str());
    let revision: &'static str = Box::leak(revision.to_owned().into_boxed_str());
    let published: &'static str = Box::leak(published.to_owned().into_boxed_str());
    Spec {
        tag,
        revision,
        published,
        prerelease: true,
        draft: false,
        archive: Some(archive(&Bundle::new(revision).entries())),
    }
}

/// Register assets and return GitHub-shaped release metadata.
fn release(server: &Server, spec: Spec) -> Value {
    let mut assets = Vec::new();
    if let Some(bytes) = spec.archive {
        let name = format!("amux-{}-x86_64-{}{}", P.1, &spec.revision[..7], P.2);
        let sums = format!("{}  {name}\n", sha(&bytes)).into_bytes();
        for (asset, body) in [(name, bytes), ("SHA256SUMS".to_owned(), sums)] {
            let path = format!("/download/{}/{asset}", spec.tag);
            assets.push(json!({
                "name": asset,
                "size": body.len(),
                "state": "uploaded",
                "digest": format!("sha256:{}", sha(&body)),
                "browser_download_url": format!("{}{path}", server.url),
            }));
            server.put(&path, body);
        }
    }
    json!({
        "tag_name": spec.tag,
        "name": format!("amux {}", spec.tag),
        "html_url": format!("https://github.com/nitecon/amux/releases/tag/{}", spec.tag),
        "draft": spec.draft,
        "prerelease": spec.prerelease,
        "published_at": spec.published,
        "target_commitish": spec.revision,
        "assets": assets,
    })
}

fn fixture_root() -> tempfile::TempDir {
    let base = Path::new(env!("CARGO_TARGET_TMPDIR")).join("updater-fixtures");
    fs::create_dir_all(&base).unwrap();
    tempfile::tempdir_in(base).unwrap()
}

fn snapshot(dir: &Path) -> BTreeMap<String, Vec<u8>> {
    fs::read_dir(dir)
        .unwrap()
        .flatten()
        // A persistent lock inode is coordination metadata, not bundle payload.
        .filter(|e| e.file_name() != ".amux-update.lock")
        .map(|e| {
            let name = e.file_name().to_string_lossy().into_owned();
            let bytes = fs::read(e.path()).unwrap_or_default();
            (name, bytes)
        })
        .collect()
}

fn installed_revision(dir: &Path) -> String {
    let build: Value = serde_json::from_slice(&fs::read(dir.join("BUILD.json")).unwrap()).unwrap();
    build["revision"].as_str().unwrap().to_owned()
}

/// Installed preview, an older preview with a lexically later hash, and a
/// later preview with a lexically earlier hash.
fn preview_releases(server: &Server) -> Vec<Value> {
    vec![
        release(server, review(OLDER, "2026-10-01T00:00:00Z")),
        release(server, review(INSTALLED, "2026-10-02T00:00:00Z")),
        release(server, review(NEWER, "2026-10-03T00:00:00Z")),
    ]
}

#[tokio::test(flavor = "multi_thread")]
async fn concurrent_update_cannot_cleanup_or_install_during_the_first_download() {
    use std::sync::{atomic::Ordering, mpsc};
    let root = fixture_root();
    let dir = root.path().join("bundle");
    Bundle::new(INSTALLED).install(&dir);
    let before = snapshot(&dir);
    let server = Server::start();
    server.publish(&preview_releases(&server));
    // Hold the first real HTTP response while its updater owns the install.
    let routes = server.routes.clone();
    let (ready_tx, ready_rx) = mpsc::channel();
    let (release_tx, release_rx) = mpsc::channel();
    let gate = std::thread::spawn(move || {
        let _routes = routes.lock().unwrap();
        ready_tx.send(()).unwrap();
        let _ = release_rx.recv_timeout(Duration::from_secs(10));
    });
    ready_rx.recv_timeout(Duration::from_secs(5)).unwrap();
    let running = Running::at(dir.join(CLI));
    let first_running = running.clone();
    let first_source = server.source();
    let first = tokio::spawn(async move { updates::update(&first_running, &first_source).await });
    wait_for("first update HTTP request", || {
        server.requests.load(Ordering::SeqCst) == 1
    });
    let second = tokio::time::timeout(
        Duration::from_secs(1),
        updates::update(&running, &server.source()),
    )
    .await;
    let during = snapshot(&dir);
    let requests = server.requests.load(Ordering::SeqCst);
    release_tx.send(()).unwrap();
    gate.join().unwrap();
    let first_result = first.await.unwrap();
    let error = second
        .expect("second update should refuse without waiting on HTTP")
        .unwrap_err();
    assert!(
        error.to_string().contains("another amux update"),
        "{error:#}"
    );
    assert_eq!(
        during, before,
        "no interleaved installation before verification"
    );
    assert_eq!(requests, 1, "the second updater sent no HTTP request");
    first_result.unwrap();
    assert_eq!(installed_revision(&dir), NEWER);
}

// ---------- discovery and channels ----------

#[tokio::test(flavor = "multi_thread")]
async fn preview_update_follows_publication_time_and_reports_restart() {
    let root = fixture_root();
    let dir = root.path().join("bundle");
    Bundle::new(INSTALLED).install(&dir);
    let server = Server::start();
    let mut releases = preview_releases(&server);
    // Later but not applicable: draft, no platform asset, unrecognised tag.
    let mut draft = review(
        "ddddddd000000000000000000000000000000000",
        "2026-10-04T00:00:00Z",
    );
    draft.draft = true;
    releases.push(release(&server, draft));
    let mut bare = review(
        "eeeeeee000000000000000000000000000000000",
        "2026-10-05T00:00:00Z",
    );
    bare.archive = None;
    releases.push(release(&server, bare));
    let mut nightly = review(
        "fffffff000000000000000000000000000000000",
        "2026-10-06T00:00:00Z",
    );
    nightly.tag = "nightly";
    releases.push(release(&server, nightly));
    server.publish(&releases);

    let gui = Running::at(dir.join(GUI));
    let found = updates::check(&gui, &server.source()).await.unwrap();
    assert_eq!(found.channel, Channel::Preview);
    assert!(
        matches!(&found.method, InstallMethod::Direct { revision, .. } if revision == INSTALLED)
    );
    assert_eq!(found.installed.unwrap().tag, "review-bbbbbbb");
    assert_eq!(found.available.unwrap().tag, "review-aaaaaaa");
    assert_eq!(
        found.guidance,
        Guidance::CliUpdate {
            command: "amux --update".into()
        }
    );
    assert!(!found.restart.required);
    assert_eq!(found.restart.path.as_deref(), Some(dir.join(GUI).as_path()));

    let summary = updates::update(&Running::at(dir.join(CLI)), &server.source())
        .await
        .unwrap();
    assert!(summary.contains("review-aaaaaaa"), "{summary}");
    let files = snapshot(&dir);
    assert_eq!(files[CLI], Bundle::new(NEWER).cli);
    assert_eq!(files[GUI], Bundle::new(NEWER).gui);
    assert_eq!(files["LICENSE"], b"license");
    assert_eq!(installed_revision(&dir), NEWER);
    assert_eq!(
        files.len(),
        4,
        "no staging or backup left: {:?}",
        files.keys()
    );

    let after = updates::check(&gui, &server.source()).await.unwrap();
    assert_eq!(after.installed.unwrap().tag, "review-aaaaaaa");
    assert_eq!(after.available, None, "no repeat or downgrade offer");
    assert!(after.restart.required, "running GUI is older than disk");
    assert!(updates::restart_status(&gui).required);

    let again = updates::update(&Running::at(dir.join(CLI)), &server.source())
        .await
        .unwrap();
    assert_eq!(again, "amux is up to date.");
}

#[tokio::test(flavor = "multi_thread")]
async fn closing_app_abandons_the_download_and_changes_nothing() {
    let root = fixture_root();
    let dir = root.path().join("bundle");
    Bundle::new(INSTALLED).install(&dir);
    let server = Server::start();
    server.publish(&preview_releases(&server));
    let before = snapshot(&dir);
    let running = Running::at(dir.join(CLI));
    assert!(running.close());
    let error = updates::update(&running.clone(), &server.source())
        .await
        .unwrap_err();
    assert!(error.to_string().contains("closing"), "{error:#}");
    assert_eq!(snapshot(&dir), before);
    running.reopen();
    updates::update(&running, &server.source()).await.unwrap();
    assert_eq!(installed_revision(&dir), NEWER);
    assert!(
        running.close(),
        "a finished install no longer blocks closing"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn unpublished_revision_is_never_offered_an_update() {
    let root = fixture_root();
    let dir = root.path().join("bundle");
    let local = "9999999000000000000000000000000000000000";
    Bundle::new(local).install(&dir);
    let server = Server::start();
    server.publish(&preview_releases(&server));
    let before = snapshot(&dir);

    let found = updates::check(&Running::at(dir.join(GUI)), &server.source())
        .await
        .unwrap();
    assert_eq!(found.available, None);
    assert!(found.note.unwrap().contains("not a published release"));
    let summary = updates::update(&Running::at(dir.join(CLI)), &server.source())
        .await
        .unwrap();
    assert!(summary.starts_with("No update applied"), "{summary}");
    assert_eq!(snapshot(&dir), before);
}

#[tokio::test(flavor = "multi_thread")]
async fn preview_installs_first_stable_and_then_follows_stable_versions() {
    let root = fixture_root();
    let dir = root.path().join("bundle");
    Bundle::new(INSTALLED).install(&dir);
    let gui = Running::at(dir.join(GUI));
    let server = Server::start();
    let stable_release = |name, rev, published| {
        let mut spec = review(rev, published);
        spec.tag = name;
        spec.prerelease = false;
        let mut bundle = Bundle::new(rev);
        bundle.candidate = false;
        spec.archive = Some(archive(&bundle.entries()));
        release(&server, spec)
    };
    let mut releases = vec![
        release(&server, review(INSTALLED, "2026-10-01T00:00:00Z")),
        stable_release("v0.0.1", NEWER, "2026-10-02T00:00:00Z"),
    ];
    server.publish(&releases);
    let found = updates::check(&gui, &server.source()).await.unwrap();
    assert_eq!(found.channel, Channel::Preview);
    assert_eq!(found.available.unwrap().tag, "v0.0.1");
    updates::update(&Running::at(dir.join(CLI)), &server.source())
        .await
        .unwrap();
    let stable = updates::check(&gui, &server.source()).await.unwrap();
    assert_eq!(stable.channel, Channel::Stable);
    assert_eq!(stable.installed.unwrap().tag, "v0.0.1");
    assert_eq!(stable.available, None);
    assert!(stable.restart.required);

    // A later preview is no longer offered after entering the stable channel.
    releases.push(release(
        &server,
        review(
            "3333333000000000000000000000000000000000",
            "2026-10-04T00:00:00Z",
        ),
    ));
    releases.push(stable_release("v0.0.2", OLDER, "2026-10-03T00:00:00Z"));
    server.publish(&releases);
    let next = updates::check(&gui, &server.source()).await.unwrap();
    assert_eq!(next.available.unwrap().tag, "v0.0.2");
    updates::update(&Running::at(dir.join(CLI)), &server.source())
        .await
        .unwrap();
    let current = updates::check(&gui, &server.source()).await.unwrap();
    assert_eq!(current.installed.unwrap().tag, "v0.0.2");
    assert_eq!(current.available, None, "no repeat or downgrade offer");
}

#[tokio::test(flavor = "multi_thread")]
async fn stable_bundle_ignores_previews_and_older_versions() {
    let root = fixture_root();
    let dir = root.path().join("bundle");
    let mut stable = Bundle::new(INSTALLED);
    stable.candidate = false;
    stable.install(&dir);
    let server = Server::start();
    let stable_release = |tag: &'static str, rev: &'static str, at: &'static str, pre: bool| {
        let mut spec = review(rev, at);
        spec.tag = tag;
        spec.prerelease = pre;
        let mut bundle = Bundle::new(rev);
        bundle.candidate = false;
        spec.archive = Some(archive(&bundle.entries()));
        release(&server, spec)
    };
    let mut releases = vec![
        stable_release("v0.1.0", INSTALLED, "2026-10-01T00:00:00Z", false),
        stable_release("v0.0.9", OLDER, "2026-10-09T00:00:00Z", false),
        stable_release(
            "v0.2.0",
            "2222222000000000000000000000000000000000",
            "2026-10-03T00:00:00Z",
            true,
        ),
        stable_release("v0.1.1", NEWER, "2026-10-02T00:00:00Z", false),
    ];
    releases.push(release(
        &server,
        review(
            "3333333000000000000000000000000000000000",
            "2026-10-10T00:00:00Z",
        ),
    ));
    server.publish(&releases);

    let found = updates::check(&Running::at(dir.join(GUI)), &server.source())
        .await
        .unwrap();
    assert_eq!(found.channel, Channel::Stable);
    assert_eq!(found.installed.unwrap().tag, "v0.1.0");
    assert_eq!(found.available.unwrap().tag, "v0.1.1");

    updates::update(&Running::at(dir.join(CLI)), &server.source())
        .await
        .unwrap();
    assert_eq!(installed_revision(&dir), NEWER);
}

#[tokio::test(flavor = "multi_thread")]
async fn unverified_bundles_get_guidance_and_are_never_written() {
    let root = fixture_root();
    let server = Server::start();
    server.publish(&preview_releases(&server));
    let modified = root.path().join("modified");
    Bundle::new(INSTALLED).install(&modified);
    fs::write(modified.join(GUI), b"locally rebuilt").unwrap();
    let missing = root.path().join("no-build");
    Bundle::new(INSTALLED).install(&missing);
    fs::remove_file(missing.join("BUILD.json")).unwrap();

    for dir in [modified, missing] {
        let before = snapshot(&dir);
        let found = updates::check(&Running::at(dir.join(GUI)), &server.source())
            .await
            .unwrap();
        assert_eq!(found.method, InstallMethod::Unmanaged);
        assert!(matches!(found.guidance, Guidance::Manual { .. }));
        assert!(found.note.is_some());
        let error = updates::update(&Running::at(dir.join(CLI)), &server.source())
            .await
            .unwrap_err()
            .to_string();
        assert!(error.contains("Nothing was changed"), "{error}");
        assert_eq!(snapshot(&dir), before);
    }
}

#[cfg(unix)]
#[tokio::test(flavor = "multi_thread")]
async fn homebrew_keg_gets_brew_guidance_and_restart_detection() {
    let root = fixture_root();
    let prefix = fs::canonicalize(root.path()).unwrap().join("brew");
    let keg = |version: &str| prefix.join("Cellar/amux").join(version).join("bin");
    // A valid BUILD.json must not turn a Homebrew keg into a writable bundle.
    Bundle::new(INSTALLED).install(&keg("0.1.0"));
    fs::create_dir_all(prefix.join("opt")).unwrap();
    std::os::unix::fs::symlink(prefix.join("Cellar/amux/0.1.0"), prefix.join("opt/amux")).unwrap();
    let server = Server::start();
    let mut stable = review(NEWER, "2026-10-03T00:00:00Z");
    stable.tag = "v0.1.1";
    stable.prerelease = false;
    stable.archive = None;
    let releases = vec![release(&server, stable)];
    server.publish(&releases);

    let gui = Running::at(keg("0.1.0").join(GUI));
    let before = snapshot(&keg("0.1.0"));
    let found = updates::check(&gui, &server.source()).await.unwrap();
    assert_eq!(
        found.method,
        InstallMethod::Homebrew {
            prefix: prefix.clone(),
            cask: false
        }
    );
    assert_eq!(
        found.guidance,
        Guidance::Homebrew {
            command: "brew update && brew upgrade amux".into()
        }
    );
    assert_eq!(found.available.unwrap().tag, "v0.1.1");
    let opt_gui = prefix.join("opt/amux/bin").join(GUI);
    assert_eq!(found.restart.path.as_deref(), Some(opt_gui.as_path()));
    assert!(!found.restart.required);

    let error = updates::update(&gui, &server.source())
        .await
        .unwrap_err()
        .to_string();
    assert!(
        error.contains("brew update && brew upgrade amux"),
        "{error}"
    );
    assert_eq!(snapshot(&keg("0.1.0")), before);

    // `brew upgrade` installs a new keg and repoints opt; the old keg stays.
    Bundle::new(NEWER).install(&keg("0.1.1"));
    fs::remove_file(prefix.join("opt/amux")).unwrap();
    std::os::unix::fs::symlink(prefix.join("Cellar/amux/0.1.1"), prefix.join("opt/amux")).unwrap();
    let restart = updates::restart_status(&gui);
    assert!(restart.required);
    assert_eq!(restart.path.as_deref(), Some(opt_gui.as_path()));
}

// ---------- integrity and failure ----------

#[tokio::test(flavor = "multi_thread")]
async fn tampered_or_malformed_releases_leave_the_install_untouched() {
    let good = Bundle::new(NEWER);
    let with = |extra: Vec<(String, Entry)>| {
        let mut entries = good.entries();
        entries.extend(extra);
        archive(&entries)
    };
    let without_gui = archive(
        &good
            .entries()
            .into_iter()
            .filter(|(n, _)| n != GUI)
            .collect::<Vec<_>>(),
    );
    let mut foreign = Bundle::new(NEWER);
    foreign.target = "aarch64-apple-darwin".into();
    let mislabeled = Bundle::new("1234567000000000000000000000000000000000");
    let mut forged_gui = good.entries();
    forged_gui[3].1 = Entry::File(b"forged".to_vec());
    let tooled = Bundle::new(NEWER).with_tools();
    let mut forged_tool = tooled.entries();
    forged_tool[4].1 = Entry::File(b"forged".to_vec());

    // (expected error, archive, metadata tampering)
    let mut cases: Vec<(&str, Vec<u8>, Option<&str>)> = vec![
        ("bundle is missing", without_gui, None),
        ("does not match its BUILD.json", archive(&forged_gui), None),
        ("does not match its BUILD.json", archive(&forged_tool), None),
        (
            "and its BUILD.json entry differ",
            with(vec![(tool("memory"), Entry::File(b"unlisted".to_vec()))]),
            None,
        ),
        (
            "aarch64-apple-darwin is not",
            archive(&foreign.entries()),
            None,
        ),
        (
            "differs from release commit",
            archive(&mislabeled.entries()),
            None,
        ),
        (
            "\"../escape\" is not a plain file name",
            with(vec![("../escape".into(), Entry::File(b"x".to_vec()))]),
            None,
        ),
        (
            "\"sub/x\" is not a plain file name",
            with(vec![("sub/x".into(), Entry::File(b"x".to_vec()))]),
            None,
        ),
        (
            "README is not a regular file",
            with(vec![("README".into(), Entry::Symlink)]),
            None,
        ),
        (
            "docs is not a regular file",
            with(vec![("docs/".into(), Entry::Dir)]),
            None,
        ),
        (
            "does not match SHA256SUMS",
            archive(&good.entries()),
            Some("sums"),
        ),
        (
            "tar.gz does not match the published digest",
            archive(&good.entries()),
            Some("digest"),
        ),
        ("is larger than", archive(&good.entries()), Some("size")),
    ];
    #[cfg(not(windows))]
    cases.push((
        "archive repeats",
        with(vec![(GUI.into(), Entry::File(b"second".to_vec()))]),
        None,
    ));
    #[cfg(windows)]
    for case in &mut cases {
        case.0 = Box::leak(case.0.replace("tar.gz", "zip").into_boxed_str());
    }

    for (case, bytes, metadata) in cases {
        let root = fixture_root();
        let dir = root.path().join("bundle");
        Bundle::new(INSTALLED).install(&dir);
        let server = Server::start();
        let mut spec = review(NEWER, "2026-10-03T00:00:00Z");
        spec.archive = Some(bytes);
        let mut newer = release(&server, spec);
        let name = newer["assets"][0]["name"].as_str().unwrap().to_owned();
        let route = format!("/download/review-aaaaaaa/{name}");
        match metadata {
            Some("sums") => {
                let sums = format!("{}  {name}\n", "0".repeat(64)).into_bytes();
                newer["assets"][1]["digest"] = json!(format!("sha256:{}", sha(&sums)));
                server.put("/download/review-aaaaaaa/SHA256SUMS", sums);
            }
            Some("digest") => {
                newer["assets"][0]["digest"] = json!(format!("sha256:{}", "0".repeat(64)))
            }
            Some("size") => {
                let mut body = server.routes.lock().unwrap()[&route].clone();
                body.extend_from_slice(b"appended");
                server.put(&route, body);
            }
            _ => {}
        }
        server.publish(&[
            release(&server, review(INSTALLED, "2026-10-02T00:00:00Z")),
            newer,
        ]);
        let before = snapshot(&dir);

        let result = updates::update(&Running::at(dir.join(CLI)), &server.source()).await;
        let error = format!("{:#}", result.expect_err(case));
        assert!(error.contains(case), "expected {case:?}, got {error}");
        assert_eq!(snapshot(&dir), before, "{case}: install changed");
        assert!(
            !root.path().join("escape").exists(),
            "{case}: wrote outside"
        );
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn bundled_tools_are_installed_with_amux() {
    let root = fixture_root();
    let dir = root.path().join("bundle");
    Bundle::new(INSTALLED).install(&dir);
    let server = Server::start();
    let newer = Bundle::new(NEWER).with_tools();
    let mut spec = review(NEWER, "2026-10-03T00:00:00Z");
    spec.archive = Some(archive(&newer.entries()));
    server.publish(&[
        release(&server, review(INSTALLED, "2026-10-02T00:00:00Z")),
        release(&server, spec),
    ]);
    updates::update(&Running::at(dir.join(CLI)), &server.source())
        .await
        .unwrap();
    for (name, bytes) in &newer.tools {
        assert_eq!(&fs::read(dir.join(name)).unwrap(), bytes, "{name}");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = fs::metadata(dir.join(name)).unwrap().permissions().mode();
            assert_eq!(mode & 0o111, 0o111, "{name} is executable");
        }
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn assets_outside_the_official_download_path_are_ignored() {
    let root = fixture_root();
    let dir = root.path().join("bundle");
    Bundle::new(INSTALLED).install(&dir);
    let server = Server::start();
    let mut newer = release(&server, review(NEWER, "2026-10-03T00:00:00Z"));
    newer["assets"][0]["browser_download_url"] = json!(format!("{}/elsewhere/archive", server.url));
    server.publish(&[
        release(&server, review(INSTALLED, "2026-10-02T00:00:00Z")),
        newer,
    ]);
    let before = snapshot(&dir);

    let found = updates::check(&Running::at(dir.join(GUI)), &server.source())
        .await
        .unwrap();
    assert_eq!(found.available, None);
    updates::update(&Running::at(dir.join(CLI)), &server.source())
        .await
        .unwrap();
    assert_eq!(snapshot(&dir), before);
}

#[cfg(windows)]
#[tokio::test(flavor = "multi_thread")]
async fn locked_file_rolls_back_the_whole_bundle() {
    use std::os::windows::fs::OpenOptionsExt;
    let root = fixture_root();
    let dir = root.path().join("bundle");
    Bundle::new(INSTALLED).install(&dir);
    let server = Server::start();
    server.publish(&preview_releases(&server));
    let before = snapshot(&dir);

    // No FILE_SHARE_DELETE: the CLI (installed last) cannot be moved aside.
    let hold = fs::OpenOptions::new()
        .read(true)
        .share_mode(1 /* FILE_SHARE_READ */)
        .open(dir.join(CLI))
        .unwrap();
    let error = updates::update(&Running::at(dir.join(CLI)), &server.source())
        .await
        .unwrap_err();
    drop(hold);
    assert!(
        format!("{error:#}").contains("previous bundle was restored"),
        "{error:#}"
    );
    assert_eq!(snapshot(&dir), before);
}

// ---------- running executables ----------

/// Copy this repo-built test executable into a fixture bundle so it can run
/// from there as the installed CLI or GUI.
fn install_self_as(dir: &Path, name: &str) -> Bundle {
    let me = fs::read(std::env::current_exe().unwrap()).unwrap();
    let mut bundle = Bundle::new(INSTALLED);
    if name == CLI {
        bundle.cli = me;
    } else {
        bundle.gui = me;
    }
    bundle.install(dir);
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(dir.join(name), fs::Permissions::from_mode(0o755)).unwrap();
    }
    bundle
}

fn spawn_fixture(exe: &Path, test: &str, envs: &[(&str, &Path)]) -> Child {
    let mut command = Command::new(exe);
    command.args([
        "--exact",
        test,
        "--ignored",
        "--nocapture",
        "--test-threads=1",
    ]);
    for (key, value) in envs {
        command.env(key, value);
    }
    command.spawn().unwrap()
}

fn wait_for(what: &str, mut ready: impl FnMut() -> bool) {
    let deadline = Instant::now() + Duration::from_secs(60);
    while !ready() {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        std::thread::sleep(Duration::from_millis(100));
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn running_gui_is_replaced_without_being_stopped() {
    let root = fixture_root();
    let dir = root.path().join("bundle");
    install_self_as(&dir, GUI);
    let server = Server::start();
    server.publish(&preview_releases(&server));
    let started = root.path().join("started");
    let stop = root.path().join("stop");
    let gui = Running::at(dir.join(GUI));
    let mut child = spawn_fixture(
        &dir.join(GUI),
        "fixture_gui_sleeper",
        &[
            ("AMUX_UPDATER_FIXTURE_STARTED", &started),
            ("AMUX_UPDATER_FIXTURE_STOP", &stop),
        ],
    );
    wait_for("fixture GUI start", || started.exists());

    let summary = updates::update(&Running::at(dir.join(CLI)), &server.source())
        .await
        .unwrap();
    assert!(
        child.try_wait().unwrap().is_none(),
        "update stopped the running GUI"
    );
    assert_eq!(fs::read(dir.join(GUI)).unwrap(), Bundle::new(NEWER).gui);
    assert_eq!(installed_revision(&dir), NEWER);
    let restart = updates::restart_status(&gui);
    assert!(restart.required);
    assert_eq!(restart.path.as_deref(), Some(dir.join(GUI).as_path()));
    if cfg!(windows) {
        // The mapped image can be renamed but not deleted while it runs.
        assert!(summary.contains(".amux-old"), "{summary}");
    }

    fs::write(&stop, b"").unwrap();
    assert!(child.wait().unwrap().success());
    wait_for("backup removal", || {
        updates::cleanup_backups(&dir).is_empty()
    });
    assert!(
        snapshot(&dir).keys().all(|n| !n.ends_with(".amux-old")),
        "{:?}",
        snapshot(&dir).keys()
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn running_cli_replaces_itself() {
    let root = fixture_root();
    let dir = root.path().join("bundle");
    install_self_as(&dir, CLI);
    let server = Server::start();
    server.publish(&preview_releases(&server));
    let source = PathBuf::from(server.url.clone());

    let child = spawn_fixture(
        &dir.join(CLI),
        "fixture_cli_updater",
        &[("AMUX_UPDATER_FIXTURE_SOURCE", &source)],
    );
    let status = tokio::task::spawn_blocking(move || child.wait_with_output())
        .await
        .unwrap()
        .unwrap()
        .status;
    assert!(status.success());
    assert_eq!(fs::read(dir.join(CLI)).unwrap(), Bundle::new(NEWER).cli);
    assert_eq!(fs::read(dir.join(GUI)).unwrap(), Bundle::new(NEWER).gui);
    assert_eq!(installed_revision(&dir), NEWER);
    // On Windows self_replace's own helper deletes the moved-aside image some
    // time after exit; that deferred cleanup is the crate's, not ours.
    let expected = [CLI, GUI, "BUILD.json", "LICENSE"];
    let leftovers: Vec<String> = snapshot(&dir)
        .into_keys()
        .filter(|n| !expected.contains(&n.as_str()) && !n.contains(".__"))
        .collect();
    assert!(leftovers.is_empty(), "updater left {leftovers:?}");
}

#[test]
#[ignore = "fixture process for running_gui_is_replaced_without_being_stopped"]
fn fixture_gui_sleeper() {
    let started = std::env::var_os("AMUX_UPDATER_FIXTURE_STARTED").unwrap();
    let stop = PathBuf::from(std::env::var_os("AMUX_UPDATER_FIXTURE_STOP").unwrap());
    fs::write(started, b"").unwrap();
    let deadline = Instant::now() + Duration::from_secs(120);
    while !stop.exists() && Instant::now() < deadline {
        std::thread::sleep(Duration::from_millis(50));
    }
}

#[test]
#[ignore = "fixture process for running_cli_replaces_itself"]
fn fixture_cli_updater() {
    let url = std::env::var("AMUX_UPDATER_FIXTURE_SOURCE").unwrap();
    let source = ReleaseSource {
        api_url: format!("{url}/releases"),
        download_prefix: format!("{url}/download/"),
    };
    let running = Running::capture().unwrap();
    let summary = tokio::runtime::Runtime::new()
        .unwrap()
        .block_on(updates::update(&running, &source))
        .unwrap();
    println!("{summary}");
}

#[test]
fn check_future_is_send_for_the_desktop() {
    fn send<T: Send>(_: &T) {}
    let running = Running::at(PathBuf::from("amux"));
    send(&updates::check(&running, &ReleaseSource::github()));
}

#[test]
fn cleanup_removes_only_updater_owned_backup_names() {
    let root = fixture_root();
    let manual = root.path().join("manual-copy.amux-old");
    let named = root.path().join(format!("{GUI}.manual.amux-old"));
    let generated = root.path().join(format!("{GUI}.abcdef12.amux-old"));
    fs::write(&manual, b"user backup").unwrap();
    fs::write(&named, b"user named backup").unwrap();
    fs::write(&generated, b"updater backup").unwrap();
    assert!(updates::cleanup_backups(root.path()).is_empty());
    assert!(manual.exists(), "unrelated backup was deleted");
    assert!(named.exists(), "manually named app backup was deleted");
    assert!(!generated.exists());
}
