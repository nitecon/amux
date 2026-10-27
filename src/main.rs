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
    #[arg(skip)]
    explicit_gateway: bool,
    #[arg(long, global = true)]
    project_dir: Option<PathBuf>,
    /// Assigned amux agent ID, shown in gateway notifications.
    #[arg(long, global = true)]
    agent: Option<String>,
    /// Update a direct-download install; managed installs show their update command.
    #[arg(long, exclusive = true)]
    update: bool,
    /// Print the bundled general coding guidelines without opening the desktop.
    #[arg(long, exclusive = true)]
    get_guidelines: bool,
    #[command(subcommand)]
    command: Option<Operation>,
}

#[derive(Subcommand)]
enum Operation {
    /// Host the API-compatible gateway using its existing .env/environment settings.
    Server {
        #[command(subcommand)]
        command: Option<amux::server::Command>,
    },
    /// Check or install managed task instructions for local coding agents.
    Harness {
        #[command(subcommand)]
        operation: HarnessOperation,
    },
    /// Inspect or finish a local Autoreview receipt.
    Reviews {
        #[command(subcommand)]
        operation: ReviewOperation,
    },
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
        /// Print the raw gateway JSON instead of Markdown.
        #[arg(long, global = true)]
        json: bool,
        #[command(subcommand)]
        operation: TaskOperation,
    },
    /// Publish and retrieve inline gateway handoff text (not binary attachments).
    Messages {
        #[arg(long, env = "AMUX_GATEWAY_PROJECT")]
        project: String,
        #[arg(long, env = "AMUX_GATEWAY_URL")]
        gateway_url: Option<String>,
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
        gateway_url: Option<String>,
        #[command(subcommand)]
        operation: amux::dispatch::Command,
    },
}

#[derive(Subcommand)]
enum HarnessOperation {
    /// Detect providers and check instructions/settings without changing files.
    Check,
    /// Print manual instructions and settings snippets without editing files.
    Print {
        #[arg(value_enum)]
        provider: amux::harness::Provider,
    },
    /// Install/update one provider's global rules and local-task settings.
    Install {
        #[arg(value_enum)]
        provider: amux::harness::Provider,
    },
}

#[derive(Subcommand)]
enum ReviewOperation {
    Status,
    Complete {
        id: Uuid,
    },
    /// Allow retry only after explicitly confirming the prior review has stopped.
    Release {
        id: Uuid,
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
    /// List tasks (default: todo and in_progress).
    List {
        /// Comma-separated statuses: todo, in_progress, user_review, done, cancelled.
        #[arg(long)]
        status: Option<String>,
        /// Include done tasks older than the gateway's falloff window.
        #[arg(long)]
        include_stale: bool,
    },
    /// List connected project agents across machines, including OS and execution identity.
    Agents {
        #[arg(long, value_enum)]
        os: Option<amux::task_platform::TaskOs>,
    },
    Get {
        id: Uuid,
    },
    Create {
        title: String,
        #[arg(long, alias = "details")]
        specification: Option<String>,
        /// Record a review finding for human triage; do not auto-execute it.
        #[arg(long)]
        review_finding: bool,
        #[arg(long, value_enum)]
        os: Option<amux::task_platform::TaskOs>,
        /// Request comment/completion notices for this exact agent execution.
        #[arg(long)]
        notify_results: bool,
        /// Reserve for a live same-project agent and subscribe this caller to results.
        #[arg(long)]
        to_agent: Option<Uuid>,
        #[command(flatten)]
        details: DetailArgs,
    },
    /// Create linked tasks in different projects, not an exact-session assignment.
    /// A lost reply is uncertain: inspect both projects before manually retrying.
    Delegate {
        title: String,
        #[arg(long)]
        target_project: String,
        #[arg(long, alias = "details")]
        specification: Option<String>,
        /// Subscribe this execution to results on the source tracking task.
        #[arg(long)]
        notify_results: bool,
        #[command(flatten)]
        details: DetailArgs,
    },
    Claim {
        id: Uuid,
    },
    /// Set a task's absolute rank (lower = higher priority within its column).
    Rank {
        id: Uuid,
        #[arg(allow_negative_numbers = true)]
        rank: i64,
    },
    /// Hand an active task to one exact, currently connected project agent.
    Reassign {
        id: Uuid,
        #[arg(long)]
        to_agent: Uuid,
    },
    /// Receive future comment/completion results for an authorized task, or unsubscribe.
    Watch {
        id: Uuid,
        #[arg(long)]
        clear: bool,
    },
    /// Change the OS requirement of owned work; incompatible work returns to the queue.
    SetOs {
        id: Uuid,
        #[arg(long, value_enum, required_unless_present = "clear")]
        os: Option<amux::task_platform::TaskOs>,
        #[arg(long, conflicts_with = "os")]
        clear: bool,
    },
    /// Park an owned task for a human decision and release agent ownership.
    #[command(name = "user-review", alias = "review")]
    UserReview {
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
    /// Bind this Git checkout, which has no origin remote, to an existing ad-hoc
    /// gateway project. Only this exact checkout root uses the binding.
    Bind {
        ident: String,
    },
}

#[derive(clap::Args)]
struct DetailArgs {
    /// One-paragraph summary.
    #[arg(long)]
    description: Option<String>,
    /// Repeatable task label.
    #[arg(long = "label")]
    labels: Vec<String>,
    /// Originating host. Defaults to this machine's hostname.
    #[arg(long)]
    hostname: Option<String>,
}

impl DetailArgs {
    fn into_details(self) -> amux::gateway::TaskDetails {
        amux::gateway::TaskDetails {
            description: self.description,
            labels: self.labels,
            hostname: self.hostname.or_else(sysinfo::System::host_name),
        }
    }
}

fn main() -> ExitCode {
    let mut cli = Cli::parse();
    cli.explicit_gateway = std::env::args_os()
        .any(|arg| arg == "--gateway-url" || arg.to_string_lossy().starts_with("--gateway-url="));
    match run(cli) {
        Ok(code) => ExitCode::from(code),
        Err(error) => {
            eprintln!("amux: {error:#}");
            ExitCode::FAILURE
        }
    }
}

fn run(cli: Cli) -> Result<u8> {
    if cli.get_guidelines {
        print!("{}", amux::autoreview::GUIDELINES);
        return Ok(0);
    }
    if cli.update {
        println!("{}", amux::updates::cli_update()?);
        return Ok(0);
    }
    let project_dir = cli
        .project_dir
        .clone()
        .unwrap_or_else(|| PathBuf::from("."));
    match cli.command.unwrap_or(Operation::Desktop) {
        Operation::Server { command } => amux::server::run(command)?,
        Operation::Harness { operation } => {
            let environment = amux::harness::Environment::discover()?;
            match operation {
                HarnessOperation::Check => {
                    println!("{}", serde_json::to_string_pretty(&environment.reports())?)
                }
                HarnessOperation::Print { provider } => {
                    print!("{}", environment.manual_instructions(provider)?)
                }
                HarnessOperation::Install { provider } => {
                    println!(
                        "{}",
                        serde_json::to_string_pretty(&environment.install(provider)?)?
                    );
                    println!(
                        "Start a new provider session to load the updated instructions and settings."
                    );
                }
            }
        }
        Operation::Reviews { operation } => {
            let path = amux::autoreview::path(&project_dir)?;
            match operation {
                ReviewOperation::Status => println!(
                    "{}",
                    serde_json::to_string_pretty(&amux::autoreview::load(&path)?)?
                ),
                ReviewOperation::Complete { id } => {
                    let actor = AgentIdentity::assigned(cli.agent.as_deref(), &project_dir)?;
                    amux::autoreview::complete(&path, id, &actor)?;
                    println!("Review {id} completed");
                }
                ReviewOperation::Release { id } => {
                    amux::autoreview::release(&path, id)?;
                    println!("Review {id} released without marking commits reviewed");
                }
            }
        }
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
            let runtime = tokio::runtime::Runtime::new()?;
            let value = runtime.block_on(async {
                let (gateway, _server) = cli_gateway(gateway_url, cli.explicit_gateway).await?;
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
            let runtime = tokio::runtime::Runtime::new()?;
            let value = runtime.block_on(async {
                let (gateway, _server) = cli_gateway(gateway_url, cli.explicit_gateway).await?;
                let dispatcher = Dispatcher::from_profile(
                    gateway.transport_profile(),
                    &project,
                    machine,
                    state,
                )?;
                amux::dispatch::run(&dispatcher, &executor, operation).await
            })?;
            println!("{}", serde_json::to_string_pretty(&value)?);
        }
        Operation::Tasks {
            project,
            gateway_url,
            json,
            operation,
        } => {
            let runtime = tokio::runtime::Runtime::new()?;
            let value = runtime.block_on(async {
                let (gateway, _server) = cli_gateway(gateway_url, cli.explicit_gateway).await?;
                if let TaskOperation::Bind { ident } = &operation {
                    let directory = project_dir.canonicalize()?;
                    anyhow::ensure!(amux::gateway_project::configured_origin(&directory)?.is_none(), "workspace has a Git origin; it resolves by repository and cannot be bound");
                    let root = amux::project_identity::resolve(&directory)?.directory;
                    anyhow::ensure!(amux::gateway_project::local_only(ident, &gateway.projects().await?)?, "gateway project is not an active ad-hoc project without a repository");
                    amux::gateway_project::bind_project_at(&identity::state_directory()?, &root, ident)?;
                    return Ok(json!({"project": ident, "root": root}));
                }
                let project = match project {
                    Some(project) => project,
                    None => gateway.resolve_project(project_dir.canonicalize()?).await?,
                };
                if let TaskOperation::List { status, include_stale } = &operation { return gateway.list_filtered(&project, status.as_deref(), *include_stale).await; }
                if let TaskOperation::Agents { os } = operation { return Ok(serde_json::to_value(gateway.task_agents(&project, &project_dir, os).await?)?); }
                if let TaskOperation::Get { id } = operation { return gateway.request(Method::GET, &project, &[&id.to_string()], None, None).await; }
                if let TaskOperation::Cancellation { id } = operation { return gateway.cancellation(&project, id).await; }
                let actor = AgentIdentity::assigned(cli.agent.as_deref(), &project_dir)?;
                if actor.project_directory != amux::project_identity::resolve(&project_dir)?.directory { bail!("task mutation project directory differs from this agent's launch; switch to its project directory"); }
                match operation {
                    TaskOperation::Create { title, specification, review_finding, os, notify_results, to_agent, details } => {
                        let mut body = json!({"title": title, "specification": specification});
                        let mut labels = Vec::new();
                        if review_finding { labels.push("autoreview-finding".to_owned()); }
                        if let Some(os) = os {
                            anyhow::ensure!(gateway.resolve_project(actor.project_directory.clone()).await? == project, "Workspace project changed");
                            labels.push(format!("os:{}", os.as_str()));
                        }
                        if !labels.is_empty() { body["labels"] = json!(labels); }
                        details.into_details().apply(&mut body)?;
                        let executor = if let Some(session) = to_agent {
                            let directory = gateway.task_agents(&project, &actor.project_directory, os).await?;
                            let mut matching = directory.targets.into_iter().filter(|entry| entry.target.origin.session_id == session);
                            let target = matching.next().context("recipient is unavailable or incompatible")?.target;
                            anyhow::ensure!(matching.next().is_none(), "recipient has multiple live executions");
                            body["routing"] = json!({"mode":"session", "target":target});
                            Some(target)
                        } else { None };
                        let task = if notify_results || to_agent.is_some() {
                            let requester = gateway.requester_target(&project, &actor).await?;
                            gateway.create_task_with_requester(&project, body, &actor, &requester).await?
                        } else {
                            gateway.request(Method::POST, &project, &[], Some(body), Some(&actor)).await?
                        };
                        if let Some(executor) = &executor {
                            anyhow::ensure!(task["routing"] == json!({"mode":"session", "target":executor}) && task["owner_origin"] == json!(executor.origin),
                                "executor assignment unconfirmed; inspect task before retrying");
                        }
                        if let Some(os) = os {
                            anyhow::ensure!(amux::task_platform::required_os(&task)? == Some(os), "OS requirement unconfirmed; inspect task before retrying");
                            if actor.os != os.as_str() && executor.is_none() {
                                anyhow::ensure!(task["status"] == "todo" && task.get("owner_origin").is_some_and(serde_json::Value::is_null) && task.get("owner_agent_id").is_some_and(serde_json::Value::is_null), "Gateway retained an incompatible owner; inspect task before retrying");
                            }
                        }
                        Ok(task)
                    },
                    TaskOperation::Delegate { title, target_project, specification, notify_results, details } => {
                        let requester = if notify_results { Some(gateway.requester_target(&project, &actor).await?) } else { None };
                        gateway.delegate_with_requester(&project, &target_project, title, specification, &details.into_details(), &actor, requester.as_ref()).await
                    },
                    TaskOperation::Rank { id, rank } => gateway.rank(&project, id, rank, &actor).await,
                    TaskOperation::Claim { id } => {
                        let task = gateway.request(Method::GET, &project, &[&id.to_string()], None, None).await?;
                        anyhow::ensure!(amux::task_platform::matches(&task, Some(&actor.os)), "Task requires another operating system");
                        gateway.claim(&project, &id.to_string(), &actor).await
                    },
                    TaskOperation::Reassign { id, to_agent } => gateway.reassign_agent_task(&project, id, to_agent, &actor).await,
                    TaskOperation::Watch { id, clear } => {
                        let requester = if clear { None } else { Some(gateway.requester_target(&project, &actor).await?) };
                        gateway.set_task_requester(&project, id, requester.as_ref(), Some(&actor)).await
                    },
                    TaskOperation::SetOs { id, os, .. } => gateway.set_task_os(&project, id, os, &actor).await,
                    TaskOperation::UserReview { id } => gateway.request_user_review(&project, id, &actor).await,
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
            if json {
                println!("{}", serde_json::to_string_pretty(&value)?);
            } else {
                print!("{}", amux::task_markdown::render(&value));
            }
        }
    }
    Ok(0)
}

async fn cli_gateway(
    url: Option<String>,
    explicit: bool,
) -> Result<(Gateway, Option<amux::server_controller::LocalServer>)> {
    use amux::gateway_preferences::{GatewayMode, GatewayPreferences};
    let state = identity::state_directory()?;
    let path = state.join("gateway-preferences.json");
    let saved = GatewayPreferences::load(&path)?;
    let mut config = saved
        .clone()
        .unwrap_or_else(GatewayPreferences::from_environment);
    // A saved secure selection wins over stale environment inherited by an
    // already-running provider process. A deliberate CLI override remains explicit.
    if explicit {
        let url = url.context("gateway URL override is missing")?;
        let same_origin = config
            .resolved_url()
            .ok()
            .and_then(|saved| {
                Some(
                    amux::gateway_transport::origin(&saved, true).ok()?
                        == amux::gateway_transport::origin(&url, true).ok()?,
                )
            })
            .unwrap_or(false);
        if config.mode == GatewayMode::Legacy || !same_origin {
            config.mode = GatewayMode::Legacy;
            config.url = url;
            config.api_key = std::env::var("AMUX_GATEWAY_KEY")
                .ok()
                .filter(|key| !key.trim().is_empty())
                .unwrap_or(config.api_key);
        }
    } else if config.mode == GatewayMode::Legacy {
        if let Some(url) = url {
            config.url = url;
        }
        if let Ok(key) = std::env::var("AMUX_GATEWAY_KEY")
            && !key.trim().is_empty()
        {
            config.api_key = key;
        }
        if saved.is_none() && config.url.is_empty() && config.api_key.is_empty() {
            config.mode = GatewayMode::Server;
        }
    }
    let server = if config.mode == GatewayMode::Server {
        let server = amux::server_controller::ensure_local(state, config.server.clone()).await?;
        config.enrolled = Some(server.connection());
        config.save(&path)?;
        Some(server)
    } else {
        None
    };
    Ok((config.gateway()?, server))
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
    let actor = AgentIdentity::new(provider, &project)?.resume_at(
        &identity::state_directory()?,
        std::time::Duration::from_secs(5),
    );
    let restoring = args
        .iter()
        .map(String::as_str)
        .eq(amux::session::resume_arguments(provider).iter().copied());
    let mut status = launch_agent_once(&project, provider, &args, &actor)?;
    // Missing native conversation history exits with 1. Retry only the standard
    // restore invocation, once, leaving custom arguments and signals alone.
    if restoring && status.code() == Some(1) {
        eprintln!(
            "Unable to resume {}; starting a new session.",
            provider.command()
        );
        status = launch_agent_once(&project, provider, &[], &actor)?;
    }
    Ok(status.code().unwrap_or(1).clamp(0, 255) as u8)
}

fn launch_agent_once(
    project: &std::path::Path,
    provider: Provider,
    args: &[String],
    actor: &AgentIdentity,
) -> Result<std::process::ExitStatus> {
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
                return Ok(status);
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
    Ok(status?)
}
