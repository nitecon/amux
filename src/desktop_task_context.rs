//! Task state belongs to a selected project, independently of local terminals.
use super::*;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(super) enum TaskWorkspace {
    Local(usize),
    Remote(usize),
}

pub(super) struct RemoteWorkspace {
    pub id: Uuid,
    pub canonical_remote: Option<String>,
    pub gateway: TaskPanel,
}

impl Desktop {
    pub(super) fn task_workspace(&self) -> TaskWorkspace {
        self.remote_active
            .filter(|index| *index < self.remote_workspaces.len())
            .map(TaskWorkspace::Remote)
            .unwrap_or(TaskWorkspace::Local(self.active))
    }

    pub(super) fn task_panel(&self, target: TaskWorkspace) -> &TaskPanel {
        match target {
            TaskWorkspace::Local(index) => &self.workspaces[index].gateway,
            TaskWorkspace::Remote(index) => &self.remote_workspaces[index].gateway,
        }
    }

    pub(super) fn task_panel_mut(&mut self, target: TaskWorkspace) -> &mut TaskPanel {
        match target {
            TaskWorkspace::Local(index) => &mut self.workspaces[index].gateway,
            TaskWorkspace::Remote(index) => &mut self.remote_workspaces[index].gateway,
        }
    }

    pub(super) fn task_contexts(&self) -> impl Iterator<Item = (TaskWorkspace, &TaskPanel)> {
        self.workspaces
            .iter()
            .enumerate()
            .map(|(index, workspace)| (TaskWorkspace::Local(index), &workspace.gateway))
            .chain(
                self.remote_workspaces
                    .iter()
                    .enumerate()
                    .map(|(index, workspace)| (TaskWorkspace::Remote(index), &workspace.gateway)),
            )
    }

    pub(super) fn remote_binding_current(&self, target: TaskWorkspace) -> bool {
        match target {
            TaskWorkspace::Local(_) => true,
            TaskWorkspace::Remote(index) => {
                self.remote_workspaces.get(index).is_some_and(|remote| {
                    self.design.projects.iter().any(|project| {
                        project.ident == remote.gateway.project
                            && project.canonical_remote == remote.canonical_remote
                    })
                })
            }
        }
    }

    pub(super) fn task_view_tick(&mut self) -> Task<Message> {
        let context = self.task_workspace();
        if !self.tasks_visible() && self.design.task_window.is_none() {
            return Task::none();
        }
        if self
            .task_panel(context)
            .queried_at
            .is_some_and(|at| at.elapsed() < Duration::from_secs(15))
        {
            return Task::none();
        }
        self.query_tasks(context)
    }

    pub(super) fn reset_remote_projects(&mut self) {
        self.remote_active = None;
        for remote in &mut self.remote_workspaces {
            let panel = &mut remote.gateway;
            panel.revision += 1;
            panel.detail = Default::default();
            panel.routing = Default::default();
            panel.loading = false;
            panel.refresh_pending = false;
            panel.loaded_at = None;
            panel.queried_at = None;
            panel.tasks.clear();
            panel.status = "Select this project from the connected gateway to refresh.".into();
        }
    }

    pub(super) fn task_scope(&self, target: TaskWorkspace) -> QueryScope {
        let (workspace_id, directory, canonical_remote) = match target {
            TaskWorkspace::Local(index) => (
                self.workspaces[index].id,
                Some(self.workspaces[index].directory.clone()),
                None,
            ),
            TaskWorkspace::Remote(index) => (
                self.remote_workspaces[index].id,
                None,
                self.remote_workspaces[index].canonical_remote.clone(),
            ),
        };
        let panel = self.task_panel(target);
        QueryScope {
            workspace: target,
            workspace_id,
            directory,
            canonical_remote,
            project: panel.project.trim().to_owned(),
            revision: panel.revision,
            epoch: self.delegation.epoch,
        }
    }

    pub(super) fn retarget_task_scope(&self, mut scope: QueryScope) -> Option<QueryScope> {
        scope.workspace = match scope.workspace {
            TaskWorkspace::Local(_) => TaskWorkspace::Local(
                self.workspaces
                    .iter()
                    .position(|workspace| workspace.id == scope.workspace_id)?,
            ),
            TaskWorkspace::Remote(_) => TaskWorkspace::Remote(
                self.remote_workspaces
                    .iter()
                    .position(|workspace| workspace.id == scope.workspace_id)?,
            ),
        };
        Some(scope)
    }

    pub(super) fn select_remote_project(&mut self, project: String) -> Task<Message> {
        let Some(project) = self
            .design
            .projects
            .iter()
            .find(|item| item.ident == project)
            .cloned()
        else {
            return Task::none();
        };
        let index = match self
            .remote_workspaces
            .iter()
            .position(|workspace| workspace.gateway.project == project.ident)
        {
            Some(index) => {
                if self.remote_workspaces[index].canonical_remote != project.canonical_remote {
                    if self.remote_workspaces[index].gateway.unresolved() {
                        self.status = "Inspect the pending task result before changing this project's repository binding.".into();
                        return Task::none();
                    }
                    let remote = &mut self.remote_workspaces[index];
                    remote.canonical_remote = project.canonical_remote;
                    remote.gateway.revision += 1;
                    remote.gateway.tasks.clear();
                    remote.gateway.detail = Default::default();
                    remote.gateway.routing = Default::default();
                    remote.gateway.loading = false;
                }
                index
            }
            None => {
                self.remote_workspaces.push(RemoteWorkspace {
                    id: Uuid::new_v4(),
                    canonical_remote: project.canonical_remote,
                    gateway: TaskPanel {
                        project: project.ident,
                        ..Default::default()
                    },
                });
                self.remote_workspaces.len() - 1
            }
        };
        self.remote_active = Some(index);
        self.show_tasks();
        self.close_design_popovers();
        Task::batch([
            self.query_tasks(TaskWorkspace::Remote(index)),
            self.refresh_task_targets(),
        ])
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn inventory(app: &mut Desktop) {
        app.design.projects = ["remote-a", "remote-b"]
            .into_iter()
            .map(|ident| amux::gateway::ProjectOverview {
                ident: ident.into(),
                canonical_remote: Some(format!("example.org/team/{ident}")),
                ..Default::default()
            })
            .collect();
    }

    #[test]
    fn remote_selection_uses_inventory_without_creating_a_local_executor() {
        let mut app = super::super::tests::desktop();
        inventory(&mut app);
        let local_ids: Vec<_> = app.workspaces.iter().map(|w| w.id).collect();
        let next_terminal = app.next_id;
        drop(app.select_remote_project("unknown".into()));
        assert!(app.remote_active.is_none());
        assert!(app.remote_workspaces.is_empty());
        drop(app.select_remote_project("remote-a".into()));
        let scope = app.task_scope(app.task_workspace());
        assert_eq!(scope.project, "remote-a");
        assert!(scope.directory.is_none());
        assert_eq!(
            scope.canonical_remote.as_deref(),
            Some("example.org/team/remote-a")
        );
        assert!(app.tasks_visible());
        assert!(app.scope_current(&scope));
        drop(app.design_view());
        drop(app.update(Message::NewTab(Some(Provider::Codex))));
        drop(app.update(Message::Split(pane_grid::Axis::Vertical)));
        drop(app.design_message(desktop_design::Action::View(desktop_design::View::Files)));
        assert_eq!(app.next_id, next_terminal);
        assert_eq!(
            app.workspaces.iter().map(|w| w.id).collect::<Vec<_>>(),
            local_ids
        );
        assert!(app.tasks_visible());
        drop(app.update(Message::SelectWorkspace(1)));
        assert_eq!(app.task_workspace(), TaskWorkspace::Local(1));
        assert!(app.tasks_visible());
    }

    #[test]
    fn remote_drafts_and_late_receipts_stay_with_the_originating_project() {
        let mut app = super::super::tests::desktop();
        inventory(&mut app);
        drop(app.select_remote_project("remote-a".into()));
        let scope = app.task_scope(app.task_workspace());
        let request = Uuid::new_v4();
        let accepted = Uuid::new_v4();
        let panel = app.task_panel_mut(scope.workspace);
        panel.draft_title = "Preserve A".into();
        panel.request = Some(TaskRequest {
            id: request,
            project: scope.project.clone(),
            title: "Preserve A".into(),
            state: TaskRequestState::Sending,
        });
        drop(app.select_remote_project("remote-b".into()));
        app.task_panel_mut(app.task_workspace()).draft_title = "Preserve B".into();
        drop(app.update(Message::TaskCreated(scope.clone(), request, Ok(accepted))));
        assert_eq!(
            app.task_panel(app.task_workspace()).draft_title,
            "Preserve B"
        );
        assert!(app.task_panel(app.task_workspace()).request.is_none());
        assert!(
            matches!(app.task_panel(scope.workspace).request.as_ref().unwrap().state,
            TaskRequestState::Accepted(id) if id == accepted)
        );
        drop(app.select_remote_project("remote-a".into()));
        assert_eq!(app.remote_workspaces.len(), 2);
        assert_eq!(
            app.task_panel(app.task_workspace()).draft_title,
            "Preserve A"
        );
        assert!(app.workspaces.iter().all(|w| w.gateway.request.is_none()));
    }

    #[test]
    fn inventory_and_connection_changes_fence_remote_responses() {
        let mut app = super::super::tests::desktop();
        inventory(&mut app);
        drop(app.select_remote_project("remote-a".into()));
        let scope = app.task_scope(app.task_workspace());
        app.design.projects[0].canonical_remote = Some("different/repository".into());
        assert!(!app.remote_binding_current(scope.workspace));
        // Already-issued operations may settle until selection explicitly rebinds the context.
        assert!(app.scope_current(&scope));
        drop(app.select_remote_project("remote-a".into()));
        assert!(!app.scope_current(&scope));
        let rebound = app.task_scope(app.task_workspace());
        assert!(app.scope_current(&rebound));
        app.reset_delegation();
        assert!(!app.scope_current(&rebound));
        assert!(app.remote_active.is_none());
        assert_eq!(app.remote_workspaces[0].gateway.project, "remote-a");
    }
}
