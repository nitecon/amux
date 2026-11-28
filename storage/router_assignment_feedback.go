package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/identity/agent"
	projectid "github.com/nitecon/amux/identity/project"
)

// migrateRouterAssignmentFeedback is migration 30. Nullable additions preserve
// legacy offers while ensuring rows without captured identity fail closed.
func migrateRouterAssignmentFeedback(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS assignment_offers(
	 notify_uid TEXT PRIMARY KEY, task_id TEXT NOT NULL, project TEXT NOT NULL,
	 session_id TEXT NOT NULL, reporter_source TEXT
	);
	CREATE TABLE IF NOT EXISTS assignment_refusals(session_id TEXT PRIMARY KEY,output_at INTEGER NOT NULL);`); err != nil {
		return err
	}
	for _, col := range []struct{ name, def string }{{"reporter_source", "TEXT"}} {
		exists, err := sqliteTableHasColumn(ctx, tx, "assignment_offers", col.name)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE assignment_offers ADD COLUMN "+col.name+" "+col.def); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS amux_assignment_offer_claims(
	 notify_uid TEXT PRIMARY KEY REFERENCES assignment_offers(notify_uid) ON DELETE CASCADE,
	 origin_json TEXT NOT NULL,claim_updated_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_assignment_offers_project ON assignment_offers(project,task_id);
	CREATE TRIGGER IF NOT EXISTS assignment_offer_task_updated AFTER UPDATE ON tasks BEGIN
	 DELETE FROM assignment_offers WHERE task_id=OLD.id OR task_id=NEW.id;
	END;
	CREATE TRIGGER IF NOT EXISTS assignment_offer_task_deleted AFTER DELETE ON tasks BEGIN
	 DELETE FROM assignment_offers WHERE task_id=OLD.id;
	END;
	CREATE TRIGGER IF NOT EXISTS assignment_offer_commented AFTER INSERT ON task_comments BEGIN
	 DELETE FROM assignment_offers WHERE task_id=NEW.task_id;
	END;
	CREATE TRIGGER IF NOT EXISTS assignment_offer_comment_updated AFTER UPDATE ON task_comments BEGIN
	 DELETE FROM assignment_offers WHERE task_id=OLD.task_id OR task_id=NEW.task_id;
	END;
	CREATE TRIGGER IF NOT EXISTS assignment_offer_comment_deleted AFTER DELETE ON task_comments BEGIN
	 DELETE FROM assignment_offers WHERE task_id=OLD.task_id;
	END;
	INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(30);`)
	return err
}

// createRouterAssignmentOffer emits the notification and captures its callback
// authority in the caller's claim transaction.
func createRouterAssignmentOffer(ctx context.Context, tx *sql.Tx, registry *fism.Registry, cause fism.Event, task *Task, owner agent.Origin, reporterSource string, newTask bool) error {
	if registry == nil || task == nil || task.ID == "" || !validAssignmentCause(cause) || !validRouterSource(reporterSource) {
		return fmt.Errorf("invalid assignment offer context")
	}
	normalized, err := agent.NormalizeOrigin(&owner)
	if err != nil || normalized == nil {
		return fmt.Errorf("invalid assignment owner: %w", err)
	}
	project := projectid.NormalizeIdent(task.ProjectIdent)
	ownerJSON, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	review := hasLabel(task.Labels, "task-review")
	clearContext := false
	if review {
		rank, rankErr := reviewRank(ctx, tx, task, *normalized)
		if rankErr != nil {
			return rankErr
		}
		clearContext = rank != (reviewPreference{})
	}
	heading := "Queued task review"
	instructions := ""
	if !review {
		instructions = "This task is already claimed by your amux identity. Check current code/comments and platform requirements; implement if needed, or close it with a reason if obsolete." + userDecisionGuidance
	}
	if review {
		heading = "Queued review"
		if !clearContext {
			heading = "Queued independent review"
		}
		instructions = "This review task is assigned to your amux identity. Review the source task, comments and actual changes. Evaluate YAGNI, DRY and KISS, and report the outcome using the instructions in the review task specification."
	}
	payload, err := json.Marshal(map[string]any{
		"target": normalized.SessionID, "project": project, "mode": "assigned", "require_idle": true,
		"new_task": newTask, "clear_context": clearContext, "heading": heading, "instructions": instructions,
		"event": map[string]any{"kind": "task_created", "project_ident": project, "created_at": cause.EmittedAt, "task": task, "comment": nil, "origin": nil},
	})
	if err != nil {
		return err
	}
	emitted, err := emitRouterNotificationIn(ctx, tx, registry, "assignment", cause.UID, payload)
	if err != nil {
		return err
	}
	if !emitted.Stored && !emitted.Replayed {
		return fmt.Errorf("assignment notification has no durable subscribers")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO assignment_offers(notify_uid,task_id,project,session_id,reporter_source) VALUES(?,?,?,?,?)`, emitted.Event.UID, task.ID, project, normalized.SessionID, reporterSource); err != nil {
		return err
	}
	var reporterPrincipal any
	var reportSequence int64
	err = tx.QueryRowContext(ctx, `SELECT sequence FROM amux_presence_agents
		WHERE project_ident=? AND instance_id=? AND session_id=? AND source=? AND report_kind='native' AND state!='gone'`, project, normalized.InstanceID, normalized.SessionID, reporterSource).Scan(&reportSequence)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		principal, principalErr := nativeSourcePrincipal(ctx, tx, reporterSource, reportSequence)
		if principalErr != nil {
			return principalErr
		}
		if principal != "" {
			reporterPrincipal = principal
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO amux_assignment_offer_claims(notify_uid,origin_json,claim_updated_at,reporter_principal) VALUES(?,?,?,?)`, emitted.Event.UID, string(ownerJSON), task.UpdatedAt, reporterPrincipal)
	return err
}

// ReconcileRouterAssignmentFeedback consumes one captured notify.result. Any
// database error rolls back both the offer receipt and task compensation.
func (s *Store) ReconcileRouterAssignmentFeedback(ctx context.Context, event fism.Event) error {
	if event.ID != "agent.notify_result" || event.Version != 1 || !canonicalUUID(event.UID) || !validRouterSource(event.Source) {
		return fmt.Errorf("assignment feedback requires a valid agent.notify_result v1 event")
	}
	receipt, err := controllers.DecodeAssignmentFeedback(event.Payload)
	if err != nil {
		return err
	}
	if event.Correlation == nil || !canonicalUUID(*event.Correlation) {
		return nil
	}
	err = s.taskMutation(ctx, nil, func(tx *sql.Tx, _ *agent.Origin) error {
		enabled, err := pluginEnabled(ctx, tx, "assignment")
		if err != nil || !enabled {
			return err
		}
		var taskID, project, session string
		var source, originJSON sql.NullString
		var claimUpdated sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT o.task_id,o.project,o.session_id,o.reporter_source,c.origin_json,c.claim_updated_at
		 FROM assignment_offers o LEFT JOIN amux_assignment_offer_claims c ON c.notify_uid=o.notify_uid WHERE o.notify_uid=?`, *event.Correlation).Scan(&taskID, &project, &session, &source, &originJSON, &claimUpdated)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if session != receipt.Target || !source.Valid || source.String == "" || source.String != event.Source || !originJSON.Valid || !claimUpdated.Valid {
			return nil
		}
		if receipt.Status == "delivered" {
			_, err = tx.ExecContext(ctx, "DELETE FROM assignment_offers WHERE notify_uid=?", *event.Correlation)
			return err
		}
		var adopted bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM amux_assignment_runs WHERE task_id=?) OR
		 EXISTS(SELECT 1 FROM amux_conversations WHERE task_id=?)`, taskID, taskID).Scan(&adopted); err != nil {
			return err
		}
		if adopted {
			_, err = tx.ExecContext(ctx, "DELETE FROM assignment_offers WHERE notify_uid=?", *event.Correlation)
			return err
		} else {
			var ownerJSON sql.NullString
			var currentUpdated int64
			err = tx.QueryRowContext(ctx, `SELECT owner_origin_json,updated_at FROM tasks WHERE id=? AND project_ident=? AND status IN ('todo','in_progress')`, taskID, project).Scan(&ownerJSON, &currentUpdated)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if !ownerJSON.Valid || ownerJSON.String != originJSON.String || currentUpdated != claimUpdated.Int64 {
				return nil
			}
			var owner agent.Origin
			if err := json.Unmarshal([]byte(originJSON.String), &owner); err != nil {
				return err
			}
			normalizedOwner, normErr := agent.NormalizeOrigin(&owner)
			if normErr != nil {
				return normErr
			}
			if normalizedOwner == nil {
				return nil
			}
			owner = *normalizedOwner
			if owner.SessionID != session || owner.InstanceID == "" {
				return nil
			}
			presence, _, err := livePresenceSnapshot(ctx, tx, event.EmittedAt)
			if err != nil {
				return err
			}
			var lastOutput *int64
			for _, candidate := range presence {
				if candidate.Source == source.String && candidate.Origin == owner {
					lastOutput = candidate.LastActivityAt
					break
				}
			}
			if _, err = tx.ExecContext(ctx, "DELETE FROM assignment_offers WHERE notify_uid=?", *event.Correlation); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `UPDATE tasks SET status='todo',owner_agent_id=NULL,owner_origin_json=NULL,started_at=NULL,updated_at=?
			 WHERE id=? AND project_ident=? AND status IN ('todo','in_progress') AND owner_origin_json=? AND updated_at=?`, event.EmittedAt, taskID, project, originJSON.String, claimUpdated.Int64)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil
			}
			if lastOutput != nil {
				if _, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO assignment_refusals(session_id,output_at) VALUES(?,?)", session, *lastOutput); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return err
}

// reconcileRouterAssignmentRefusals clears suppression after the live output
// position changes (including an identity reset to nil) or the session leaves,
// and returns remaining refused sessions for shared claim selectors.
func reconcileRouterAssignmentRefusals(ctx context.Context, q queryer, presence []AgentPresence) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT session_id,output_at FROM assignment_refusals")
	if err != nil {
		return nil, err
	}
	type refusal struct {
		session string
		output  int64
	}
	var saved []refusal
	for rows.Next() {
		var r refusal
		if err := rows.Scan(&r.session, &r.output); err != nil {
			rows.Close()
			return nil, err
		}
		saved = append(saved, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	blocked := make(map[string]bool, len(saved))
	for _, r := range saved {
		var current *int64
		var seenAt int64
		present := false
		for _, agent := range presence {
			if agent.Origin.SessionID != r.session {
				continue
			}
			if !present || agent.SeenAt > seenAt {
				present, seenAt, current = true, agent.SeenAt, agent.LastActivityAt
			}
		}
		if !present || current == nil || *current != r.output {
			if _, err := q.ExecContext(ctx, "DELETE FROM assignment_refusals WHERE session_id=? AND output_at=?", r.session, r.output); err != nil {
				return nil, err
			}
			continue
		}
		blocked[r.session] = true
	}
	return blocked, nil
}
