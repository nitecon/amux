package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/projects"
)

var ErrProjectDeleted = errors.New("project was permanently deleted")
var ErrProjectDeleteInput = errors.New("project deletion requires its exact project identifier")

// DeleteProject permanently removes gateway data for one project. Foreign keys
// remain enabled and the transaction preserves tasks owned by other projects.
func (s *Store) DeleteProject(ctx context.Context, ident, confirmation string) (bool, error) {
	if ident == "" || ident == "__global__" || ident != projects.NormalizeIdent(ident) || confirmation != ident {
		return false, ErrProjectDeleteInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	project, err := getProject(ctx, tx, ident)
	if err != nil || project == nil {
		return false, err
	}

	statements := []string{
		// Preserve surviving project data while removing cross-project links.
		`UPDATE tasks SET kind=CASE WHEN kind='delegated' THEN 'normal' ELSE kind END,
			delegated_to_project_ident=NULL, delegated_to_task_id=NULL
			WHERE project_ident<>?1 AND delegated_to_project_ident=?1`,
		`UPDATE workflow_runs SET source_artifact_version_id=NULL WHERE source_artifact_version_id IN (
			SELECT v.artifact_version_id FROM artifact_versions v JOIN artifacts a USING(artifact_id) WHERE a.project_ident=?1)
			AND artifact_id NOT IN (SELECT artifact_id FROM artifacts WHERE project_ident=?1)`,
		`UPDATE artifact_versions SET created_via_workflow_run_id=NULL WHERE created_via_workflow_run_id IN (
			SELECT w.workflow_run_id FROM workflow_runs w JOIN artifacts a USING(artifact_id) WHERE a.project_ident=?1)`,
		`UPDATE artifact_comments SET resolved_by_workflow_run_id=NULL WHERE resolved_by_workflow_run_id IN (
			SELECT w.workflow_run_id FROM workflow_runs w JOIN artifacts a USING(artifact_id) WHERE a.project_ident=?1)`,
		`UPDATE artifact_contributions SET workflow_run_id=NULL WHERE workflow_run_id IN (
			SELECT w.workflow_run_id FROM workflow_runs w JOIN artifacts a USING(artifact_id) WHERE a.project_ident=?1)`,
		`WITH owned(kind,id) AS (
			SELECT 'artifact',artifact_id FROM artifacts WHERE project_ident=?1
			UNION ALL SELECT 'artifact_version',v.artifact_version_id FROM artifact_versions v JOIN artifacts a USING(artifact_id) WHERE a.project_ident=?1
			UNION ALL SELECT 'comment',c.comment_id FROM artifact_comments c JOIN artifacts a USING(artifact_id) WHERE a.project_ident=?1
			UNION ALL SELECT 'contribution',c.contribution_id FROM artifact_contributions c JOIN artifacts a USING(artifact_id) WHERE a.project_ident=?1
			UNION ALL SELECT 'chunk',c.chunk_id FROM artifact_chunks c JOIN artifacts a USING(artifact_id) WHERE a.project_ident=?1
			UNION ALL SELECT 'task',id FROM tasks WHERE project_ident=?1
			UNION ALL SELECT 'memory',id FROM gateway_memories WHERE project_ident=?1
		)
		DELETE FROM artifact_links WHERE
			(source_kind,source_id) IN (SELECT kind,id FROM owned) OR
			(target_kind,target_id) IN (SELECT kind,id FROM owned) OR
			source_version_id IN (SELECT id FROM owned WHERE kind='artifact_version') OR
			target_version_id IN (SELECT id FROM owned WHERE kind='artifact_version') OR
			created_via_workflow_run_id IN (SELECT w.workflow_run_id FROM workflow_runs w JOIN artifacts a USING(artifact_id) WHERE a.project_ident=?1)`,
		`DELETE FROM artifacts WHERE project_ident=?1`,
		`DELETE FROM task_delegations WHERE source_project_ident=?1 OR target_project_ident=?1`,
		`DELETE FROM amux_assignment_runs WHERE task_id IN (SELECT id FROM tasks WHERE project_ident=?1)
			OR conversation_id IN (SELECT id FROM amux_conversations WHERE project_ident=?1)`,
		`DELETE FROM amux_conversation_command_changes WHERE conversation_id IN (SELECT id FROM amux_conversations WHERE project_ident=?1)`,
		`DELETE FROM amux_conversation_commands WHERE conversation_id IN (SELECT id FROM amux_conversations WHERE project_ident=?1)`,
		`DELETE FROM amux_conversation_events WHERE conversation_id IN (SELECT id FROM amux_conversations WHERE project_ident=?1)`,
		`DELETE FROM amux_conversations WHERE project_ident=?1`,
		`DELETE FROM reminder_nudges WHERE task_id IN (SELECT id FROM tasks WHERE project_ident=?1)`,
		`DELETE FROM router_events WHERE json_valid(payload) AND
			(json_extract(payload,'$.project')=?1 OR json_extract(payload,'$.project_ident')=?1 OR
			json_extract(payload,'$.task.project_ident')=?1 OR
			COALESCE(json_extract(payload,'$.task.id'),json_extract(payload,'$.task_id'),json_extract(payload,'$.task'))
				IN (SELECT id FROM tasks WHERE project_ident=?1))`,
		`DELETE FROM tasks WHERE project_ident=?1`,
		`DELETE FROM messages WHERE project_ident=?1`,
		`DELETE FROM agents WHERE project_ident=?1`,
		`DELETE FROM agent_confirmations WHERE project_ident=?1`,
		`DELETE FROM cursors WHERE project_ident=?1`,
		`DELETE FROM amux_go_machine_checkouts WHERE project_ident=?1`,
		`DELETE FROM amux_presence_agents WHERE project_ident=?1`,
		`DELETE FROM task_target_presence WHERE project_ident=?1`,
		`DELETE FROM amux_native_delivery_receipts WHERE project=?1`,
		`DELETE FROM assignment_offers WHERE project=?1`,
		`DELETE FROM assignment_turns WHERE project=?1`,
		`DELETE FROM router_task_outbox WHERE project=?1`,
		`DELETE FROM task_event_deliveries WHERE event_id IN (SELECT id FROM task_events WHERE project_ident=?1)`,
		`DELETE FROM task_events WHERE project_ident=?1`,
		`DELETE FROM projects WHERE ident=?1`,
		`INSERT INTO amux_deleted_projects(ident) VALUES(?1) ON CONFLICT DO NOTHING`,
	}
	if _, err = tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); err != nil {
		return false, err
	}
	for _, table := range []string{
		"task_sessions", "task_machines", "execution_runs", "machine_checkouts",
		"task_dispatch_events", "task_dispatch_requests", "task_dispatches",
	} {
		_, exists, err := sqliteTableColumns(ctx, tx, table)
		if err != nil {
			return false, err
		}
		if exists {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE project_ident=?", ident); err != nil {
				return false, fmt.Errorf("erase legacy project data: %w", err)
			}
		}
	}
	for _, statement := range statements {
		if _, err = tx.ExecContext(ctx, statement, ident); err != nil {
			return false, fmt.Errorf("erase project data: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE artifact_links SET supersedes_link_id=NULL WHERE supersedes_link_id NOT IN (SELECT link_id FROM artifact_links)`); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("commit project deletion: %w", err)
	}
	_ = s.bus.Publish(eventbus.Event{Topic: eventbus.ProjectChanged})
	s.wakeTaskEvents()
	s.wakeConversations()
	return true, nil
}
