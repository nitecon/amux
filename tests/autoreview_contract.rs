use amux::{
    autoreview::*,
    identity::{AgentIdentity, Provider},
};
use std::{path::Path, process::Command};
use uuid::Uuid;

fn fixture() -> tempfile::TempDir {
    let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    std::fs::create_dir_all(&root).unwrap();
    tempfile::tempdir_in(root).unwrap()
}
fn git(directory: &Path, args: &[&str]) -> String {
    let output = Command::new("git")
        .arg("-C")
        .arg(directory)
        .args(args)
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout).unwrap().trim().to_owned()
}
fn actor(directory: &Path) -> AgentIdentity {
    AgentIdentity {
        session_id: Uuid::new_v4(),
        instance_id: Uuid::new_v4(),
        provider: Provider::Claude,
        os: "fixture".into(),
        project_directory: directory.into(),
        project_id: Uuid::new_v4(),
        agent_id: "fixture-reviewer".into(),
        name: "Reviewer".into(),
    }
}

#[test]
fn five_newest_unreviewed_commits_require_explicit_exact_agent_completion() {
    let dir = fixture();
    git(dir.path(), &["init", "--quiet"]);
    git(dir.path(), &["config", "user.name", "Codex"]);
    git(
        dir.path(),
        &["config", "user.email", "fixture@example.invalid"],
    );
    let mut ids = Vec::new();
    for i in 0..7 {
        git(
            dir.path(),
            &[
                "-c",
                "commit.gpgsign=false",
                "commit",
                "--quiet",
                "--allow-empty",
                "-m",
                &format!("change {i}"),
            ],
        );
        ids.push(git(dir.path(), &["rev-parse", "HEAD"]));
    }
    let path = dir.path().join("history.json");
    let batch = prepare(dir.path(), &path).unwrap().unwrap();
    assert_eq!(
        batch.commits,
        ids.iter().rev().take(5).cloned().collect::<Vec<_>>()
    );
    assert_eq!(batch.preferred, Some(Provider::Claude));
    let owner = actor(dir.path());
    reserve(&path, &batch, &owner).unwrap();
    assert!(load(&path).unwrap().reviewed.is_empty());
    assert!(prepare(dir.path(), &path).unwrap().is_none());
    assert!(reserve(&path, &batch, &owner).is_err());
    let mut peer = owner.clone();
    peer.instance_id = Uuid::new_v4();
    assert!(complete(&path, batch.id, &peer).is_err());
    assert!(load(&path).unwrap().reviewed.is_empty());
    complete(&path, batch.id, &owner).unwrap();
    let next = prepare(dir.path(), &path).unwrap().unwrap();
    assert_eq!(
        next.commits,
        ids[..2].iter().rev().cloned().collect::<Vec<_>>()
    );
    assert!(reserve(&path, &batch, &owner).is_err());
    reserve(&path, &next, &owner).unwrap();
    release(&path, next.id).unwrap();
    assert_eq!(
        prepare(dir.path(), &path).unwrap().unwrap().commits,
        next.commits
    );
    reserve(&path, &next, &owner).unwrap();
    complete(&path, next.id, &owner).unwrap();
    assert!(prepare(dir.path(), &path).unwrap().is_none());
    assert_eq!(load(&path).unwrap().reviewed.len(), 7);
}

#[test]
fn fallback_guidelines_cli_is_exact_and_review_prompt_is_read_only() {
    let output = Command::new(env!("CARGO_BIN_EXE_amux"))
        .arg("--get-guidelines")
        .output()
        .unwrap();
    assert!(output.status.success());
    assert_eq!(
        String::from_utf8(output.stdout).unwrap(),
        include_str!("../docs/general_guidelines.md")
    );
    let batch = Batch {
        id: Uuid::new_v4(),
        commits: vec!["a".repeat(40)],
        preferred: None,
    };
    let text = prompt(&batch, "reviewer-1");
    assert!(text.contains("amux --get-guidelines"));
    assert!(text.contains("--review-finding"));
    assert!(text.contains("Do not implement fixes, edit files, stage/commit/push code, open PRs"));
    assert!(text.contains(&format!("reviews complete {}", batch.id)));
}

#[test]
fn findings_wait_for_human_assignment_and_corrupt_history_is_preserved() {
    let mut task = serde_json::json!({"labels":["autoreview-finding"],"status":"todo"});
    assert!(held_finding(&task));
    assert!(
        amux::delegation::Event::parse(&serde_json::json!({"kind":"task_created","task":task}))
            .unwrap()
            .is_none()
    );
    task["owner_origin"] = serde_json::json!({"session_id":Uuid::new_v4()});
    assert!(!held_finding(&task));
    let dir = fixture();
    let path = dir.path().join("history.json");
    std::fs::write(&path, b"corrupt").unwrap();
    assert!(load(&path).is_err());
    assert!(release(&path, Uuid::new_v4()).is_err());
    assert_eq!(std::fs::read(path).unwrap(), b"corrupt");
}

#[test]
fn finding_cli_posts_a_held_task_with_exact_agent_attribution() {
    use std::io::{Read, Write};
    let dir = fixture();
    let identity = AgentIdentity::for_terminal(Provider::Codex, dir.path(), 1).unwrap();
    amux::identity::AgentRecord::inspect(identity.clone(), std::process::id())
        .unwrap()
        .register_at(dir.path())
        .unwrap();
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let expected_agent = identity.gateway_id();
    let server = std::thread::spawn(move || {
        let (mut socket, _) = listener.accept().unwrap();
        socket
            .set_read_timeout(Some(std::time::Duration::from_secs(10)))
            .unwrap();
        let mut headers = Vec::new();
        while !headers.ends_with(b"\r\n\r\n") {
            let mut byte = [0];
            socket.read_exact(&mut byte).unwrap();
            headers.push(byte[0]);
        }
        let headers = String::from_utf8(headers).unwrap();
        assert!(headers.starts_with("POST /v1/projects/fixture/tasks "));
        assert!(
            headers
                .to_lowercase()
                .contains(&format!("x-agent-id: {}", expected_agent.to_lowercase()))
        );
        let length: usize = headers
            .lines()
            .find_map(|line| {
                line.to_lowercase()
                    .strip_prefix("content-length: ")
                    .map(str::to_owned)
            })
            .unwrap()
            .parse()
            .unwrap();
        let mut body = vec![0; length];
        socket.read_exact(&mut body).unwrap();
        let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(body["labels"], serde_json::json!(["autoreview-finding"]));
        assert_eq!(body["title"], "Confirmed defect");
        let reply = serde_json::json!({"id":Uuid::new_v4(),"project_ident":"fixture","status":"todo","labels":["autoreview-finding"]}).to_string();
        write!(socket, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{reply}", reply.len()).unwrap();
    });
    let output = Command::new(env!("CARGO_BIN_EXE_amux"))
        .args([
            "--agent",
            &identity.session_id.to_string(),
            "tasks",
            "--project",
            "fixture",
            "--gateway-url",
            &url,
            "create",
            "Confirmed defect",
            "--review-finding",
            "--specification",
            "commit/file/line and evidence",
        ])
        .current_dir(dir.path())
        .env("AMUX_STATE_DIR", dir.path())
        .env("AMUX_GATEWAY_KEY", "fixture-only")
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    server.join().unwrap();
}
