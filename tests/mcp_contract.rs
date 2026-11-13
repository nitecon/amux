//! `amux mcp` against the bundled agent-tools and memory binaries.
//! Fetch them with `python scripts/fetch_bundled_tools.py --target <target>
//! --output target/bundled` and set AMUX_BUNDLED_DIR=target/bundled. CI always
//! does; locally these tests are skipped without it.

use amux::mcp::{Bin, Kind, TOOLS};
use serde_json::{Value, json};
use std::io::{BufRead, BufReader, Write};
use std::path::PathBuf;
use std::process::{Command, Stdio};

fn bundled() -> Option<PathBuf> {
    match std::env::var_os("AMUX_BUNDLED_DIR") {
        Some(dir) => Some(std::fs::canonicalize(dir).expect("AMUX_BUNDLED_DIR exists")),
        None => {
            assert!(
                std::env::var_os("CI").is_none(),
                "CI must set AMUX_BUNDLED_DIR"
            );
            eprintln!("skipped: AMUX_BUNDLED_DIR is not set");
            None
        }
    }
}

fn executable(dir: &std::path::Path, bin: Bin) -> PathBuf {
    match bin {
        Bin::Amux => PathBuf::from(env!("CARGO_BIN_EXE_amux")),
        _ => dir.join(bin.file_name()),
    }
}

#[test]
fn large_submit_work_lists_reach_the_cli_without_os_argument_overflow() {
    let root = tempfile::tempdir().unwrap();
    let mut child = Command::new(env!("CARGO_BIN_EXE_amux"))
        .arg("mcp")
        .env("AMUX_STATE_DIR", root.path().join("state"))
        .env("AMUX_AGENT_PROJECT", root.path())
        .env_remove("AMUX_AGENT_PROVIDER")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .spawn()
        .unwrap();
    let paths: Vec<_> = (0..16673)
        .map(|i| format!("assets/actor_{i:05}_{}.scene", "a".repeat(80)))
        .collect();
    let request = json!({"jsonrpc":"2.0", "id":1, "method":"tools/call",
    "params":{"name":"submit_work", "arguments":{
        "task_id":"not-a-uuid", "message":"Move assets", "paths":paths,
        "resume_commit":"abc123"
    }}});
    writeln!(child.stdin.take().unwrap(), "{request}").unwrap();
    let output = child.wait_with_output().unwrap();
    assert!(output.status.success());
    let reply: Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(reply["result"]["isError"], true);
    let text = reply["result"]["content"][0]["text"].as_str().unwrap();
    // Deliberately invalid task: the actual CLI must launch and validate it,
    // before any ownership/network/Git action. Old argv expansion cannot launch.
    assert!(text.contains("invalid value 'not-a-uuid'"), "{text}");
    assert!(!text.contains("could not run"), "{text}");
}

#[test]
fn every_tool_command_and_option_exists_in_the_bundled_version() {
    let Some(dir) = bundled() else { return };
    for tool in TOOLS {
        let output = Command::new(executable(&dir, tool.bin))
            .args(tool.command)
            .arg("--help")
            .envs(amux::mcp::NO_UPDATE)
            .output()
            .unwrap();
        let help = String::from_utf8_lossy(&output.stdout);
        assert!(output.status.success(), "{}: {help}", tool.name);
        for arg in tool.args {
            if let Kind::Option(flag) = arg.kind {
                assert!(
                    help.split(|c: char| c.is_whitespace() || c == ',')
                        .any(|word| word == flag),
                    "{}: {flag} missing from `{}` help",
                    tool.name,
                    tool.command.join(" ")
                );
            }
        }
    }
}

#[test]
fn the_server_lists_and_runs_bundled_tools_over_stdio() {
    let Some(dir) = bundled() else { return };
    let mut child = Command::new(env!("CARGO_BIN_EXE_amux"))
        .arg("mcp")
        .env("AMUX_BUNDLED_DIR", &dir)
        .env("AMUX_AGENT_PROJECT", env!("CARGO_MANIFEST_DIR"))
        // No provider: the startup-context hook check never matches.
        .env_remove("AMUX_AGENT_PROVIDER")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .spawn()
        .unwrap();
    let mut input = child.stdin.take().unwrap();
    let mut output = BufReader::new(child.stdout.take().unwrap());
    let mut request = |message: Value| -> Value {
        writeln!(input, "{message}").unwrap();
        let mut line = String::new();
        output.read_line(&mut line).unwrap();
        serde_json::from_str(&line).unwrap()
    };
    let init = request(json!({"jsonrpc": "2.0", "id": 1, "method": "initialize",
        "params": {"protocolVersion": "2025-06-18"}}));
    assert_eq!(init["result"]["serverInfo"]["name"], "amux");
    let listed = request(json!({"jsonrpc": "2.0", "id": 2, "method": "tools/list"}));
    let names: Vec<&str> = listed["result"]["tools"]
        .as_array()
        .unwrap()
        .iter()
        .map(|t| t["name"].as_str().unwrap())
        .collect();
    assert_eq!(names, TOOLS.iter().map(|t| t.name).collect::<Vec<_>>());
    let read = request(json!({"jsonrpc": "2.0", "id": 3, "method": "tools/call",
        "params": {"name": "code_read", "arguments": {"file": "Cargo.toml", "lines": "1:2"}}}));
    assert_eq!(read["result"]["isError"], false, "{read}");
    assert!(
        read["result"]["content"][0]["text"]
            .as_str()
            .unwrap()
            .contains("name = \"amux\""),
        "{read}"
    );
    let missing = request(json!({"jsonrpc": "2.0", "id": 4, "method": "tools/call",
        "params": {"name": "code_read", "arguments": {}}}));
    assert_eq!(missing["result"]["isError"], true);
    drop(input);
    assert!(child.wait().unwrap().success());
}

fn isolated_memory(dir: &std::path::Path) -> (tempfile::TempDir, rusqlite::Connection) {
    let root = tempfile::tempdir().unwrap();
    let memory_dir = root.path().join("memory");
    let initialized = Command::new(executable(dir, Bin::Memory))
        .args(["list", "--limit", "1"])
        .env("AGENT_MEMORY_DIR", &memory_dir)
        .env("AGENT_MEMORY_GATEWAY_AUTO_SYNC", "false")
        .envs(amux::mcp::NO_UPDATE)
        .output()
        .unwrap();
    assert!(
        initialized.status.success(),
        "{}",
        String::from_utf8_lossy(&initialized.stderr)
    );
    let conn = rusqlite::Connection::open(memory_dir.join("memory.db")).unwrap();
    (root, conn)
}

fn retained_tombstone(conn: &rusqlite::Connection, id: &str, project: &str) {
    conn.execute(
        "INSERT INTO memories (id, content, content_raw, tags, project,
             created_at, updated_at, memory_type)
         VALUES (?1, 'retained lesson', 'original lesson', '[\"undo\"]',
             ?2, '2026-01-01', '2026-01-01', 'project')",
        [id, project],
    )
    .unwrap();
    conn.execute(
        "INSERT INTO memory_gateway_sync (local_memory_id, project,
             gateway_memory_id, last_seen_server_revision, sync_state,
             tombstone_deleted, created_at, updated_at)
         VALUES (?1, ?2, ?1, 10, 'tombstone', 1,
             '2026-01-01', '2026-01-01')",
        [id, project],
    )
    .unwrap();
}

#[test]
fn memory_update_reports_tombstone_recovery_without_changing_retained_content() {
    let Some(dir) = bundled() else { return };
    let (root, conn) = isolated_memory(&dir);
    let memory_dir = root.path().join("memory");
    let id = "aaaaaaaa-0000-1111-2222-000000000901";
    retained_tombstone(&conn, id, "amux");

    let mut child = Command::new(env!("CARGO_BIN_EXE_amux"))
        .arg("mcp")
        .env("AMUX_BUNDLED_DIR", &dir)
        .env("AMUX_AGENT_PROJECT", root.path())
        .env("AGENT_MEMORY_DIR", &memory_dir)
        .env("AGENT_MEMORY_GATEWAY_AUTO_SYNC", "false")
        .env_remove("AMUX_AGENT_PROVIDER")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .spawn()
        .unwrap();
    let request = json!({"jsonrpc":"2.0", "id":1, "method":"tools/call",
    "params":{"name":"memory_update", "arguments":{
        "id":id, "content":"replacement lesson", "tags":"transactions"
    }}});
    writeln!(child.stdin.take().unwrap(), "{request}").unwrap();
    let output = child.wait_with_output().unwrap();
    assert!(output.status.success());
    let reply: Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(reply["result"]["isError"], true, "{reply}");
    let text = reply["result"]["content"][0]["text"].as_str().unwrap();
    assert!(text.contains("tombstoned gateway identity"), "{text}");
    assert!(text.contains("memory copy --id"), "{text}");
    assert!(!text.contains("status=\"updated\""), "{text}");
    let retained: (String, String, String, String, bool) = conn
        .query_row(
            "SELECT m.content, m.content_raw, m.tags, m.updated_at,
                 s.tombstone_deleted FROM memories m
             JOIN memory_gateway_sync s ON s.local_memory_id = m.id WHERE m.id = ?1",
            [id],
            |row| {
                Ok((
                    row.get(0)?,
                    row.get(1)?,
                    row.get(2)?,
                    row.get(3)?,
                    row.get(4)?,
                ))
            },
        )
        .unwrap();
    assert_eq!(
        retained,
        (
            "retained lesson".into(),
            "original lesson".into(),
            "[\"undo\"]".into(),
            "2026-01-01".into(),
            true
        )
    );
}

#[test]
fn memory_push_all_continues_past_retained_gateway_deletions() {
    let Some(dir) = bundled() else { return };
    let (root, conn) = isolated_memory(&dir);
    retained_tombstone(&conn, "aaaaaaaa-0000-1111-2222-000000000901", "a-project");
    retained_tombstone(&conn, "aaaaaaaa-0000-1111-2222-000000000902", "z-project");
    let output = Command::new(executable(&dir, Bin::Memory))
        .args(["push", "--all"])
        .current_dir(root.path())
        .env("AGENT_MEMORY_DIR", root.path().join("memory"))
        .env("AGENT_MEMORY_GATEWAY_URL", "http://127.0.0.1:9")
        .env("AGENT_MEMORY_GATEWAY_API_KEY", "isolated-test-key")
        .env("AGENT_MEMORY_GATEWAY_AUTO_SYNC", "false")
        .envs(amux::mcp::NO_UPDATE)
        .output()
        .unwrap();
    let text = String::from_utf8_lossy(&output.stdout);
    assert!(
        output.status.success(),
        "{text}\n{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(text.contains("push_all_complete"), "{text}");
    assert!(text.contains("projects=\"2\""), "{text}");
    assert!(text.contains("blocked=\"2\""), "{text}");
    assert!(text.contains("rejected=\"0\""), "{text}");
    assert!(!text.contains("memory copy --id"), "{text}");
    let retained: i64 = conn
        .query_row(
            "SELECT COUNT(*) FROM memory_gateway_sync WHERE tombstone_deleted = 1",
            [],
            |row| row.get(0),
        )
        .unwrap();
    assert_eq!(retained, 2);
}

#[test]
fn transition_confirmation_keeps_null_distinct_from_omission() {
    let tool = TOOLS.iter().find(|tool| tool.name == "task_done").unwrap();
    let mut arguments = serde_json::json!({"id":"task"});
    assert!(
        !amux::mcp::argv(tool, &arguments)
            .unwrap()
            .iter()
            .any(|arg| arg.starts_with("--changeset="))
    );
    for value in [
        serde_json::Value::Null,
        serde_json::json!(["docs/two words.md"]),
    ] {
        arguments["changeset"] = value.clone();
        assert!(
            amux::mcp::argv(tool, &arguments)
                .unwrap()
                .contains(&format!("--changeset={value}"))
        );
    }
    arguments["changeset"] = serde_json::json!([]);
    assert!(amux::mcp::argv(tool, &arguments).is_err());
    assert!(!TOOLS.iter().any(|tool| tool.name == "task_set_type"));
}

#[test]
fn guided_confirmation_tools_preserve_required_json_and_file_names() {
    let confirm = TOOLS
        .iter()
        .find(|t| t.name == "task_confirm_mutation")
        .unwrap();
    assert_eq!(
        amux::mcp::argv(confirm, &serde_json::json!({"id":"task"})).unwrap(),
        ["set-task", "--", "task"]
    );
    let changeset = TOOLS
        .iter()
        .find(|t| t.name == "task_set_changeset")
        .unwrap();
    assert!(amux::mcp::argv(changeset, &serde_json::json!({"id":"task"})).is_err());
    for value in [
        serde_json::Value::Null,
        serde_json::json!(["docs/two words.md"]),
    ] {
        assert_eq!(
            amux::mcp::argv(
                changeset,
                &serde_json::json!({"id":"task","changeset":value})
            )
            .unwrap(),
            [
                "set-changeset".to_owned(),
                "--".into(),
                "task".into(),
                value.to_string()
            ]
        );
    }
    assert!(
        amux::mcp::argv(
            changeset,
            &serde_json::json!({"id":"task","changeset":true})
        )
        .is_err()
    );
}
