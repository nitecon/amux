pub mod delivery;
pub mod gateway;
pub mod identity;
#[cfg(feature = "desktop")]
pub mod readiness;
pub mod recipient;
pub mod session;
#[cfg(feature = "desktop")]
pub mod submission;

#[cfg(target_os = "linux")]
pub mod terminal_lifetime;
#[cfg(windows)]
#[path = "terminal_lifetime_windows.rs"]
pub mod terminal_lifetime;
