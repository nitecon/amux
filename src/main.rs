use amux::{
    gateway::Gateway,
    identity::{self, AgentIdentity, AgentRecord, Provider},
};
use anyhow::{Context, Result, bail};
use clap::{Parser, Subcommand};
use reqwest::Method;
use serde_json::json;
use std::{
    path::PathBuf,
    process::{Command, ExitCode},
};
use uuid::Uuid;

#[cfg(feature = "desktop")]
mod desktop;

#[derive(Parser)]
#[command(version, about = "Agent terminals and gateway task coordination")]
struct Cli {
    #[arg(long, global = true, default_value = ".")]
    project_dir: PathBuf,
    #[command(subcommand)]
    command: Option<Operation>,
}

#[derive(Subcommand)]
enum Operation {
    /// Launch the desktop (also the default).
    Desktop,
    /// Run a native agent with its own session and execution identity.
    Agent {
        #[arg(value_enum)]
        provider: Provider,
        #[arg(last = true)]
        args: Vec<String>,
    },
    /// Print the calling agent's full execution identity.
    Whoami,
    /// List live amux-launched agent sessions, including three in one project.
    Agents,
    /// Use the authoritative gateway without agent-tools installed.
    Tasks {
        #[arg(long, env = "AMUX_GATEWAY_PROJECT")]
        project: String,
        #[arg(long, env = "AMUX_GATEWAY_URL")]
        gateway_url: String,
        #[command(subcommand)]
        operation: TaskOperation,
    },
    /// Publish and retrieve inline gateway handoff text (not binary attachments).
    Messages {
        #[arg(long, env = "AMUX_GATEWAY_PROJECT")]
        project: String,
        #[arg(long, env = "AMUX_GATEWAY_URL")]
        gateway_url: String,
        #[command(subcommand)]
        operation: MessageOperation,
    },
}

#[derive(Subcommand)]
enum MessageOperation {
    /// Include resolved threads; use offset to retrieve subsequent pages.
    List {
        #[arg(long, default_value_t = 50, value_parser = clap::value_parser!(u16).range(1..=500))]
        limit: u16,
        #[arg(long, default_value_t = 0)]
        offset: usize,
    },
    /// Retrieve a complete thread by its gateway-local numeric message ID.
    Get {
        #[arg(value_parser = clap::value_parser!(u64).range(1..=i64::MAX as u64))]
        id: u64,
    },
    /// Publish inline text; may notify the project's configured external channel.
    /// A lost reply is uncertain: check messages before manually retrying.
    Send {
        body: String,
        #[arg(long)]
        subject: Option<String>,
    },
}

#[derive(Subcommand)]
enum TaskOperation {
    List,
    Get {
        id: Uuid,
    },
    Create {
        title: String,
        #[arg(long)]
        specification: Option<String>,
    },
    Claim {
        id: Uuid,
    },
    Comment {
        id: Uuid,
        body: String,
    },
    /// Explicit execution-start report; claiming alone does not report start.
    Started {
        id: Uuid,
    },
    Done {
        id: Uuid,
    },
    Release {
        id: Uuid,
    },
}

fn main() -> ExitCode {
    match run(Cli::parse()) {
        Ok(code) => ExitCode::from(code),
        Err(error) => {
            eprintln!("amux: {error:#}");
            ExitCode::FAILURE
        }
    }
}

fn run(cli: Cli) -> Result<u8> {
    match cli.command.unwrap_or(Operation::Desktop) {
        Operation::Desktop => {
            #[cfg(feature = "desktop")]
            {
                desktop::run(cli.project_dir)?;
            }
            #[cfg(not(feature = "desktop"))]
            bail!("desktop not included; build without --no-default-features");
        }
        Operation::Agent { provider, args } => {
            return launch_agent(cli.project_dir, provider, args);
        }
        Operation::Whoami => println!(
            "{}",
            serde_json::to_string_pretty(&AgentIdentity::from_environment()?)?
        ),
        Operation::Agents => println!(
            "{}",
            serde_json::to_string_pretty(&identity::live_agents(&identity::state_directory()?)?)?
        ),
        Operation::Messages {
            project,
            gateway_url,
            operation,
        } => {
            let token = std::env::var("AMUX_GATEWAY_KEY")
                .context("set AMUX_GATEWAY_KEY for gateway access")?;
            let gateway = Gateway::new(&gateway_url, token)?;
            let runtime = tokio::runtime::Runtime::new()?;
            let value = runtime.block_on(async {
                match operation {
                    MessageOperation::List { limit, offset } => {
                        gateway.messages(&project, limit, offset).await
                    }
                    MessageOperation::Get { id } => gateway.message(&project, id).await,
                    MessageOperation::Send { body, subject } => {
                        let actor = AgentIdentity::verified_from_environment()?;
                        if actor.project_directory != cli.project_dir.canonicalize()? {
                            bail!("message publication project directory differs from this agent's launch; switch to its project directory");
                        }
                        gateway.send_message(&project, body, subject, &actor).await
                    }
                }
            })?;
            println!("{}", serde_json::to_string_pretty(&value)?);
        }
        Operation::Tasks {
            project,
            gateway_url,
            operation,
        } => {
            let token = std::env::var("AMUX_GATEWAY_KEY")
                .context("set AMUX_GATEWAY_KEY for gateway access")?;
            let gateway = Gateway::new(&gateway_url, token)?;
            let runtime = tokio::runtime::Runtime::new()?;
            let value = runtime.block_on(async {
                if matches!(operation, TaskOperation::List) { return gateway.list(&project).await; }
                if let TaskOperation::Get { id } = operation { return gateway.request(Method::GET, &project, &[&id.to_string()], None, None).await; }
                let actor = AgentIdentity::verified_from_environment()?;
                if actor.project_directory != cli.project_dir.canonicalize()? { bail!("task mutation project directory differs from this agent's launch; switch to its project directory"); }
                match operation {
                    TaskOperation::Create { title, specification } => gateway.request(Method::POST, &project, &[], Some(json!({"title": title, "specification": specification})), Some(&actor)).await,
                    TaskOperation::Claim { id } => gateway.claim(&project, &id.to_string(), &actor).await,
                    TaskOperation::Comment { id, body } => gateway.request(Method::POST, &project, &[&id.to_string(), "comments"], Some(json!({"content":body})), Some(&actor)).await,
                    TaskOperation::Started { id } => {
                        confirm_owner(&gateway, &project, id, &actor).await?;
                        gateway.request(Method::POST, &project, &[&id.to_string(), "comments"], Some(json!({"content":format!("Agent reports execution started: session={} instance={}; recorded as a comment, not an atomic gateway lifecycle transition", actor.session_id, actor.instance_id)})), Some(&actor)).await
                    },
                    TaskOperation::Done { id } | TaskOperation::Release { id } => {
                        confirm_owner(&gateway, &project, id, &actor).await?;
                        let status = if matches!(operation, TaskOperation::Done { .. }) { "done" } else { "todo" };
                        gateway.request(Method::PATCH, &project, &[&id.to_string()], Some(json!({"status":status})), Some(&actor)).await
                    },
                    _ => unreachable!(),
                }
            })?;
            println!("{}", serde_json::to_string_pretty(&value)?);
        }
    }
    Ok(0)
}

/// Only the exact owning execution may report start, complete or release a task.
/// This is a client preflight read, not atomic server authorization. The gateway
/// already guards bound owner_origin claims, but until it enforces the exact
/// owner atomically for every claim type, ownership can change between this read
/// and the mutation.
async fn confirm_owner(
    gateway: &Gateway,
    project: &str,
    id: Uuid,
    actor: &AgentIdentity,
) -> Result<()> {
    let task = gateway
        .request(Method::GET, project, &[&id.to_string()], None, None)
        .await?;
    if task["status"] != "in_progress"
        || task["owner_origin"]["session_id"] != actor.session_id.to_string()
        || task["owner_origin"]["instance_id"] != actor.instance_id.to_string()
    {
        bail!(
            "only the confirmed owning execution (this exact session and instance) may change this task; no status update or start report was sent"
        );
    }
    Ok(())
}

fn launch_agent(project: PathBuf, provider: Provider, args: Vec<String>) -> Result<u8> {
    let actor = AgentIdentity::new(provider, &project)?;
    let root = identity::state_directory()?.join("agents");
    std::fs::create_dir_all(&root)?;
    let mut command = Command::new(provider.command());
    command
        .args(args)
        .current_dir(&actor.project_directory)
        .envs(actor.environment());
    #[cfg(target_os = "linux")]
    {
        use std::os::unix::process::CommandExt;
        let parent = std::process::id();
        // SAFETY: only async-signal-safe syscalls run between fork and exec.
        // This fences the direct owned agent even when it ignores terminal HUP.
        unsafe {
            command.pre_exec(move || {
                if libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGKILL) == -1 {
                    return Err(std::io::Error::last_os_error());
                }
                if libc::getppid() as u32 != parent {
                    return Err(std::io::Error::other("agent launcher exited during spawn"));
                }
                Ok(())
            });
        }
    }
    // Agent subprocesses can call this same binary without installing agent-tools.
    if let Some(bin) = std::env::current_exe()?.parent() {
        let mut paths = vec![bin.to_path_buf()];
        paths.extend(std::env::split_paths(
            &std::env::var_os("PATH").unwrap_or_default(),
        ));
        command.env("PATH", std::env::join_paths(paths)?);
    }
    let mut child = command.spawn().with_context(|| {
        format!(
            "launch {}; install it and ensure it is on PATH",
            provider.command()
        )
    })?;
    // The foreground group already delivers Ctrl-C to the provider. The wrapper
    // must keep waiting so it cannot turn an ordinary interrupt into agent death.
    #[cfg(unix)]
    let _interrupt = match signal_hook::flag::register(
        signal_hook::consts::SIGINT,
        std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false)),
    ) {
        Ok(id) => id,
        Err(error) => {
            let _ = child.kill();
            let _ = child.wait();
            return Err(error.into());
        }
    };
    let record = match AgentRecord::inspect(actor.clone(), child.id()) {
        Ok(r) => r,
        Err(error) => {
            if let Some(status) = child.try_wait()? {
                return Ok(status.code().unwrap_or(1).clamp(0, 255) as u8);
            }
            let _ = child.kill();
            let _ = child.wait();
            return Err(error);
        }
    };
    let path = root.join(format!("{}.json", actor.session_id));
    let registration = (|| -> Result<()> {
        use std::io::Write;
        let mut options = std::fs::OpenOptions::new();
        options.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        let mut file = options.open(&path)?;
        file.write_all(&serde_json::to_vec(&record)?)?;
        Ok(())
    })();
    if let Err(error) = registration {
        let _ = child.kill();
        let _ = child.wait();
        return Err(error);
    }
    let status = child.wait();
    let _ = std::fs::remove_file(path);
    let status = status?;
    Ok(status.code().unwrap_or(1).clamp(0, 255) as u8)
}
