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
    #[arg(long, global = true)]
    project_dir: Option<PathBuf>,
    /// Assigned amux agent ID, shown in gateway notifications.
    #[arg(long, global = true)]
    agent: Option<String>,
    /// Update a direct-download install; managed installs show their update command.
    #[arg(long, exclusive = true)]
    update: bool,
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
    /// Use the authoritative gateway without agent-tools installed. The URL and
    /// key default to saved gateway preferences; the project resolves from Git.
    Tasks {
        #[arg(long, env = "AMUX_GATEWAY_PROJECT")]
        project: Option<String>,
        #[arg(long, env = "AMUX_GATEWAY_URL")]
        gateway_url: Option<String>,
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
    /// Exact-session task dispatch through the gateway's credential-bound API.
    /// Needs AMUX_GATEWAY_KEY plus the operator-issued AMUX_GATEWAY_MACHINE_ID
    /// and AMUX_GATEWAY_MACHINE_KEY; amux never provisions or stores them.
    Dispatch {
        #[arg(long, env = "AMUX_GATEWAY_PROJECT")]
        project: String,
        #[arg(long, env = "AMUX_GATEWAY_URL")]
        gateway_url: String,
        #[command(subcommand)]
        operation: amux::dispatch::Command,
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
    /// Create linked tasks in different projects, not an exact-session assignment.
    /// A lost reply is uncertain: inspect both projects before manually retrying.
    Delegate {
        title: String,
        #[arg(long)]
        target_project: String,
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
    /// Record cancellation; this does not confirm the executor has stopped.
    Cancel {
        id: Uuid,
        #[arg(long)]
        reason: String,
    },
    /// Fetch a cancellation report, including after an uncertain submission.
    Cancellation {
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
    if cli.update {
        println!("{}", amux::updates::cli_update()?);
        return Ok(0);
    }
    let project_dir = cli
        .project_dir
        .clone()
        .unwrap_or_else(|| PathBuf::from("."));
    match cli.command.unwrap_or(Operation::Desktop) {
        Operation::Desktop => {
            #[cfg(feature = "desktop")]
            {
                desktop::run(
                    cli.project_dir
                        .map(Ok)
                        .unwrap_or_else(amux::workspace_launch::home_directory)?,
                )?;
            }
            #[cfg(not(feature = "desktop"))]
            bail!("desktop not included; build without --no-default-features");
        }
        Operation::Agent { provider, args } => {
            return launch_agent(project_dir, provider, args);
        }
        Operation::Whoami => println!(
            "{}",
            serde_json::to_string_pretty(&AgentIdentity::assigned(
                cli.agent.as_deref(),
                &project_dir
            )?)?
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
                        let actor = AgentIdentity::assigned(cli.agent.as_deref(), &project_dir)?;
                        if actor.project_directory != amux::project_identity::resolve(&project_dir)?.directory {
                            bail!("message publication project directory differs from this agent's launch; switch to its project directory");
                        }
                        gateway.send_message(&project, body, subject, &actor).await
                    }
                }
            })?;
            println!("{}", serde_json::to_string_pretty(&value)?);
        }
        Operation::Dispatch {
            project,
            gateway_url,
            operation,
        } => {
            use amux::dispatch::{Dispatcher, Executor, Machine};
            let actor = AgentIdentity::verified_from_environment()?;
            if actor.project_directory != amux::project_identity::resolve(&project_dir)?.directory {
                bail!(
                    "dispatch project directory differs from this agent's launch; switch to its project directory"
                );
            }
            let token = std::env::var("AMUX_GATEWAY_KEY")
                .context("set AMUX_GATEWAY_KEY for gateway access")?;
            let machine = Machine::new(
                std::env::var("AMUX_GATEWAY_MACHINE_ID")
                    .context("set AMUX_GATEWAY_MACHINE_ID to the provisioned machine UUID")?
                    .parse()
                    .context("AMUX_GATEWAY_MACHINE_ID must be a UUID")?,
                std::env::var("AMUX_GATEWAY_MACHINE_KEY")
                    .context("set AMUX_GATEWAY_MACHINE_KEY to the operator-issued key")?,
            )?;
            let state = identity::state_directory()?;
            let executor = Executor::load(&state, actor)?;
            let dispatcher = Dispatcher::new(&gateway_url, token, &project, machine, state)?;
            let runtime = tokio::runtime::Runtime::new()?;
            let value = runtime.block_on(amux::dispatch::run(&dispatcher, &executor, operation))?;
            println!("{}", serde_json::to_string_pretty(&value)?);
        }
        Operation::Tasks {
            project,
            gateway_url,
            operation,
        } => {
            let saved = amux::gateway_preferences::GatewayPreferences::load(
                &identity::state_directory()?.join("gateway-preferences.json"),
            )
            .ok()
            .flatten()
            .unwrap_or_default();
            let gateway_url = gateway_url
                .or_else(|| Some(saved.url.clone()).filter(|url| !url.trim().is_empty()))
                .context("set --gateway-url, AMUX_GATEWAY_URL or desktop gateway preferences")?;
            let token = std::env::var("AMUX_GATEWAY_KEY")
                .ok()
                .filter(|key| !key.trim().is_empty())
                .or_else(|| Some(saved.api_key).filter(|key| !key.trim().is_empty()))
                .context(
                    "set AMUX_GATEWAY_KEY or desktop gateway preferences for gateway access",
                )?;
            let gateway = Gateway::new(gateway_url.trim(), token)?;
            let runtime = tokio::runtime::Runtime::new()?;
            let value = runtime.block_on(async {
                let project = match project {
                    Some(project) => project,
                    None => gateway.resolve_project(project_dir.canonicalize()?).await?,
                };
                if matches!(operation, TaskOperation::List) { return gateway.list(&project).await; }
                if let TaskOperation::Get { id } = operation { return gateway.request(Method::GET, &project, &[&id.to_string()], None, None).await; }
                if let TaskOperation::Cancellation { id } = operation { return gateway.cancellation(&project, id).await; }
                let actor = AgentIdentity::assigned(cli.agent.as_deref(), &project_dir)?;
                if actor.project_directory != amux::project_identity::resolve(&project_dir)?.directory { bail!("task mutation project directory differs from this agent's launch; switch to its project directory"); }
                match operation {
                    TaskOperation::Create { title, specification } => gateway.request(Method::POST, &project, &[], Some(json!({"title": title, "specification": specification})), Some(&actor)).await,
                    TaskOperation::Delegate { title, target_project, specification } => gateway.delegate(&project, &target_project, title, specification, &actor).await,
                    TaskOperation::Claim { id } => gateway.claim(&project, &id.to_string(), &actor).await,
                    TaskOperation::Cancel { id, reason } => gateway.cancel_task(&project, id, &reason, &actor).await,
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
    let mut command = Command::new(provider.command());
    command
        .args(args)
        .current_dir(amux::workspace_launch::external_path(
            project.canonicalize()?,
        ))
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
    if let Err(error) = record.register() {
        let _ = child.kill();
        let _ = child.wait();
        return Err(error);
    }
    let status = child.wait();
    // Retain the private native generation record for recovery. A missing
    // gateway lease alone cannot prove this executor has stopped; live_agents
    // still lists only records whose native process generation is running.
    let status = status?;
    Ok(status.code().unwrap_or(1).clamp(0, 255) as u8)
}
