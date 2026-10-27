//! Small, dismissible overlays; status messages never change terminal layout.
use std::time::{Duration, Instant};

pub(super) struct Toast {
    pub id: u64,
    pub key: &'static str,
    pub title: String,
    pub body: String,
    expires: Instant,
}

#[derive(Default)]
pub(super) struct State {
    pub items: Vec<Toast>,
    next: u64,
    offline: bool,
}

impl State {
    pub fn show(&mut self, key: &'static str, title: &str, body: &str) {
        self.items.retain(|toast| toast.key != key);
        self.next += 1;
        self.items.push(Toast {
            id: self.next,
            key,
            title: title.into(),
            body: body.chars().take(500).collect(),
            expires: Instant::now() + Duration::from_secs(10),
        });
        if self.items.len() > 3 {
            self.items.remove(0);
        }
    }
    pub fn dismiss(&mut self, id: u64) {
        self.items.retain(|toast| toast.id != id);
    }
    pub fn expire(&mut self) {
        self.items.retain(|toast| toast.expires > Instant::now());
    }
    pub fn connection_lost(&mut self) {
        if !self.offline {
            self.offline = true;
            self.show(
                "connection",
                "Gateway connection lost",
                "Reconnecting automatically when the network is available.",
            );
        }
    }
    pub fn connection_restored(&mut self) {
        if self.offline {
            self.offline = false;
            self.show(
                "connection",
                "Gateway connection restored",
                "Connected again. Refreshing projects, task lists and agent availability.",
            );
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn outages_notify_once_and_recovery_replaces_the_notice() {
        let mut state = State::default();
        state.connection_restored();
        assert!(state.items.is_empty());
        state.connection_lost();
        let first = state.items[0].id;
        state.connection_lost();
        assert_eq!(state.items[0].id, first);
        state.dismiss(first);
        state.connection_lost();
        assert!(
            state.items.is_empty(),
            "retry ticks cannot resurrect a dismissed outage notice"
        );
        state.connection_restored();
        assert_eq!(state.items.len(), 1);
        assert!(state.items[0].title.contains("restored"));
        state.items[0].expires = Instant::now();
        state.expire();
        assert!(state.items.is_empty());
    }
}
