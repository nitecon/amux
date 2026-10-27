//! Built-in event sources and plugins registered with the event router.
pub mod clock;

use crate::router::{EventDefinition, Plugin};

pub fn events() -> Vec<EventDefinition> {
    vec![clock::definition()]
}

pub fn plugins() -> Vec<Box<dyn Plugin>> {
    Vec::new()
}
