//! Clock: the only source of time for plugins. It emits `clock.tick` at a
//! coarse interval; with no subscribers a tick is skipped and not stored.
use crate::{
    db,
    router::{self, Bus, EventDefinition},
};
use serde_json::json;
use std::time::Duration;

pub const TICK: &str = "clock.tick";
pub const INTERVAL: Duration = Duration::from_secs(10);

pub fn definition() -> EventDefinition {
    EventDefinition {
        id: TICK,
        version: 1,
        remote_emit: false,
        validate: router::object,
    }
}

pub fn spawn(bus: Bus) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(INTERVAL);
        interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            interval.tick().await;
            let payload = json!({ "at": db::now_ms() });
            if let Err(error) = bus.emit("clock", TICK, 1, payload, None).await {
                tracing::warn!("Clock tick failed: {error}");
            }
        }
    })
}
