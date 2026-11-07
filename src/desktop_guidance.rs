//! Feed contextual guidance through the same verified terminal delivery queue
//! as gateway notifications. New work always has priority over idle review.
use super::*;

impl Desktop {
    pub(super) fn guidance_tick(&mut self) {
        if self.preferences.open
            || self.workspace_dialog.open
            || self.delegation.events.delivering.is_some()
            || !self.delegation.events.pending.is_empty()
        {
            return;
        }
        let Some(registry) = self.registry.clone() else {
            return;
        };
        for candidate in self.delegation.candidates.clone() {
            let session = candidate.target.session;
            let attachment = format!(
                "{}:{}:{}",
                candidate.actor.instance_id, candidate.process.pid, candidate.process.generation
            );
            let Some(terminal_tab) = tab(self, candidate.target.terminal) else {
                continue;
            };
            let clears = terminal_tab.delegation.context_clears;
            let recorded_clears = terminal_tab.delegation.recorded_context_clears;
            let input_revision = terminal_tab.delegation.input_revision;
            let eligible = terminal_tab.delegation.input.is_empty()
                && terminal_tab
                    .terminal
                    .as_ref()
                    .and_then(Terminal::activity_elapsed)
                    .is_some_and(|elapsed| elapsed >= amux::session::IDLE_AFTER);
            if !eligible {
                continue;
            }
            let seen = self.delegation.events.guidance_seen.get(&session);
            let kind = if seen.is_none_or(|(old, _)| old != &attachment) {
                "session"
            } else if seen.is_some_and(|(_, old)| *old != clears) {
                "clear"
            } else {
                if self
                    .delegation
                    .events
                    .last_delivery
                    .get(&session)
                    .is_some_and(|at| at.elapsed() < amux::session::IDLE_AFTER)
                {
                    continue;
                }
                "review"
            };
            let reserved = amux::guidance::with_session(&registry, session, |state| {
                state.activate(candidate.actor.instance_id);
                if recorded_clears != clears {
                    state.clear();
                }
                if kind == "review" {
                    state.reserve_review()
                } else {
                    Some(state.cycle)
                }
            });
            let cycle = match reserved {
                Ok(Some(cycle)) => cycle,
                Ok(None) => continue,
                Err(error) => {
                    self.queue_diagnostic(format!(
                        "Session guidance unavailable for {session}: {error:#}"
                    ));
                    continue;
                }
            };
            self.delegation
                .events
                .guidance_seen
                .insert(session, (attachment, clears));
            if let Some(tab) = tab(self, candidate.target.terminal) {
                tab.delegation.recorded_context_clears = clears;
            }
            let mut event = json!({
                "id": "agent.notify", "uid": format!("guidance:{session}:{cycle}:{kind}"),
                "emitted_at": self.events_now(), "amux_guidance": kind,
                "payload": {"target": session.to_string(), "project": candidate.target.project, "require_idle": true},
                "receiver": {"session_id": session, "instance_id": candidate.actor.instance_id,
                    "generation": candidate.target.executor_generation().to_string(), "project": candidate.target.project},
            });
            event["amux_cycle"] = json!(cycle);
            event["amux_input_revision"] = json!(input_revision);
            self.delegation.events.pending.push_back(event);
            break;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::desktop::desktop_delegation::tests::{human, project, scan, spawn, wait_lines};

    #[test]
    fn idle_review_delivers_once_and_failed_dispatch_and_restart_do_not_retry() {
        let mut app = crate::desktop::tests::desktop();
        let agent = spawn(&mut app, 0, 0, Provider::Codex, "opaque");
        app.registry = Some(agent.directory.clone());
        project(&mut app, &agent, "alpha");
        scan(&mut app);
        assert_eq!(app.delegation.candidates.len(), 1);
        std::thread::sleep(amux::session::IDLE_AFTER + Duration::from_millis(100));

        // First restore guidance, then exercise the first idle review. Deliberately
        // decline startup delivery so this fixture does not start a model turn.
        app.guidance_tick();
        let startup = app.delegation.events.pending.pop_front().unwrap();
        assert_eq!(startup["amux_guidance"], "session");
        app.events_result(&startup, "skipped", "fixture skips initial guidance");
        app.guidance_tick();
        assert_eq!(
            app.delegation.events.pending.front().unwrap()["amux_guidance"],
            "review"
        );
        drop(app.events_deliver());
        let delivery = app.delegation.events.delivering.as_ref().unwrap();
        let token = delivery.token;
        assert!(verify(&delivery.candidate));
        drop(app.events_verified(token, true));
        assert!(
            app.events_cards()
                .iter()
                .any(|card| card.detail.contains("delivered")),
            "guidance outcomes: {:?}",
            app.events_cards()
                .iter()
                .map(|card| &card.detail)
                .collect::<Vec<_>>()
        );
        assert!(wait_lines(&agent, 1)[0].contains("idle memory review"));
        assert!(
            app.events_cards()
                .iter()
                .any(|card| card.detail.contains("delivered"))
        );
        // The review wakes the fixture. Becoming idle again must not re-arm it.
        std::thread::sleep(amux::session::IDLE_AFTER + Duration::from_millis(100));
        app.guidance_tick();
        assert!(app.delegation.events.pending.is_empty());
        app.events_stop();
        app.guidance_tick();
        assert!(app.delegation.events.pending.is_empty());

        // New cycle: a failed native verification consumes its review allowance.
        let session = app.delegation.candidates[0].target.session;
        amux::guidance::with_session(&agent.directory, session, |state| state.clear()).unwrap();
        app.guidance_tick();
        drop(app.events_deliver());
        let token = app.delegation.events.delivering.as_ref().unwrap().token;
        drop(app.events_verified(token, false));
        app.guidance_tick();
        assert!(app.delegation.events.pending.is_empty());
        assert_eq!(agent.lines().len(), 1);

        // A restarted desktop restores guidance but retains the consumed cycle.
        app.delegation.events = State::default();
        app.guidance_tick();
        let restored = app.delegation.events.pending.pop_front().unwrap();
        assert_eq!(restored["amux_guidance"], "session");
        app.events_result(&restored, "skipped", "fixture skips resumed guidance");
        app.guidance_tick();
        assert!(app.delegation.events.pending.is_empty());

        // Persist an explicit clear immediately, before another idle tick or
        // app restart can lose the operation.
        let before = amux::guidance::with_session(&agent.directory, session, |s| s.cycle).unwrap();
        human(&mut app, agent.terminal, b"/clear\r");
        assert_ne!(
            amux::guidance::with_session(&agent.directory, session, |s| s.cycle).unwrap(),
            before
        );
        assert!(
            amux::guidance::with_session(&agent.directory, session, |s| s.reserve_review())
                .unwrap()
                .is_some()
        );
        assert_eq!(wait_lines(&agent, 2)[1], "/clear");
    }
}
