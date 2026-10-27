//! Built-in event sources and plugins registered with the event router.
pub mod assignment;
pub mod clock;
pub mod notifier;
pub mod notify;
pub mod presence;
pub mod remote;
pub mod tasks;

use crate::router::{EventDefinition, Plugin};
use rusqlite::Connection;

pub fn events() -> Vec<EventDefinition> {
    let mut events = vec![clock::definition()];
    events.extend(tasks::events());
    events.extend(remote::events());
    events.extend(presence::events());
    events.extend(notify::events());
    events
}

/// Events remote consumers may subscribe to.
fn forwarded() -> Vec<&'static str> {
    let mut forwarded: Vec<_> = tasks::events().iter().map(|event| event.id).collect();
    forwarded.extend(presence::events().iter().map(|event| event.id));
    forwarded.push(notify::NOTIFY);
    forwarded.push(crate::router::RETRYING);
    forwarded
}

pub fn plugins(remote: &remote::Remote) -> Vec<Box<dyn Plugin>> {
    vec![
        remote.plugin(forwarded()),
        Box::new(presence::Presence),
        Box::new(assignment::Assignment),
        Box::new(notifier::Notifier),
    ]
}

/// Plugin-owned tables.
pub fn initialize(conn: &Connection) -> anyhow::Result<()> {
    tasks::initialize(conn)?;
    remote::initialize(conn)?;
    presence::initialize(conn)?;
    assignment::initialize(conn)?;
    notifier::initialize(conn)?;
    Ok(())
}
