pub mod backlog;
pub mod cli_path;
pub mod delivery;
pub mod dispatch;
pub mod gateway;
pub mod gateway_preferences;
pub mod gateway_project;
pub mod identity;
pub mod notifications;
#[cfg(feature = "desktop")]
pub mod readiness;
pub mod recipient;
pub mod session;
#[cfg(feature = "desktop")]
pub mod submission;
pub mod task_management;
pub mod task_markdown;
pub mod task_platform;
pub mod task_requester;
pub mod task_routing;

#[cfg(target_os = "linux")]
pub mod terminal_lifetime;
#[cfg(windows)]
#[path = "terminal_lifetime_windows.rs"]
pub mod terminal_lifetime;

pub mod updates;

pub mod workspace_launch;

pub mod gateway_stream;

pub mod delegation;

pub mod delegation_process;

pub mod delegation_queue;

#[cfg(feature = "desktop")]
pub mod delegation_readiness;

pub mod delegation_connection;

pub mod autoreview;
pub mod project_identity;
