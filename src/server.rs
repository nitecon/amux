//! Native server hosting and local database operator commands.
use amux_gateway_backend::{db, runtime, task_dispatch, ui_auth};
use anyhow::{Context, Result};
use clap::Subcommand;

#[derive(Subcommand)]
pub enum Command {
    /// Host native mTLS APIs and public discovery using a separate security profile.
    Secure {
        #[arg(long)]
        profile: std::path::PathBuf,
        #[arg(long, default_value = "0.0.0.0:24182")]
        bind: std::net::SocketAddr,
        #[arg(long, default_value = "0.0.0.0:24181")]
        discovery_bind: std::net::SocketAddr,
        #[arg(long = "name")]
        names: Vec<String>,
        #[arg(long)]
        instance_id: String,
        #[arg(long)]
        webhook_bind: Option<std::net::SocketAddr>,
        #[arg(long, hide = true)]
        parent_lease: Option<std::path::PathBuf>,
    },
    /// Print a new short-lived peering key from the running local server.
    PeeringKey {
        #[arg(long)]
        profile: Option<std::path::PathBuf>,
        /// Print the active key without replacing it or extending its lifetime.
        #[arg(long)]
        copy: bool,
    },
    /// Provision or revoke project-scoped dispatch machine credentials.
    TaskMachine {
        #[command(subcommand)]
        action: MachineCommand,
    },
    /// Inspect or recover orphaned task ownership against the local database.
    Task {
        #[command(subcommand)]
        action: TaskCommand,
    },
    /// Manage backend user accounts without the browser frontend.
    User {
        #[command(subcommand)]
        action: UserCommand,
    },
}

#[derive(Subcommand)]
pub enum MachineCommand {
    /// Issue a new secret, invalidating the previous key and session leases.
    Issue {
        project: String,
        instance_id: String,
        os: String,
    },
    Revoke {
        project: String,
        instance_id: String,
    },
}

#[derive(Subcommand)]
pub enum TaskCommand {
    ClaimInfo {
        project: String,
        task_id: String,
    },
    Recover {
        project: String,
        task_id: String,
        #[arg(long)]
        expected_claim: String,
        #[arg(long)]
        reason: String,
    },
}

#[derive(Subcommand)]
pub enum UserCommand {
    Add {
        username: String,
        #[arg(long)]
        display_name: Option<String>,
        #[arg(long)]
        admin: bool,
    },
    ResetPassword {
        username: String,
    },
    List,
}

pub fn run(command: Command) -> Result<()> {
    let _ = tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .try_init();
    match command {
        Command::Secure {
            profile,
            bind,
            discovery_bind,
            names,
            instance_id,
            webhook_bind,
            parent_lease,
        } => {
            uuid::Uuid::parse_str(&instance_id)?;
            let mut config = runtime::RuntimeConfig::from_env_secure()?;
            config.database = db::DatabaseConfig::sqlite(
                profile
                    .join("application.sqlite")
                    .to_str()
                    .context("profile path is not UTF8")?,
            );
            let _profile_lock = profile_lock(&config.database)?;
            tokio::runtime::Runtime::new()?.block_on(amux_gateway_backend::secure::serve(
                config,
                amux_gateway_backend::secure::SecureOptions {
                    profile,
                    bind,
                    discovery_bind,
                    names,
                    instance_id,
                    webhook_bind,
                    parent_lease,
                },
            ))
        }
        Command::PeeringKey { profile, copy } => {
            let directory = match profile {
                Some(path) => path,
                None => crate::identity::state_directory()?.join("server"),
            };
            let path = amux_gateway_backend::security::local_profile_path(&directory);
            let local = amux_gateway_backend::security::LocalProfile::load(&path)
                .context("No local server credentials; start amux server secure first")?;
            let connection = crate::gateway_preferences::EnrolledConnection {
                url: local.url,
                cluster_id: local.cluster_id,
                node_id: local.node_id,
                profile_path: path,
            };
            let invitation = tokio::runtime::Runtime::new()?
                .block_on(crate::server_controller::invitation(connection, !copy))?;
            println!("{}", invitation.invitation);
            Ok(())
        }
        Command::TaskMachine { action } => {
            let database = db::open_config(&operator_database()?)?;
            let conn = database.lock().unwrap();
            match action {
                MachineCommand::Issue {
                    project,
                    instance_id,
                    os,
                } => println!(
                    "{}",
                    task_dispatch::provision(&conn, &project, &instance_id, &os)?
                ),
                MachineCommand::Revoke {
                    project,
                    instance_id,
                } => task_dispatch::revoke(&conn, &project, &instance_id)?,
            }
            Ok(())
        }
        Command::Task { action } => {
            let database = db::open_config(&operator_database()?)?;
            let conn = database.lock().unwrap();
            let task = match action {
                TaskCommand::ClaimInfo { project, task_id } => {
                    db::get_task_detail(&conn, &project, &task_id)?
                        .context("task not found in project")?
                        .task
                }
                TaskCommand::Recover {
                    project,
                    task_id,
                    expected_claim,
                    reason,
                } => db::recover_task_claim(&conn, &project, &task_id, &expected_claim, &reason)?,
            };
            // Preserve the existing local operator recovery contract.
            println!(
                "{}",
                serde_json::to_string_pretty(&serde_json::json!({
                    "id": task.id, "project_ident": task.project_ident, "status": task.status,
                    "owner_agent_id": task.owner_agent_id, "updated_at": task.updated_at,
                    "claim_token": db::task_claim_token(&task)?,
                }))?
            );
            Ok(())
        }
        Command::User { action } => user(action),
    }
}

/// Use the native application's database unless the operator explicitly
/// selects another database for recovery or administration.
fn operator_database() -> Result<db::DatabaseConfig> {
    let configured = db::DatabaseConfig::from_env()?;
    let explicit_location = ["DATABASE_PATH", "DATABASE_URL"]
        .iter()
        .any(|key| std::env::var(key).is_ok_and(|value| !value.trim().is_empty()));
    if configured.sqlite_path().is_some() && !explicit_location {
        let directory = crate::identity::state_directory()?.join("server");
        amux_gateway_backend::security::private_directory(&directory)?;
        let path = directory.join("application.sqlite");
        return Ok(db::DatabaseConfig::sqlite(
            path.to_str().context("database path is not UTF8")?,
        ));
    }
    Ok(configured)
}

pub(crate) fn profile_lock(config: &db::DatabaseConfig) -> Result<Option<std::fs::File>> {
    let Some(path) = config.sqlite_path().filter(|path| *path != ":memory:") else {
        return Ok(None);
    };
    let path = std::path::Path::new(path);
    if let Some(parent) = path
        .parent()
        .filter(|parent| !parent.as_os_str().is_empty())
    {
        std::fs::create_dir_all(parent)?;
    }
    // Existing aliases must share the same owner lock. For a fresh database,
    // resolve its parent after creating it so relative paths agree as well.
    let path = if path.exists() {
        path.canonicalize()?
    } else {
        path.parent()
            .filter(|parent| !parent.as_os_str().is_empty())
            .unwrap_or_else(|| std::path::Path::new("."))
            .canonicalize()?
            .join(path.file_name().context("database path has no filename")?)
    };
    let mut lock = path.as_os_str().to_owned();
    lock.push(".amux-server.lock");
    let file = std::fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(std::path::PathBuf::from(lock))?;
    file.try_lock().map_err(|error| {
        anyhow::anyhow!("Gateway database profile is already owned or cannot be locked: {error}")
    })?;
    Ok(Some(file))
}

fn user(action: UserCommand) -> Result<()> {
    let database = db::open_config(&operator_database()?)?;
    let conn = database.lock().unwrap();
    let password = || -> Result<String> {
        if let Ok(value) = std::env::var("GATEWAY_USER_PASSWORD") {
            return Ok(value);
        }
        eprint!("Password: ");
        let mut line = String::new();
        std::io::stdin()
            .read_line(&mut line)
            .context("read password from stdin")?;
        Ok(line.trim_end_matches(['\r', '\n']).to_owned())
    };
    match action {
        UserCommand::Add {
            username,
            display_name,
            admin,
        } => {
            let hash = ui_auth::hash_password(&password()?)?;
            let role = if admin {
                db::USER_ROLE_ADMIN
            } else {
                db::USER_ROLE_MEMBER
            };
            let user = db::create_user(
                &conn,
                &username,
                display_name.as_deref().unwrap_or(""),
                role,
                &hash,
            )?;
            println!(
                "created {} ({}) as {}",
                user.username, user.display_name, user.role
            );
        }
        UserCommand::ResetPassword { username } => {
            let user = db::get_user_by_username(&conn, &username)?
                .with_context(|| format!("no user named '{username}'"))?;
            let hash = ui_auth::hash_password(&password()?)?;
            db::set_user_password(&conn, user.id, &hash)?;
            println!(
                "password reset for {}; existing sessions signed out",
                user.username
            );
        }
        UserCommand::List => {
            for user in db::list_users(&conn)? {
                println!(
                    "{:<20} {:<8} {}{}",
                    user.username,
                    user.role,
                    user.display_name,
                    if user.is_active() { "" } else { "  (disabled)" }
                );
            }
        }
    }
    Ok(())
}
