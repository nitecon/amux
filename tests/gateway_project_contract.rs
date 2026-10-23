//! Automatic gateway project resolution: real temporary Git workspaces and a
//! private local HTTP fixture. No live gateway, user keys or model turns.
use amux::{
    gateway::Gateway,
    gateway_project::{matching, origin, repository},
};
use serde_json::{Value, json};
use std::{
    io::{Read, Write},
    net::TcpListener,
    path::{Path, PathBuf},
    process::Command,
    thread,
    time::{Duration, Instant},
};

const REMOTE: &str = "git.example.test/team/tool";

struct Request {
    head: String,
    body: Value,
}

/// Serves one raw HTTP response per expected request, then returns what it saw.
fn server(replies: Vec<Vec<u8>>) -> (String, thread::JoinHandle<Vec<Request>>) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    listener.set_nonblocking(true).unwrap();
    let handle = thread::spawn(move || {
        let mut seen = Vec::new();
        for reply in replies {
            let deadline = Instant::now() + Duration::from_secs(15);
            let mut stream = loop {
                match listener.accept() {
                    Ok((stream, _)) => break stream,
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        assert!(Instant::now() < deadline, "missing gateway request");
                        thread::sleep(Duration::from_millis(5));
                    }
                    Err(e) => panic!("accept: {e}"),
                }
            };
            stream.set_nonblocking(false).unwrap();
            stream
                .set_read_timeout(Some(Duration::from_secs(3)))
                .unwrap();
            let mut bytes = Vec::new();
            let (end, length) = loop {
                let mut buf = [0; 4096];
                let count = stream.read(&mut buf).unwrap();
                assert_ne!(count, 0, "request closed before headers");
                bytes.extend_from_slice(&buf[..count]);
                if let Some(end) = bytes.windows(4).position(|w| w == b"\r\n\r\n") {
                    let head = String::from_utf8_lossy(&bytes[..end]).to_lowercase();
                    let length = head
                        .lines()
                        .find_map(|l| l.strip_prefix("content-length: ")?.parse().ok())
                        .unwrap_or(0);
                    break (end + 4, length);
                }
            };
            while bytes.len() < end + length {
                let mut buf = [0; 4096];
                let count = stream.read(&mut buf).unwrap();
                assert_ne!(count, 0, "incomplete request body");
                bytes.extend_from_slice(&buf[..count]);
            }
            seen.push(Request {
                head: String::from_utf8_lossy(&bytes[..end]).to_lowercase(),
                body: serde_json::from_slice(&bytes[end..end + length]).unwrap_or(Value::Null),
            });
            let _ = stream.write_all(&reply);
        }
        // A request beyond the script (e.g. a followed redirect) would be unanswered.
        thread::sleep(Duration::from_millis(200));
        assert!(
            listener.accept().is_err(),
            "unexpected extra gateway request"
        );
        seen
    });
    (url, handle)
}

fn reply(status: u16, body: &Value) -> Vec<u8> {
    let body = serde_json::to_vec(body).unwrap();
    let mut out = format!(
        "HTTP/1.1 {status} Test\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    )
    .into_bytes();
    out.extend(body);
    out
}

fn git(directory: &Path, args: &[&str]) {
    let mut command = Command::new("git");
    command.arg("-C").arg(directory).args(args);
    for (key, _) in std::env::vars_os() {
        if key.to_string_lossy().starts_with("GIT_") {
            command.env_remove(key);
        }
    }
    let status = command.env("GIT_CONFIG_NOSYSTEM", "1").status().unwrap();
    assert!(status.success(), "git {args:?}");
}

/// A temporary repository whose origin is `remote`.
fn workspace(remote: Option<&str>) -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    git(dir.path(), &["init", "--quiet"]);
    if let Some(remote) = remote {
        git(dir.path(), &["remote", "add", "origin", remote]);
    }
    dir
}

fn resolve(gateway: &Gateway, directory: PathBuf) -> anyhow::Result<String> {
    tokio::runtime::Runtime::new()
        .unwrap()
        .block_on(gateway.resolve_project(directory))
}

#[test]
fn remotes_normalize_like_cmux_including_ports() {
    for (input, expected) in [
        ("git@GitHub.com:Org/Repo.git", Some("github.com/org/repo")),
        ("https://github.com/org/repo", Some("github.com/org/repo")),
        (
            "https://github.com/org/repo.git/",
            Some("github.com/org/repo"),
        ),
        ("http://github.com/org/repo", Some("github.com/org/repo")),
        (
            "ssh://git@github.com/org/repo.git",
            Some("github.com/org/repo"),
        ),
        (
            "ssh://git@github.com:22/org/repo",
            Some("github.com/org/repo"),
        ),
        (
            "git://github.com:9418/org/repo",
            Some("github.com/org/repo"),
        ),
        (
            "https://github.com:443/org/repo",
            Some("github.com/org/repo"),
        ),
        ("github.com/org/repo", Some("github.com/org/repo")),
        (
            "ssh://git@git.example.test:2222/team/tool.git",
            Some("git.example.test:2222/team/tool"),
        ),
        (
            "https://git.example.test:8443/team/tool",
            Some("git.example.test:8443/team/tool"),
        ),
        ("https://user:secret@github.com/org/repo", None),
        ("https://github.com/org/repo?x=1", None),
        ("https://github.com/org/repo#x", None),
        ("file:///srv/repo.git", None),
        ("/srv/repo.git", None),
        ("https://github.com/", None),
        ("github.com/org/\nrepo", None),
        ("", None),
    ] {
        assert_eq!(repository(input).as_deref(), expected, "{input}");
    }
    assert_eq!(
        repository(&format!("github.com/{}", "a".repeat(4096))),
        None
    );
}

#[test]
fn origin_is_read_from_repository_subdirectory_and_worktree() {
    let repo = workspace(Some("git@git.example.test:Team/Tool.git"));
    assert_eq!(origin(repo.path()).unwrap(), REMOTE);
    let nested = repo.path().join("src/deep");
    std::fs::create_dir_all(&nested).unwrap();
    assert_eq!(origin(&nested).unwrap(), REMOTE);

    git(
        repo.path(),
        &[
            "-c",
            "user.name=t",
            "-c",
            "user.email=t@example.test",
            "commit",
            "--quiet",
            "--allow-empty",
            "-m",
            "init",
        ],
    );
    let worktree = repo.path().join("wt");
    git(
        repo.path(),
        &["worktree", "add", "--quiet", worktree.to_str().unwrap()],
    );
    assert_eq!(origin(&worktree).unwrap(), REMOTE);
}

#[test]
fn missing_or_unsupported_origins_fail_without_echoing_the_remote() {
    let none = workspace(None);
    assert!(origin(none.path()).is_err());
    let plain = tempfile::tempdir().unwrap();
    assert!(origin(plain.path()).is_err());
    assert!(origin(&plain.path().join("missing")).is_err());
    let secret = workspace(Some("https://user:hunter2@git.example.test/team/tool"));
    let error = format!("{:#}", origin(secret.path()).unwrap_err());
    assert!(
        !error.contains("hunter2") && !error.contains("example.test"),
        "{error}"
    );
}

#[test]
fn listing_maps_only_a_unique_canonical_remote() {
    // Existing aliases are preserved: the identity is the gateway's, not the repo slug.
    let projects = json!([
        {"ident": "cmux-gtk", "canonical_remote": "github.com/nitecon/cmux-gtk"},
        {"ident": "tool", "canonical_remote": "github.com/other/tool"},
        {"ident": "adhoc", "canonical_remote": null},
        {"ident": "odd", "canonical_remote": "not a remote"},
        {"ident": "alias", "canonical_remote": "git.example.test/Team/Tool.git"},
    ]);
    assert_eq!(
        matching(REMOTE, &projects).unwrap().as_deref(),
        Some("alias")
    );
    assert_eq!(
        matching(
            "github.com/nitecon/cmux-linux",
            &json!([{"ident":"cmux-gtk","canonical_remote":"github.com/nitecon/cmux-gtk"}])
        )
        .unwrap(),
        None
    );
    assert_eq!(
        matching("github.com/nitecon/cmux-gtk", &projects)
            .unwrap()
            .as_deref(),
        Some("cmux-gtk")
    );
    // Same basename under a different owner and nameonly projects never match.
    assert_eq!(
        matching("github.com/someone/tool", &projects).unwrap(),
        None
    );
    assert_eq!(
        matching("git.example.test/x/adhoc", &projects).unwrap(),
        None
    );
    assert_eq!(matching(REMOTE, &json!([])).unwrap(), None);
}

#[test]
fn malformed_duplicate_and_ambiguous_listings_are_refused() {
    let invalid = [
        json!({"ident": "a"}),
        json!([{"canonical_remote": REMOTE}]),
        json!([{"ident": "", "canonical_remote": REMOTE}]),
        json!([{"ident": 7, "canonical_remote": REMOTE}]),
        json!([{"ident": "a", "canonical_remote": 7}]),
        json!([{"ident": "a"}, {"ident": "a"}]),
        json!([
            {"ident": "a", "canonical_remote": REMOTE},
            {"ident": "b", "canonical_remote": "https://git.example.test/team/tool.git"},
        ]),
        // A matched identity must be usable as a project path segment.
        json!([{"ident": "team/tool", "canonical_remote": REMOTE}]),
        Value::Array(vec![json!({"ident": "x"}); 4097]),
    ];
    for projects in invalid {
        assert!(matching(REMOTE, &projects).is_err());
    }
}

#[test]
fn matching_project_resolves_with_one_authenticated_read() {
    let repo = workspace(Some("ssh://git@git.example.test/team/tool.git"));
    let (url, server) = server(vec![reply(
        200,
        &json!([{"ident": "tool-alias", "canonical_remote": REMOTE}]),
    )]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    assert_eq!(
        resolve(&gateway, repo.path().join(".")).unwrap(),
        "tool-alias"
    );
    let seen = server.join().unwrap();
    assert_eq!(seen.len(), 1);
    assert!(
        seen[0].head.starts_with("get /v1/projects http/1.1"),
        "{}",
        seen[0].head
    );
    assert!(seen[0].head.contains("authorization: bearer test-only"));
}

#[test]
fn missing_project_is_registered_then_verified_by_reread() {
    let repo = workspace(Some("git@git.example.test:team/tool.git"));
    let (url, server) = server(vec![
        reply(
            200,
            &json!([{"ident": "tool", "canonical_remote": "github.com/other/tool"}]),
        ),
        reply(200, &json!({"ident": "tool-2", "kind": "repo"})),
        reply(
            200,
            &json!([
                {"ident": "tool", "canonical_remote": "github.com/other/tool"},
                {"ident": "tool-2", "canonical_remote": REMOTE},
            ]),
        ),
    ]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    assert_eq!(resolve(&gateway, repo.path().into()).unwrap(), "tool-2");
    let seen = server.join().unwrap();
    assert!(seen[1].head.starts_with("post /v1/projects http/1.1"));
    assert!(seen[1].head.contains("authorization: bearer test-only"));
    assert_eq!(
        seen[1].body,
        json!({"ident": REMOTE, "repo_url": format!("https://{REMOTE}")})
    );
    assert!(seen[2].head.starts_with("get /v1/projects "));
}

#[test]
fn registration_without_an_authoritative_mapping_fails() {
    let repo = workspace(Some("git@git.example.test:team/tool.git"));
    // The gateway kept an existing same-named project for another repository.
    let listing = json!([{"ident": "tool", "canonical_remote": "github.com/other/tool"}]);
    let (url, first) = server(vec![
        reply(200, &listing),
        reply(200, &json!({"ident": "tool"})),
        reply(200, &listing),
    ]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    assert!(resolve(&gateway, repo.path().into()).is_err());
    assert_eq!(first.join().unwrap().len(), 3);

    for registered in [reply(200, &json!({})), reply(500, &json!({"error": "x"}))] {
        let (url, server) = server(vec![reply(200, &json!([])), registered]);
        let gateway = Gateway::new(&url, "test-only".into()).unwrap();
        assert!(resolve(&gateway, repo.path().into()).is_err());
        assert_eq!(server.join().unwrap().len(), 2);
    }
}

#[test]
fn unauthorized_redirected_oversized_and_invalid_listings_fail() {
    let repo = workspace(Some("git@git.example.test:team/tool.git"));
    let mut oversized =
        b"HTTP/1.1 200 Test\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n["
            .to_vec();
    oversized.extend(vec![b' '; 4 * 1024 * 1024 + 1]);
    for response in [
        reply(401, &json!({"error": "unauthorized"})),
        b"HTTP/1.1 302 Found\r\nLocation: http://127.0.0.1:9/v1/projects\r\nContent-Length: 0\r\nConnection: close\r\n\r\n".to_vec(),
        oversized,
        b"HTTP/1.1 200 Test\r\nContent-Length: 3\r\nConnection: close\r\n\r\nnot".to_vec(),
        reply(200, &json!({"projects": []})),
    ] {
        let (url, server) = server(vec![response]);
        let gateway = Gateway::new(&url, "test-only".into()).unwrap();
        let error = format!("{:#}", resolve(&gateway, repo.path().into()).unwrap_err());
        assert!(!error.contains("test-only"), "{error}");
        assert_eq!(server.join().unwrap().len(), 1);
    }
}

#[test]
fn workspace_without_origin_makes_no_gateway_request() {
    let repo = workspace(None);
    let (url, server) = server(vec![]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    assert!(resolve(&gateway, repo.path().into()).is_err());
    assert!(server.join().unwrap().is_empty());
}
