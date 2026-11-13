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
    /// Print the bundled general coding guidelines without opening the desktop.
    #[arg(long, exclusive = true)]
    get_guidelines: bool,
    #[command(subcommand)]
    command: Option<Operation>,
}

#[cfg(test)]
mod task_creation_tests {
    use super::*;

    #[test]
    fn roots_require_an_explicit_supported_reason_and_specification() {
        let parse = |args: &[&str]| {
            Cli::try_parse_from(
                ["amux", "tasks", "create", "Work"]
                    .into_iter()
                    .chain(args.iter().copied()),
            )
        };
        for reason in ["user-directed", "principle"] {
            assert!(
                parse(&[
                    "--root-reason",
                    reason,
                    "--specification",
                    "Concrete request or finding"
                ])
                .is_ok()
            );
        }
        let parent = "44444444-4444-4444-8444-444444444444";
        assert!(parse(&["--parent", parent]).is_ok());
        for args in [
            vec![],
            vec!["--specification", "No root reason"],
            vec!["--root-reason", "user-directed"],
            vec!["--root-reason", "feature", "--specification", "An idea"],
            vec![
                "--parent",
                parent,
                "--root-reason",
                "principle",
                "--specification",
                "DRY",
            ],
            vec![
                "--root-reason",
                "principle",
                "--specification",
                "DRY",
                "--review-finding",
            ],
        ] {
            assert!(parse(&args).is_err(), "{args:?}");
        }
    }
}

#[derive(Subcommand)]
enum Operation {
    /// Host the native server or administer its local database.
    Server {
        #[command(subcommand)]
        command: amux::server::Command,
    },
    /// Check or install managed task instructions for local coding agents.
    Harness {
        #[command(subcommand)]
        operation: HarnessOperation,
    },
    /// Launch the desktop (also the default).
    Desktop,
    /// Run a native agent with its own session and execution identity.
    Agent {
        #[arg(value_enum)]
        provider: Provider,
        /// Register `amux mcp` (tasks, agent-tools and memory) for this session.
        #[arg(long)]
        mcp: bool,
        #[arg(last = true)]
        args: Vec<String>,
    },
    /// Serve amux tasks and the bundled agent-tools and memory commands as an
    /// MCP stdio server for one agent session.
    Mcp,
    /// Print the calling agent's full execution identity.
    Whoami,
    /// List live amux-launched agent sessions, including three in one project.
    Agents,
    /// Use the enrolled amux server. The project resolves from Git.
    Tasks {
        #[arg(long, env = "AMUX_GATEWAY_PROJECT")]
        project: Option<String>,
        #[arg(long)]
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
        #[arg(long)]
        gateway_url: Option<String>,
        #[command(subcommand)]
        operation: MessageOperation,
    },
    /// Commit an owned task's listed changed files and publish them as chosen
    /// in Settings (commit and push, or pull request); the result is posted on
    /// the task. Never commit task work directly.
    SubmitWork {
        #[arg(long, env = "AMUX_GATEWAY_PROJECT")]
        project: Option<String>,
        #[arg(long)]
        gateway_url: Option<String>,
        #[command(flatten)]
        work: WorkArgs,
    },
    /// Exact-session task dispatch through the gateway's credential-bound API.
    /// Needs the enrolled connection plus operator-issued AMUX_GATEWAY_MACHINE_ID
    /// and AMUX_GATEWAY_MACHINE_KEY; amux never provisions or stores them.
    Dispatch {
        #[arg(long, env = "AMUX_GATEWAY_PROJECT")]
        project: String,
        #[arg(long)]
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
    /// Read-only local Git work inventory with optional gateway and exact-SHA CI facts.
    Report(amux::work_report::Args),
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
        /// Show only the newest N comments.
        #[arg(long, default_value_t = 50)]
        comments: usize,
    },
    Create {
        title: String,
        /// General by default; use software for any code or configuration changes.
        #[arg(long, value_parser = ["general", "software"], default_value = "general")]
        task_type: String,
        /// Existing task whose scope includes this work. Read it before creating a subtask.
        #[arg(
            long,
            required_unless_present = "root_reason",
            conflicts_with = "root_reason"
        )]
        parent: Option<Uuid>,
        /// Create a root for an explicit user request or a core-principle finding.
        /// Explain the request or concrete finding in --specification.
        #[arg(long, value_parser = ["user-directed", "principle"], requires = "specification", conflicts_with = "review_finding")]
        root_reason: Option<String>,
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
        /// General by default; use software for any code or configuration changes.
        #[arg(long, value_parser = ["general", "software"], default_value = "general")]
        task_type: String,
        /// Parent task in this project for the cross-project handoff.
        #[arg(long)]
        parent: Uuid,
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
    /// Convert owned work before making code or configuration changes.
    SetType {
        id: Uuid,
        #[arg(value_parser = ["general", "software"])]
        task_type: String,
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
        /// Exact decision or action the user must take.
        action: String,
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
    /// Reached through the top-level `submit-work`, sharing task resolution.
    #[command(skip)]
    SubmitWork(WorkArgs),
    /// Bind this Git checkout, which has no origin remote, to an existing ad-hoc
    /// gateway project. Only this exact checkout root uses the binding.
    Bind {
        ident: String,
    },
}

#[derive(clap::Args)]
struct WorkArgs {
    /// The owned task the work completes.
    task: Uuid,
    /// Commit message; attribution lines such as Co-Authored-By are removed.
    message: String,
    /// Publish this existing HEAD task commit without committing files again.
    #[arg(long)]
    resume_commit: Option<String>,
    /// Changed files to submit: modified, added or deleted, inside the project.
    #[arg(required_unless_present = "paths_file", conflicts_with = "paths_file")]
    paths: Vec<PathBuf>,
    /// JSON array of exact changed paths, for lists too large for the command line.
    #[arg(long)]
    paths_file: Option<PathBuf>,
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
            task_type: None,
            description: self.description,
            labels: self.labels,
            hostname: self.hostname.or_else(sysinfo::System::host_name),
        }
    }
}

fn main() -> ExitCode {
    let cli = Cli::parse();
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
    let operation = match cli.command.unwrap_or(Operation::Desktop) {
        Operation::SubmitWork {
            project,
            gateway_url,
            work,
        } => Operation::Tasks {
            project,
            gateway_url,
            json: false,
            operation: TaskOperation::SubmitWork(work),
        },
        operation => operation,
    };
    match operation {
        Operation::SubmitWork { .. } => unreachable!("rewritten as a task operation"),
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
        Operation::Agent {
            provider,
            mcp,
            args,
        } => {
            return launch_agent(project_dir, provider, mcp, args);
        }
        Operation::Mcp => {
            amux::mcp::serve()?;
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
                let (gateway, _server, _) = cli_gateway(gateway_url).await?;
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
                let (gateway, _server, _) = cli_gateway(gateway_url).await?;
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
            // Reports must work without enrollment and must not start a server,
            // register projects, assign an actor, or write task/release state.
            if let TaskOperation::Report(args) = &operation {
                let runtime = tokio::runtime::Runtime::new()?;
                let report = runtime.block_on(async {
                    let mut report = amux::work_report::local(&project_dir, args).await?;
                    if args.offline {
                        report.partial.push(
                            "Offline: gateway tasks/reviews and GitHub CI not requested".into(),
                        );
                    } else {
                        let tasks = async {
                            let config = amux::gateway_preferences::GatewayPreferences::load(
                                &identity::state_directory()?.join("gateway-preferences.json"),
                            )?
                            .context("No saved gateway connection")?;
                            if let Some(url) = &gateway_url {
                                anyhow::ensure!(
                                    amux::gateway_transport::origin(&config.resolved_url()?, true)?
                                        == amux::gateway_transport::origin(url, true)?,
                                    "Gateway override differs from enrolled connection"
                                );
                            }
                            let gateway = config.gateway()?;
                            let project = if let Some(project) = &project {
                                project.clone()
                            } else {
                                let projects = gateway.projects().await?;
                                let root = amux::project_identity::resolve(&project_dir)?.directory;
                                if let Some(remote) =
                                    amux::gateway_project::configured_origin(&root)?
                                {
                                    amux::gateway_project::matching(&remote, &projects)?.context(
                                        "No existing gateway project maps to this repository",
                                    )?
                                } else {
                                    let binding = amux::gateway_project::bound_project_at(
                                        &identity::state_directory()?,
                                        &root,
                                    )?
                                    .context("No existing project binding")?;
                                    anyhow::ensure!(
                                        amux::gateway_project::local_only(&binding, &projects)?,
                                        "Invalid project binding"
                                    );
                                    binding
                                }
                            };
                            report.enrich_tasks(&gateway, &project).await
                        };
                        match tokio::time::timeout(std::time::Duration::from_secs(60), tasks).await
                        {
                            Ok(Ok(())) => {}
                            Ok(Err(error)) => report
                                .partial
                                .push(format!("Gateway task/review facts unavailable: {error:#}")),
                            Err(_) => report.partial.push(
                                "Gateway task/review lookup timed out; inventory may be incomplete"
                                    .into(),
                            ),
                        }
                        if let Err(error) = report.enrich_ci(&project_dir).await {
                            report
                                .partial
                                .push(format!("Exact-SHA GitHub CI facts unavailable: {error:#}"));
                        }
                    }
                    Ok::<_, anyhow::Error>(report)
                })?;
                if json {
                    println!("{}", serde_json::to_string_pretty(&report)?);
                } else {
                    print!("{}", report.render());
                }
                return Ok(0);
            }
            let runtime = tokio::runtime::Runtime::new()?;
            let value = runtime.block_on(async {
                let (gateway, _server, preferences) = cli_gateway(gateway_url).await?;
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
                if let TaskOperation::Get { id, comments } = operation { return gateway.task(&project, id, comments).await; }
                if let TaskOperation::Cancellation { id } = operation { return gateway.cancellation(&project, id).await; }
                let actor = AgentIdentity::assigned(cli.agent.as_deref(), &project_dir)?;
                if actor.project_directory != amux::project_identity::resolve(&project_dir)?.directory { bail!("task mutation project directory differs from this agent's launch; switch to its project directory"); }
                let result = match operation {
                    TaskOperation::Create { title, task_type, parent, root_reason, specification, review_finding, os, notify_results, to_agent, details } => {
                        gateway.require_task_hierarchy().await?;
                        let mut body = json!({"title": title, "task_type":task_type, "parent_task_id":parent, "specification": specification});
                        if let Some(reason) = root_reason { body["root_reason"] = json!(reason); }
                        let mut labels = Vec::new();
                        if review_finding { labels.push("autoreview-finding".to_owned()); }
                        if review_finding && to_agent.is_none() {
                            body["routing"] = json!({"mode":"unassigned"});
                            body["canonical_remote"] = json!(amux::gateway_project::configured_origin(&actor.project_directory)?);
                        }
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
                        anyhow::ensure!(task["task_type"] == task_type, "Task type unconfirmed; inspect task and update the server before retrying");
                        if let Some(parent) = parent {
                            anyhow::ensure!(task["parent"]["id"] == parent.to_string() && task["parent"]["project_ident"] == project,
                                "Parent link unconfirmed; inspect the task before retrying. The gateway may need updating.");
                        } else {
                            anyhow::ensure!(task["parent"].is_null(), "Root creation unconfirmed; inspect the task before retrying");
                        }
                        if review_finding && to_agent.is_none() {
                            anyhow::ensure!(task["status"] == "todo" && task["owner_origin"].is_null() && task["owner_agent_id"].is_null(), "Finding was not left unowned in the queue; inspect task before retrying");
                        }
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
                    TaskOperation::Delegate { title, task_type, parent, target_project, specification, notify_results, details } => {
                        gateway.require_task_hierarchy().await?;
                        let requester = if notify_results { Some(gateway.requester_target(&project, &actor).await?) } else { None };
                        let mut details = details.into_details();
                        details.task_type = Some(task_type);
                        gateway.delegate_with_requester(&project, &target_project, parent, title, specification, &details, &actor, requester.as_ref()).await
                    },
                    TaskOperation::Rank { id, rank } => gateway.rank(&project, id, rank, &actor).await,
                    TaskOperation::Claim { id } => {
                        let task = gateway.task(&project, id, 0).await?;
                        anyhow::ensure!(amux::task_platform::matches(&task, Some(&actor.os)), "Task requires another operating system");
                        gateway.claim(&project, &id.to_string(), &actor).await
                    },
                    TaskOperation::Reassign { id, to_agent } => gateway.reassign_agent_task(&project, id, to_agent, &actor).await,
                    TaskOperation::Watch { id, clear } => {
                        let requester = if clear { None } else { Some(gateway.requester_target(&project, &actor).await?) };
                        gateway.set_task_requester(&project, id, requester.as_ref(), Some(&actor)).await
                    },
                    TaskOperation::SetType { id, task_type } => gateway.set_task_type(&project, id, &actor, &task_type).await,
                    TaskOperation::SetOs { id, os, .. } => gateway.set_task_os(&project, id, os, &actor).await,
                    TaskOperation::UserReview { id, action } => gateway.request_user_review(&project, id, &actor, &action).await,
                    TaskOperation::Cancel { id, reason } => gateway.cancel_task(&project, id, &reason, &actor).await,
                    TaskOperation::Comment { id, body } => gateway.request(Method::POST, &project, &[&id.to_string(), "comments"], Some(json!({"content":body})), Some(&actor)).await,
                    TaskOperation::Started { id } => {
                        gateway.confirm_owner(&project, id, &actor).await?;
                        gateway.request(Method::POST, &project, &[&id.to_string(), "comments"], Some(json!({"content":format!("Agent reports execution started: session={} instance={}; recorded as a comment, not an atomic gateway lifecycle transition", actor.session_id, actor.instance_id)})), Some(&actor)).await
                    },
                    TaskOperation::SubmitWork(work) => {
                        let task = gateway.confirm_owner(&project, work.task, &actor).await.context("work was not submitted")?;
                        anyhow::ensure!(task["task_type"].as_str() != Some("general"), "Convert this General task with amux tasks set-type {} software before submitting code or configuration changes", work.task);
                        let selected = match work.paths_file {
                            Some(file) => serde_json::from_reader(std::fs::File::open(&file).with_context(|| format!("cannot read path list {}", file.display()))?).context("path list must be a JSON array of file paths")?,
                            None => work.paths,
                        };
                        let paths = selected.iter().map(std::path::absolute).collect::<std::io::Result<Vec<_>>>()?;
                        let request = amux::work_submission::Request { root: &actor.project_directory, task_id: work.task, message: &work.message, paths: &paths };
                        let outcome = match work.resume_commit {
                            Some(commit) => amux::work_submission::resume(&request, &preferences.work_submission, &commit, amux::work_submission::open_pull_request)?,
                            None => amux::work_submission::submit(&request, &preferences.work_submission, amux::work_submission::open_pull_request)?,
                        };
                        let comment = gateway.request(Method::POST, &project, &[&work.task.to_string(), "comments"], Some(json!({"content": outcome.comment()})), Some(&actor)).await
                            .with_context(|| format!("work was submitted as {}, but posting the result on the task failed", outcome.reference))?;
                        Ok(json!({"task_id": work.task, "reference": outcome.reference, "files": outcome.files, "comment_id": comment["id"]}))
                    },
                    TaskOperation::Done { id } | TaskOperation::Release { id } => {
                        gateway.confirm_owner(&project, id, &actor).await?;
                        let status = if matches!(operation, TaskOperation::Done { .. }) { "done" } else { "todo" };
                        gateway.request(Method::PATCH, &project, &[&id.to_string()], Some(json!({"status":status})), Some(&actor)).await
                    },
                    _ => unreachable!(),
                }?;
                // CLI/MCP claims, creation and conversion share the desktop's
                // durable memory policy, including tasks claimed without an offer.
                if result["owner_origin"]["session_id"] == actor.session_id.to_string()
                    && result["owner_origin"]["instance_id"] == actor.instance_id.to_string()
                    && let (Some(id), Some(task_type)) = (result["id"].as_str().and_then(|id| Uuid::parse_str(id).ok()), result["task_type"].as_str())
                {
                    let guidance = identity::state_directory().and_then(|registry| {
                        amux::guidance::with_session(&registry, actor.session_id, |state| {
                            state.activate(actor.instance_id);
                            state.assign_typed(id, task_type);
                        })
                    });
                    if let Err(error) = guidance { eprintln!("Task updated; idle memory guidance could not be saved: {error:#}"); }
                }
                Ok(result)
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

/// The enrolled gateway, the local server it needs in Server mode, and the
/// saved preferences.
async fn cli_gateway(
    url: Option<String>,
) -> Result<(
    Gateway,
    Option<amux::server_controller::LocalServer>,
    amux::gateway_preferences::GatewayPreferences,
)> {
    use amux::gateway_preferences::{GatewayMode, GatewayPreferences};
    let state = identity::state_directory()?;
    let path = state.join("gateway-preferences.json");
    let mut config =
        GatewayPreferences::load(&path)?.unwrap_or_else(GatewayPreferences::from_environment);
    // URL overrides may select only the already-enrolled authority. Pairing
    // supplies trust and identity; a URL or environment key cannot replace it.
    if let Some(url) = url {
        let selected = config
            .resolved_url()
            .context("Pair with the server in Settings first")?;
        anyhow::ensure!(
            amux::gateway_transport::origin(&selected, true)?
                == amux::gateway_transport::origin(&url, true)?,
            "Server URL differs from the enrolled connection; pair with that server in Settings first"
        );
    }
    let server = if config.mode == GatewayMode::Server {
        let server = amux::server_controller::ensure_local(state, config.server.clone()).await?;
        config.enrolled = Some(server.connection());
        config.save(&path)?;
        Some(server)
    } else {
        None
    };
    Ok((config.gateway()?, server, config))
}

/// Only the exact owning execution may report start, complete or release a task.
/// This is a client preflight read, not atomic server authorization. The gateway
/// already guards bound owner_origin claims, but until it enforces the exact
/// owner atomically for every claim type, ownership can change between this read
/// and the mutation.
fn launch_agent(project: PathBuf, provider: Provider, mcp: bool, args: Vec<String>) -> Result<u8> {
    let actor = AgentIdentity::new(provider, &project)?.resume_at(
        &identity::state_directory()?,
        std::time::Duration::from_secs(5),
    );
    let restoring = args
        .iter()
        .map(String::as_str)
        .eq(amux::session::resume_arguments(provider).iter().copied());
    let begin_cycle = || {
        let result = identity::state_directory().and_then(|registry| {
            amux::guidance::with_session(&registry, actor.session_id, |state| {
                state.activate(actor.instance_id);
                state.new_session();
            })
        });
        if let Err(error) = result {
            eprintln!("amux: session guidance unavailable: {error:#}");
        }
    };
    if !amux::session::is_resume(provider, &args) {
        begin_cycle();
    }
    let mut status = launch_agent_once(&project, provider, mcp, &args, &actor)?;
    // Missing native conversation history exits with 1. Retry only the standard
    // restore invocation, once, leaving custom arguments and signals alone.
    if restoring && status.code() == Some(1) {
        eprintln!(
            "Unable to resume {}; starting a new session.",
            provider.command()
        );
        begin_cycle();
        status = launch_agent_once(&project, provider, mcp, &[], &actor)?;
    }
    Ok(status.code().unwrap_or(1).clamp(0, 255) as u8)
}

fn launch_agent_once(
    project: &std::path::Path,
    provider: Provider,
    mcp: bool,
    args: &[String],
    actor: &AgentIdentity,
) -> Result<std::process::ExitStatus> {
    let directory = amux::workspace_launch::external_path(project.canonicalize()?);
    if mcp
        && std::env::var_os("AMUX_BUNDLED_DIR").is_none()
        && let Err(error) = amux::updates::bootstrap_bundled_tools()
    {
        eprintln!(
            "amux: bundled tools could not be restored: {error:#}. Retry with amux --update."
        );
    }
    // Codex reads `-c` overrides before its subcommand; Claude's --mcp-config
    // takes several values, so it goes last where it cannot swallow a prompt.
    let mut registration = if mcp {
        amux::mcp::launch_arguments(provider, &std::env::current_exe()?, &actor.environment())
    } else {
        Vec::new()
    };
    if mcp
        && matches!(provider, Provider::Codex)
        && std::env::var_os("AMUX_DESKTOP_GUIDANCE").is_none()
    {
        registration.extend(amux::mcp::codex_context_arguments(&directory));
    }
    let mut args = args.to_vec();
    if matches!(provider, Provider::Codex) {
        // Keep the server owned by this terminal instead of a shared daemon.
        if !args.iter().any(|arg| arg == "--no-daemon") {
            registration.push("--no-daemon".into());
        }
        args.splice(0..0, registration);
    } else {
        args.extend(registration);
    }
    let args = args.as_slice();
    let mut command = Command::new(provider.command());
    command
        .args(args)
        .current_dir(&directory)
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
    let spawned = command.spawn();
    #[cfg(target_os = "macos")]
    let spawned = match spawned {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            let shell = std::env::var_os("SHELL")
                .filter(|shell| !shell.is_empty())
                .map(PathBuf::from)
                .unwrap_or_else(|| PathBuf::from("/bin/zsh"));
            let bin = std::env::current_exe()?
                .parent()
                .context("amux executable has no parent directory")?
                .to_path_buf();
            macos_agent_command(&shell, &bin, &directory, provider)
                .args(args)
                .current_dir(&directory)
                .envs(actor.environment())
                .spawn()
        }
        result => result,
    };
    let mut child = spawned.with_context(|| {
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

/// Apply the user's profile in the agent's PTY, rather than relying on the
/// Finder/Dock environment or the desktop's best-effort startup PATH probe.
#[cfg(target_os = "macos")]
fn macos_agent_command(
    shell: &std::path::Path,
    bin: &std::path::Path,
    directory: &std::path::Path,
    provider: Provider,
) -> Command {
    let mut command = Command::new(shell);
    if shell.file_name() == Some(std::ffi::OsStr::new("fish")) {
        command.args([
            "-ilc",
            "set -gx PATH $argv[1] $PATH; cd -- $argv[2]; or exit; set -e argv[1..2]; exec $argv",
        ]);
    } else {
        command.args([
            "-ilc",
            "export PATH=\"$1:$PATH\"; cd -- \"$2\" || exit; shift 2; exec \"$@\"",
            "amux-agent",
        ]);
    }
    // Paths and provider arguments are positional values, never shell source.
    command.arg(bin).arg(directory).arg(provider.command());
    command
}
