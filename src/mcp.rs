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
use std::time::Duration;

/// Keep the bundled versions pinned to the amux release that shipped them.
pub const NO_UPDATE: [(&str, &str); 2] = [
    ("AGENT_MEMORY_NO_UPDATE", "1"),
    ("AGENT_TOOLS_NO_UPDATE", "1"),
];
/// Session settings authorize only amux MCP tools and disable attribution.
pub const CLAUDE_SESSION_SETTINGS: &str = r#"{"attribution":{"commit":"","pr":""},"includeCoAuthoredBy":false,"permissions":{"allow":["mcp__amux__*"]}}"#;
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
    /// A JSON array of strings, each passed as its own positional value, so
    /// values may contain whitespace.
    List,
    /// Explicit array/null confirmation, preserving null instead of omitting it.
    Changeset,
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

const CHANGESET: Arg = Arg {
    name: "changeset",
    description: "Every file mutated by this task (modified, added or deleted), or null if no files were mutated. Omit to receive the server confirmation question without moving the task.",
    kind: Kind::Changeset,
    required: false,
};

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
        description: "Create a subtask under a fitting existing parent; read the parent first and check scope. Create a root (root_reason instead of parent) only for an explicit user request (user-directed) or a core-principle finding (principle), with a specification recording the request or finding, evidence and acceptance criteria. Otherwise, if no parent fits, ask the user to create the top-level task and propose its title and description.",
        bin: Bin::Amux,
        command: &["tasks", "create"],
        args: &[
            pos("title", "Task title"),
            opt(
                "parent",
                "--parent",
                "Existing parent task UUID whose scope includes this work",
            ),
            opt(
                "root_reason",
                "--root-reason",
                "Create a root instead of a subtask: user-directed or principle. Requires specification; not with parent",
            ),
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
        name: "task_delegate",
        description: "Delegate cross-project implementation under a fitting source parent you have read. Keep changes in the owning repository; inspect both projects before retrying an uncertain delegation.",
        bin: Bin::Amux,
        command: &["tasks", "delegate"],
        args: &[
            pos("title", "Handoff title"),
            Arg {
                name: "parent",
                description: "Source parent whose scope includes this work",
                kind: Kind::Option("--parent"),
                required: true,
            },
            Arg {
                name: "target_project",
                description: "Owning project",
                kind: Kind::Option("--target-project"),
                required: true,
            },
            opt(
                "specification",
                "--specification",
                "Context and acceptance criteria",
            ),
        ],
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
        description: "Park an owned task for a user decision. Supply the specific action the user must take.",
        bin: Bin::Amux,
        command: &["tasks", "user-review"],
        args: &[
            ID,
            pos(
                "action",
                "Specific decision or action required from the user",
            ),
            CHANGESET,
        ],
    },
    Tool {
        name: "task_release",
        description: "Release an owned task you cannot continue.",
        bin: Bin::Amux,
        command: &["tasks", "release"],
        args: &[ID, CHANGESET],
    },
    Tool {
        name: "task_done",
        description: "Mark an owned task done.",
        bin: Bin::Amux,
        command: &["tasks", "done"],
        args: &[ID, CHANGESET],
    },
    Tool {
        name: "submit_work",
        description: "Finish code work on an owned task: commit exactly the listed changed files (and push, or open a pull request, as set in amux Settings), then post the result on the task. To publish an already-created task commit after recovery, supply resume_commit and its exact file list instead of committing files again. Never commit directly or add attribution lines.",
        bin: Bin::Amux,
        command: &["submit-work"],
        args: &[
            pos("task_id", "Owned task ID"),
            pos(
                "message",
                "Commit message: a subject line, then an optional body",
            ),
            Arg {
                name: "paths",
                description: "Changed files to submit (modified, added or deleted), relative to the project or absolute",
                kind: Kind::List,
                required: true,
            },
            opt(
                "resume_commit",
                "--resume-commit",
                "Existing HEAD task commit to publish after recovery; its original message is retained",
            ),
        ],
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
        name: "memory_update",
        description: "Update an existing durable memory by ID after inspecting it; preserve unrelated guidance.",
        bin: Bin::Memory,
        command: &["update"],
        args: &[
            pos("id", "Existing memory ID or ID prefix"),
            Arg {
                name: "content",
                description: "Replacement memory content",
                kind: Kind::Option("--content"),
                required: true,
            },
            opt(
                "tags",
                "--tags",
                "Optional replacement comma-separated tags",
            ),
        ],
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
            let mut schema = match a.kind {
                Kind::List => json!({"type": "array", "items": {"type": "string"}}),
                Kind::Changeset => {
                    json!({"type": ["array", "null"], "items": {"type": "string"}, "minItems": 1})
                }
                _ => json!({"type": "string"}),
            };
            schema["description"] = a.description.into();
            (a.name.to_owned(), schema)
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
        if matches!(arg.kind, Kind::Changeset) {
            if let Some(value) = arguments.get(arg.name) {
                amux_gateway_backend::task_changesets::validate(value)
                    .map_err(|e| e.to_string())?;
                options.push(format!("--changeset={value}"));
            }
            continue;
        }
        if matches!(arg.kind, Kind::List) {
            let values = list(arguments.get(arg.name), arg.name)?;
            if values.is_empty() && arg.required {
                return Err(format!("missing required argument: {}", arg.name));
            }
            positionals.extend(values);
            continue;
        }
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
            Kind::List | Kind::Changeset => unreachable!("handled above"),
        }
    }
    let mut argv: Vec<String> = tool.command.iter().map(|s| (*s).to_owned()).collect();
    argv.extend(options);
    argv.push("--".into());
    argv.extend(positionals);
    Ok(argv)
}

/// The non-empty strings of an array argument; absent means none.
fn list(value: Option<&Value>, name: &str) -> Result<Vec<String>, String> {
    let items = match value {
        None | Some(Value::Null) => return Ok(Vec::new()),
        Some(Value::Array(items)) => items,
        Some(_) => return Err(format!("{name} must be an array of strings")),
    };
    items
        .iter()
        .filter(|item| item.as_str().is_none_or(|s| !s.trim().is_empty()))
        .map(|item| {
            item.as_str()
                .map(str::to_owned)
                .ok_or_else(|| format!("{name} must be an array of strings"))
        })
        .collect()
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
        Some(tool) => match run_tool(tool, &params["arguments"], &directory()) {
            Ok(output) => output,
            Err(message) => (message, true),
        },
    };
    json!({"content": [{"type": "text", "text": text}], "isError": error})
}

/// Large submit-work lists travel in a file, never in the child command line.
fn run_tool(tool: &Tool, arguments: &Value, directory: &Path) -> Result<(String, bool), String> {
    let mut argv = argv(tool, arguments)?;
    let mut paths_file = None;
    if tool.name == "submit_work" {
        // Optional flags precede `-- task_id message paths...`.
        let start = argv
            .iter()
            .position(|arg| arg == "--")
            .expect("argv has a positional separator")
            + 3;
        let paths = argv.split_off(start);
        let mut file = tempfile::NamedTempFile::new().map_err(|e| e.to_string())?;
        serde_json::to_writer(&mut file, &paths).map_err(|e| e.to_string())?;
        file.flush().map_err(|e| e.to_string())?;
        let path = file
            .path()
            .to_str()
            .ok_or("path-list filename is not UTF-8")?;
        argv.insert(1, format!("--paths-file={path}"));
        paths_file = Some(file);
    }
    let output = run(tool.bin, &argv, directory);
    // Keep the path list available until the child has exited on every OS.
    drop(paths_file);
    Ok(output)
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

/// Durable search deliberately bypasses `context`, which reads the shared
/// WorkingContext. Bound execution and output; slow recall never blocks work.
pub fn durable_memories(project: &Path, query: &str) -> Option<String> {
    use std::io::{Read, Seek};
    let path = Bin::Memory.path()?;
    let mut output = tempfile::tempfile().ok()?;
    let mut child = Command::new(path)
        .args(["search", "--limit=5", "--", query])
        .current_dir(project)
        .envs(NO_UPDATE)
        .stdin(Stdio::null())
        .stdout(output.try_clone().ok()?)
        .stderr(Stdio::null())
        .spawn()
        .ok()?;
    let started = std::time::Instant::now();
    loop {
        match child.try_wait() {
            Ok(Some(status)) if status.success() => break,
            Ok(Some(_)) => return None,
            Ok(None) if started.elapsed() < CONTEXT_WAIT => {
                std::thread::sleep(Duration::from_millis(20))
            }
            _ => {
                let _ = child.kill();
                let _ = child.wait();
                return None;
            }
        }
    }
    output.rewind().ok()?;
    let mut bytes = Vec::new();
    output.take(10_000).read_to_end(&mut bytes).ok()?;
    let text = String::from_utf8_lossy(&bytes).into_owned();
    (!text.trim().is_empty()).then_some(text)
}

pub fn startup_context(_provider: &str, project: &Path) -> Option<String> {
    let hints = crate::guidance::session_hints(true);
    let query = format!(
        "{} project decisions and conventions",
        project.file_name()?.to_string_lossy()
    );
    let memories = durable_memories(project, &query).unwrap_or_default();
    Some(format!("{hints}\n{memories}"))
}

/// True when any Codex config that may apply sets developer instructions,
/// which a launch override would replace: the user config (any profile) or a
/// project `.codex/config.toml` in the project folder or a parent.
fn codex_sets_developer_instructions(project: &Path) -> bool {
    let user = crate::harness::Environment::discover()
        .ok()
        .map(|e| e.codex_home.join("config.toml"));
    user.into_iter()
        .chain(
            project
                .ancestors()
                .map(|d| d.join(".codex").join("config.toml")),
        )
        .filter_map(|path| std::fs::read_to_string(path).ok())
        .any(|text| text.contains("developer_instructions"))
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
                && std::env::var_os("AMUX_DESKTOP_GUIDANCE").is_none()
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
/// parent environment to MCP servers). Approval is scoped to amux tools;
/// provider deny rules and managed restrictions still apply.
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
    let desktop_guidance = std::env::var("AMUX_DESKTOP_GUIDANCE").unwrap_or_default();
    if !desktop_guidance.is_empty() {
        env.insert("AMUX_DESKTOP_GUIDANCE", &desktop_guidance);
    }
    match provider {
        // Work is committed by `amux submit-work`, which never adds attribution;
        // these settings stop Claude Code adding its own to commits and PRs
        // (`attribution` is current, `includeCoAuthoredBy` the legacy key).
        // --mcp-config takes several values, so it stays last.
        crate::identity::Provider::Claude => vec![
            "--settings".into(),
            CLAUDE_SESSION_SETTINGS.into(),
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
                "mcp_servers.amux.default_tools_approval_mode=\"approve\"".into(),
                "-c".into(),
                format!("mcp_servers.amux.env={table}"),
            ]
        }
    }
}

/// Codex ignores MCP server instructions, so its startup memory goes in as
/// launch-time developer instructions, unless the user configures their own.
pub fn codex_context_arguments(project: &Path) -> Vec<String> {
    if codex_sets_developer_instructions(project) {
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
            argv(
                tool,
                &json!({"title": "T", "parent":"P", "specification": "- item"})
            )
            .unwrap(),
            [
                "tasks",
                "create",
                "--parent=P",
                "--specification=- item",
                "--",
                "T"
            ]
        );
        assert_eq!(
            argv(
                tool,
                &json!({"title": "T", "root_reason": "user-directed", "specification": "Asked"})
            )
            .unwrap(),
            [
                "tasks",
                "create",
                "--root-reason=user-directed",
                "--specification=Asked",
                "--",
                "T"
            ]
        );
        assert!(argv(tool, &json!({"parent": "P"})).is_err());
        let tool = TOOLS.iter().find(|t| t.name == "memory_get").unwrap();
        assert_eq!(
            argv(tool, &json!({"ids": "ab12 cd34"})).unwrap(),
            ["get", "--", "ab12", "cd34"]
        );
    }

    #[test]
    fn list_arguments_pass_each_element_as_one_positional() {
        let tool = TOOLS.iter().find(|t| t.name == "submit_work").unwrap();
        let id = "01a19a44-e705-743e-86f7-126db8e7b5eb";
        assert_eq!(
            argv(
                tool,
                &json!({"task_id": id, "message": "- Fix it", "paths": ["a b.rs", "-c.rs"]})
            )
            .unwrap(),
            ["submit-work", "--", id, "- Fix it", "a b.rs", "-c.rs"]
        );
        assert_eq!(
            argv(tool, &json!({"task_id": id, "message": "Retry", "paths": ["a b.rs"], "resume_commit": "abc123"})).unwrap(),
            ["submit-work", "--resume-commit=abc123", "--", id, "Retry", "a b.rs"]
        );
        for paths in [json!([]), json!(null), json!("a.rs"), json!([1])] {
            assert!(
                argv(
                    tool,
                    &json!({"task_id": id, "message": "m", "paths": paths})
                )
                .is_err(),
                "{paths}"
            );
        }
        assert_eq!(
            schema(tool)["properties"]["paths"],
            json!({"type": "array", "items": {"type": "string"}, "description": tool.args[2].description})
        );
        assert_eq!(
            schema(tool)["required"],
            json!(["task_id", "message", "paths"])
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
        assert_eq!(claude[0], "--settings");
        let settings: Value = serde_json::from_str(&claude[1]).unwrap();
        assert_eq!(settings["attribution"], json!({"commit": "", "pr": ""}));
        assert_eq!(settings["includeCoAuthoredBy"], false);
        assert_eq!(settings["permissions"], json!({"allow": ["mcp__amux__*"]}));
        assert_eq!(claude[2], "--mcp-config");
        let config: Value = serde_json::from_str(&claude[3]).unwrap();
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
        assert!(
            codex.contains(&"mcp_servers.amux.default_tools_approval_mode=\"approve\"".to_owned())
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
