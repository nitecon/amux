//! Built-in event sources and plugins registered with the event router.
pub mod clock;
pub mod remote;
pub mod tasks;

use crate::router::{EventDefinition, Plugin};

pub fn events() -> Vec<EventDefinition> {
    let mut events = vec![clock::definition()];
    events.extend(tasks::events());
    events.extend(remote::events());
    events
}

/// Events remote consumers may subscribe to.
fn forwarded() -> Vec<&'static str> {
    let mut forwarded: Vec<_> = tasks::events().iter().map(|event| event.id).collect();
    forwarded.push(crate::router::RETRYING);
    forwarded
}

pub fn plugins(remote: &remote::Remote) -> Vec<Box<dyn Plugin>> {
    vec![remote.plugin(forwarded())]
}
