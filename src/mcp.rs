//! `amux mcp`: a stdio MCP server for one agent session. It exposes amux task
//! actions plus a curated set of the bundled agent-tools and memory commands.
//! Every tool runs one command and returns its output unchanged.
//!
//! The bundled `agent-tools` and `memory` executables live beside amux and are
//! always called by full path, never from PATH, so their versions match the
//! release that shipped them. Their self-updaters are disabled for the same
//! reason. `AMUX_BUNDLED_DIR` overrides the location for development and tests.

use anyhow::Result;
use serde_json::{Value, json};
use std::io::{BufRead, Write};
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::mpsc;
use std::time::Duration;

/// Keep the bundled versions pinned to the amux release that shipped them.
pub const NO_UPDATE: [(&str, &str); 2] = [
    ("AGENT_MEMORY_NO_UPDATE", "1"),
    ("AGENT_TOOLS_NO_UPDATE", "1"),
];
/// Startup memory context is skipped rather than delay the agent's start.
const CONTEXT_WAIT: Duration = Duration::from_secs(5);

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Bin {
    Amux,
    AgentTools,
    Memory,
}

impl Bin {
    pub fn file_name(self) -> String {
        let name = match self {
            Bin::Amux => "amux",
            Bin::AgentTools => "agent-tools",
            Bin::Memory => "memory",
        };
        format!("{name}{}", std::env::consts::EXE_SUFFIX)
    }

    /// Full path of this executable, if present.
    pub fn path(self) -> Option<PathBuf> {
        let exe = std::env::current_exe().ok()?;
        let path = match self {
            Bin::Amux => exe,
            _ => match std::env::var_os("AMUX_BUNDLED_DIR") {
                Some(dir) => PathBuf::from(dir),
                None => exe.parent()?.to_path_buf(),
            }
            .join(self.file_name()),
        };
        path.is_file().then_some(path)
    }
}

pub enum Kind {
    /// Positional value, passed after `--` so text starting with `-` stays text.
    Positional,
    /// Several whitespace-separated positional values.
    Words,
    /// `--flag=value`, one argument so a value starting with `-` stays a value.
    Option(&'static str),
}

pub struct Arg {
    pub name: &'static str,
    pub description: &'static str,
    pub kind: Kind,
    pub required: bool,
}

pub struct Tool {
    pub name: &'static str,
    pub description: &'static str,
    pub bin: Bin,
    pub command: &'static [&'static str],
    pub args: &'static [Arg],
}

const fn pos(name: &'static str, description: &'static str) -> Arg {
    Arg {
        name,
        description,
        kind: Kind::Positional,
        required: true,
    }
}

const fn opt(name: &'static str, flag: &'static str, description: &'static str) -> Arg {
    Arg {
        name,
        description,
        kind: Kind::Option(flag),
        required: false,
    }
}

const ID: Arg = pos("id", "Task ID");

/// The single map of exposed tools to the commands they run. It changes only
/// when a pinned agent-tools or memory version changes its command line.
pub const TOOLS: &[Tool] = &[
    Tool {
        name: "task_list",
        description: "List this project's tasks (default: todo and in_progress).",
        bin: Bin::Amux,
        command: &["tasks", "list"],
        args: &[opt(
            "status",
            "--status",
            "Comma-separated statuses: todo, in_progress, user_review, done, cancelled",
        )],
    },
    Tool {
        name: "task_get",
        description: "Show a task with its specification and comments.",
        bin: Bin::Amux,
        command: &["tasks", "get"],
        args: &[ID],
    },
    Tool {
        name: "task_create",
        description: "Create a task in this project.",
        bin: Bin::Amux,
        command: &["tasks", "create"],
        args: &[
            pos("title", "Task title"),
            opt(
                "specification",
                "--specification",
                "Context and acceptance criteria",
            ),
        ],
    },
    Tool {
        name: "task_claim",
        description: "Claim a task before working on it.",
        bin: Bin::Amux,
        command: &["tasks", "claim"],
        args: &[ID],
    },
    Tool {
        name: "task_started",
        description: "Report that work on a claimed task has started.",
        bin: Bin::Amux,
        command: &["tasks", "started"],
        args: &[ID],
    },
    Tool {
        name: "task_comment",
        description: "Add a progress, evidence or question comment to a task.",
        bin: Bin::Amux,
        command: &["tasks", "comment"],
        args: &[ID, pos("body", "Comment text")],
    },
    Tool {
        name: "task_user_review",
        description: "Park an owned task for a user decision. Comment with the question first.",
        bin: Bin::Amux,
        command: &["tasks", "user-review"],
        args: &[ID],
    },
    Tool {
        name: "task_release",
        description: "Release an owned task you cannot continue.",
        bin: Bin::Amux,
        command: &["tasks", "release"],
        args: &[ID],
    },
    Tool {
        name: "task_done",
        description: "Mark an owned task done.",
        bin: Bin::Amux,
        command: &["tasks", "done"],
        args: &[ID],
    },
    Tool {
        name: "memory_search",
        description: "Search stored memories (project and global) by meaning and keywords.",
        bin: Bin::Memory,
        command: &["search"],
        args: &[
            pos("query", "Search query"),
            opt("limit", "--limit", "Number of results (default 10)"),
        ],
    },
    Tool {
        name: "memory_store",
        description: "Store a durable memory for this project, or globally with scope global.",
        bin: Bin::Memory,
        command: &["store"],
        args: &[
            pos("content", "Memory content"),
            opt(
                "memory_type",
                "--memory-type",
                "user, feedback, project or reference (default user)",
            ),
            opt("tags", "--tags", "Comma-separated tags"),
            opt("scope", "--scope", "project (default) or global"),
        ],
    },
    Tool {
        name: "memory_get",
        description: "Fetch the full content of memories by ID.",
        bin: Bin::Memory,
        command: &["get"],
        args: &[Arg {
            name: "ids",
            description: "Space-separated memory IDs or ID prefixes",
            kind: Kind::Words,
            required: true,
        }],
    },
    Tool {
        name: "code_read",
        description: "Read a file, optionally limited to a line range.",
        bin: Bin::AgentTools,
        command: &["read"],
        args: &[
            pos("file", "File path"),
            opt(
                "lines",
                "--lines",
                "Line or range: N, START:END, START:, :END",
            ),
        ],
    },
    Tool {
        name: "code_symbols",
        description: "List the symbols in a file.",
        bin: Bin::AgentTools,
        command: &["symbols"],
        args: &[pos("file", "File path")],
    },
    Tool {
        name: "code_symbol",
        description: "Extract one symbol's source by name.",
        bin: Bin::AgentTools,
        command: &["symbol"],
        args: &[
            pos("name", "Symbol name"),
            opt(
                "file",
                "--file",
                "File to search (default: the project index)",
            ),
        ],
    },
    Tool {
        name: "code_search",
        description: "Search the project index for symbols, files or knowledge concepts.",
        bin: Bin::AgentTools,
        command: &["search"],
        args: &[
            pos("query", "Search query"),
            opt("type", "--type", "symbol (default), file, knowledge or all"),
            opt("limit", "--limit", "Maximum results (default 20)"),
        ],
    },
    Tool {
        name: "doc_outline",
        description: "Show a Markdown file's heading outline.",
        bin: Bin::AgentTools,
        command: &["doc", "outline"],
        args: &[pos("file", "Markdown file")],
    },
    Tool {
        name: "doc_section",
        description: "Return one section of a Markdown file by heading text.",
        bin: Bin::AgentTools,
        command: &["doc", "section"],
        args: &[pos("file", "Markdown file"), pos("section", "Heading text")],
    },
    Tool {
        name: "knowledge_get",
        description: "Show a knowledge-graph concept as a compact card.",
        bin: Bin::AgentTools,
        command: &["get"],
        args: &[pos("resource", "Resource URI, title or identifier")],
    },
    Tool {
        name: "knowledge_graph",
        description: "Traverse the project knowledge graph from a resource.",
        bin: Bin::AgentTools,
        command: &["graph"],
        args: &[
            pos("resource", "Resource URI, title or symbol"),
            opt(
                "relation",
                "--relation",
                "Relationship to follow: calls, imports, inherits, implements, ...",
            ),
            opt("depth", "--depth", "Maximum depth (default 1)"),
        ],
    },
];

/// Tools whose executable is present.
pub fn available() -> Vec<&'static Tool> {
    TOOLS.iter().filter(|t| t.bin.path().is_some()).collect()
}

fn schema(tool: &Tool) -> Value {
    let properties: serde_json::Map<String, Value> = tool
        .args
        .iter()
        .map(|a| {
            (
                a.name.to_owned(),
                json!({"type": "string", "description": a.description}),
            )
        })
        .collect();
    let required: Vec<&str> = tool
        .args
        .iter()
        .filter(|a| a.required)
        .map(|a| a.name)
        .collect();
    json!({"type": "object", "properties": properties, "required": required})
}

/// Command-line arguments for one call, from its JSON arguments.
pub fn argv(tool: &Tool, arguments: &Value) -> Result<Vec<String>, String> {
    let mut options = Vec::new();
    let mut positionals = Vec::new();
    for arg in tool.args {
        let value = match arguments.get(arg.name) {
            None | Some(Value::Null) => None,
            Some(Value::String(s)) => Some(s.clone()),
            Some(other) => Some(other.to_string()),
        };
        let Some(value) = value.filter(|v| !v.trim().is_empty()) else {
            if arg.required {
                return Err(format!("missing required argument: {}", arg.name));
            }
            continue;
        };
        match arg.kind {
            Kind::Positional => positionals.push(value),
            Kind::Words => positionals.extend(value.split_whitespace().map(str::to_owned)),
            Kind::Option(flag) => options.push(format!("{flag}={value}")),
        }
    }
    let mut argv: Vec<String> = tool.command.iter().map(|s| (*s).to_owned()).collect();
    argv.extend(options);
    argv.push("--".into());
    argv.extend(positionals);
    Ok(argv)
}

fn directory() -> PathBuf {
    std::env::var_os("AMUX_AGENT_PROJECT")
        .map(PathBuf::from)
        .filter(|p| p.is_dir())
        .or_else(|| std::env::current_dir().ok())
        .unwrap_or_else(|| PathBuf::from("."))
}

/// Run one command and return (output, is_error).
fn run(bin: Bin, argv: &[String], directory: &Path) -> (String, bool) {
    let Some(path) = bin.path() else {
        return (
            format!("{} is not bundled with this amux", bin.file_name()),
            true,
        );
    };
    let output = Command::new(path)
        .args(argv)
        .current_dir(directory)
        .envs(NO_UPDATE)
        .stdin(Stdio::null())
        .output();
    match output {
        Ok(out) => {
            let mut text = String::from_utf8_lossy(&out.stdout).into_owned();
            if !out.status.success() {
                text.push_str(&String::from_utf8_lossy(&out.stderr));
            }
            (text, !out.status.success())
        }
        Err(error) => (format!("could not run {}: {error}", bin.file_name()), true),
    }
}

fn call(params: &Value) -> Value {
    let name = params["name"].as_str().unwrap_or_default();
    let (text, error) = match available().into_iter().find(|t| t.name == name) {
        None => (format!("unknown tool: {name}"), true),
        Some(tool) => match argv(tool, &params["arguments"]) {
            Ok(argv) => run(tool.bin, &argv, &directory()),
            Err(message) => (message, true),
        },
    };
    json!({"content": [{"type": "text", "text": text}], "isError": error})
}

/// True when the provider already injects memory each turn through
/// `memory setup hooks`, so startup context would only duplicate it.
pub fn memory_hook_installed(provider: &str) -> bool {
    let Ok(environment) = crate::harness::Environment::discover() else {
        return false;
    };
    let file = match provider {
        "claude" => environment.claude_home.join("settings.json"),
        "codex" => environment.codex_home.join("config.toml"),
        _ => return false,
    };
    std::fs::read_to_string(file)
        .map(|text| text.contains("memory hook --agent") || text.contains("memory-inject"))
        .unwrap_or(false)
}

/// Project memory for the start of a session, or None when memory is not
/// bundled, the provider's own memory hook already supplies it, or it is slow.
pub fn startup_context(provider: &str, project: &Path) -> Option<String> {
    Bin::Memory.path()?;
    if memory_hook_installed(provider) {
        return None;
    }
    let name = project.file_name()?.to_string_lossy().into_owned();
    let project = project.to_path_buf();
    let (sender, receiver) = mpsc::channel();
    std::thread::spawn(move || {
        let _ = sender.send(run(
            Bin::Memory,
            &[
                "context".into(),
                "-k".into(),
                "5".into(),
                "--".into(),
                format!("{name} project: current work, decisions and conventions"),
            ],
            &project,
        ));
    });
    match receiver.recv_timeout(CONTEXT_WAIT) {
        Ok((text, false)) if !text.trim().is_empty() => Some(format!(
            "Relevant project memory from amux (the amux memory_search and memory_get tools fetch more):\n{text}"
        )),
        _ => None,
    }
}

/// True when the user's Codex config sets its own developer instructions,
/// which a launch override would replace.
fn codex_sets_developer_instructions() -> bool {
    crate::harness::Environment::discover()
        .ok()
        .and_then(|e| std::fs::read_to_string(e.codex_home.join("config.toml")).ok())
        .and_then(|text| text.parse::<toml_edit::DocumentMut>().ok())
        .is_some_and(|config| config.contains_key("developer_instructions"))
}

fn respond(id: &Value, message: Value) -> Value {
    let mut response = json!({"jsonrpc": "2.0", "id": id});
    let key = if message.get("code").is_some() {
        "error"
    } else {
        "result"
    };
    response[key] = message;
    response
}

/// Handle one JSON-RPC message; notifications get no response.
fn handle(message: &Value) -> Option<Value> {
    let id = message.get("id")?;
    let params = &message["params"];
    let result = match message["method"].as_str().unwrap_or_default() {
        "initialize" => {
            let mut result = json!({
                "protocolVersion": params["protocolVersion"].as_str().unwrap_or("2025-06-18"),
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "amux", "version": env!("CARGO_PKG_VERSION")},
            });
            // Codex ignores server instructions; it gets this context at launch.
            let provider = std::env::var("AMUX_AGENT_PROVIDER").unwrap_or_default();
            if provider != "codex"
                && let Some(context) = startup_context(&provider, &directory())
            {
                result["instructions"] = context.into();
            }
            result
        }
        "ping" => json!({}),
        "tools/list" => json!({"tools": available().iter().map(|t| json!({
            "name": t.name,
            "description": t.description,
            "inputSchema": schema(t),
        })).collect::<Vec<_>>()}),
        "tools/call" => call(params),
        method => json!({"code": -32601, "message": format!("method not found: {method}")}),
    };
    Some(respond(id, result))
}

/// Serve newline-delimited JSON-RPC on stdin/stdout until stdin closes.
pub fn serve() -> Result<()> {
    let stdout = std::io::stdout();
    for line in std::io::stdin().lock().lines() {
        let line = line?;
        if line.trim().is_empty() {
            continue;
        }
        let response = match serde_json::from_str::<Value>(&line) {
            Ok(message) => handle(&message),
            Err(error) => Some(json!({"jsonrpc": "2.0", "id": null,
                "error": {"code": -32700, "message": error.to_string()}})),
        };
        if let Some(response) = response {
            let mut out = stdout.lock();
            writeln!(out, "{response}")?;
            out.flush()?;
        }
    }
    Ok(())
}

/// Provider arguments that register `amux mcp` for one launched session,
/// passing that session's identity explicitly (Codex does not forward the
/// parent environment to MCP servers).
pub fn launch_arguments(
    provider: crate::identity::Provider,
    amux: &Path,
    environment: &std::collections::HashMap<String, String>,
) -> Vec<String> {
    let mut env: std::collections::BTreeMap<&str, &str> = environment
        .iter()
        .map(|(k, v)| (k.as_str(), v.as_str()))
        .collect();
    let bundled = std::env::var("AMUX_BUNDLED_DIR").unwrap_or_default();
    if !bundled.is_empty() {
        env.insert("AMUX_BUNDLED_DIR", &bundled);
    }
    // Codex forwards only allowlisted variables to MCP servers, so pass the
    // resolved state directory: task commands read gateway preferences there.
    let state = crate::identity::state_directory()
        .map(|p| p.to_string_lossy().into_owned())
        .unwrap_or_default();
    if !state.is_empty() {
        env.insert("AMUX_STATE_DIR", &state);
    }
    let command = amux.to_string_lossy();
    match provider {
        crate::identity::Provider::Claude => vec![
            "--mcp-config".into(),
            json!({"mcpServers": {"amux": {"command": command, "args": ["mcp"], "env": env}}})
                .to_string(),
        ],
        crate::identity::Provider::Codex => {
            let mut table = toml_edit::InlineTable::new();
            for (k, v) in &env {
                table.insert(*k, (*v).into());
            }
            vec![
                "-c".into(),
                format!(
                    "mcp_servers.amux.command={}",
                    toml_edit::Value::from(command.as_ref())
                ),
                "-c".into(),
                "mcp_servers.amux.args=[\"mcp\"]".into(),
                "-c".into(),
                format!("mcp_servers.amux.env={table}"),
            ]
        }
    }
}

/// Codex ignores MCP server instructions, so its startup memory goes in as
/// launch-time developer instructions, unless the user configures their own.
pub fn codex_context_arguments(project: &Path) -> Vec<String> {
    if codex_sets_developer_instructions() {
        return Vec::new();
    }
    match startup_context("codex", project) {
        Some(context) => vec![
            "-c".into(),
            format!(
                "developer_instructions={}",
                toml_edit::Value::from(context.as_str())
            ),
        ],
        None => Vec::new(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn positionals_follow_options_after_a_separator() {
        let tool = TOOLS.iter().find(|t| t.name == "task_comment").unwrap();
        assert_eq!(
            argv(tool, &json!({"id": "1", "body": "- starts with a dash"})).unwrap(),
            ["tasks", "comment", "--", "1", "- starts with a dash"]
        );
        let tool = TOOLS.iter().find(|t| t.name == "code_read").unwrap();
        assert_eq!(
            argv(tool, &json!({"file": "a.rs", "lines": 3})).unwrap(),
            ["read", "--lines=3", "--", "a.rs"]
        );
        assert!(argv(tool, &json!({})).is_err());
        let tool = TOOLS.iter().find(|t| t.name == "task_create").unwrap();
        assert_eq!(
            argv(tool, &json!({"title": "T", "specification": "- item"})).unwrap(),
            ["tasks", "create", "--specification=- item", "--", "T"]
        );
        let tool = TOOLS.iter().find(|t| t.name == "memory_get").unwrap();
        assert_eq!(
            argv(tool, &json!({"ids": "ab12 cd34"})).unwrap(),
            ["get", "--", "ab12", "cd34"]
        );
    }

    #[test]
    fn tool_names_are_unique() {
        let mut names: Vec<_> = TOOLS.iter().map(|t| t.name).collect();
        names.sort_unstable();
        names.dedup();
        assert_eq!(names.len(), TOOLS.len());
    }

    #[test]
    fn launch_arguments_carry_the_session_identity() {
        let env = std::collections::HashMap::from([(
            "AMUX_AGENT_NAME".to_owned(),
            "A \"quoted\" name".to_owned(),
        )]);
        let claude = launch_arguments(
            crate::identity::Provider::Claude,
            Path::new("/opt/amux/amux"),
            &env,
        );
        let config: Value = serde_json::from_str(&claude[1]).unwrap();
        assert_eq!(config["mcpServers"]["amux"]["command"], "/opt/amux/amux");
        assert_eq!(
            config["mcpServers"]["amux"]["env"]["AMUX_STATE_DIR"],
            crate::identity::state_directory()
                .unwrap()
                .to_string_lossy()
                .as_ref()
        );
        assert_eq!(
            config["mcpServers"]["amux"]["env"]["AMUX_AGENT_NAME"],
            "A \"quoted\" name"
        );
        let codex = launch_arguments(
            crate::identity::Provider::Codex,
            Path::new("C:\\amux\\amux.exe"),
            &env,
        );
        for value in codex.iter().filter(|a| *a != "-c") {
            let (key, value) = value.split_once('=').unwrap();
            let parsed: toml_edit::DocumentMut = format!("v = {value}").parse().unwrap();
            assert!(key.starts_with("mcp_servers.amux."), "{key}");
            if key.ends_with("command") {
                assert_eq!(parsed["v"].as_str(), Some("C:\\amux\\amux.exe"));
            }
            if key.ends_with("env") {
                assert_eq!(
                    parsed["v"]["AMUX_AGENT_NAME"].as_str(),
                    Some("A \"quoted\" name")
                );
            }
        }
    }
}
