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
