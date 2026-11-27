package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/nitecon/amux/router"
	"github.com/nitecon/amux/taskplatform"
)

// migrateAssignments adds the exact-session suppression recorded when an
// agent explicitly releases unchanged work. The migration is intentionally
// separate so the coordinator can run it as migration 9, after presence 8.
func migrateAssignments(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS assignment_releases(
	 task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	 session_id TEXT NOT NULL,
	 instance_id TEXT NOT NULL,
	 PRIMARY KEY(task_id,session_id,instance_id)
	);
	CREATE TRIGGER IF NOT EXISTS assignment_explicit_release AFTER UPDATE ON tasks
	WHEN NEW.status='todo' AND OLD.status IN ('todo','in_progress')
	 AND NEW.owner_origin_json IS NULL AND NEW.owner_agent_id IS NULL
	 AND json_extract(OLD.owner_origin_json,'$.session_id')=
	     (SELECT json_extract(origin_json,'$.session_id') FROM task_mutation_context WHERE id=1)
	 AND json_extract(OLD.owner_origin_json,'$.instance_id')=
	     (SELECT json_extract(origin_json,'$.instance_id') FROM task_mutation_context WHERE id=1)
	BEGIN
	 INSERT OR IGNORE INTO assignment_releases(task_id,session_id,instance_id)
	 VALUES(NEW.id,json_extract(OLD.owner_origin_json,'$.session_id'),json_extract(OLD.owner_origin_json,'$.instance_id'));
	END;
	CREATE TRIGGER IF NOT EXISTS assignment_release_task_changed AFTER UPDATE ON tasks
	WHEN NEW.title IS NOT OLD.title OR NEW.description IS NOT OLD.description
	 OR NEW.details IS NOT OLD.details OR NEW.labels IS NOT OLD.labels
	 OR NEW.goal IS NOT OLD.goal OR NEW.routing_json IS NOT OLD.routing_json
	 OR NEW.rank IS NOT OLD.rank OR NEW.status IN ('done','cancelled','user_review')
	BEGIN DELETE FROM assignment_releases WHERE task_id=NEW.id; END;
	CREATE TRIGGER IF NOT EXISTS assignment_release_commented AFTER INSERT ON task_comments
	WHEN NOT (NEW.author='amux' AND NEW.author_type='system')
	BEGIN
	 DELETE FROM assignment_releases WHERE task_id=NEW.task_id
	  AND (session_id IS NOT json_extract(NEW.origin_json,'$.session_id')
	       OR instance_id IS NOT json_extract(NEW.origin_json,'$.instance_id'));
	END;
	INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(9);`)
	return err
}

// migrateReviewAssignment persists the per-project ordinary/review turn. The
// migration coordinator owns ordering and invokes this after the review schema.
func migrateReviewAssignment(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS assignment_turns(
 project TEXT PRIMARY KEY,
 review_next INTEGER NOT NULL CHECK(review_next IN(0,1))
 );
 INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(13);`)
	return err
}

type assignmentCandidate struct {
	presence AgentPresence
	origin   AgentOrigin
}

// ReconcileAssignments refreshes presence first, then allocates at most one
// FIFO todo task to each explicitly opted-in idle session in one transaction.
func (s *Store) ReconcileAssignments(ctx context.Context, now time.Time) error {
	if err := s.ReconcilePresence(ctx, now); err != nil {
		return err
	}
	return s.taskMutation(ctx, nil, func(tx *sql.Tx, _ *AgentOrigin) error {
		return allocateAssignments(ctx, tx, now.UnixMilli())
	})
}

func allocateAssignments(ctx context.Context, tx *sql.Tx, nowMS int64) error {
	return allocateAssignmentsWithOffers(ctx, tx, nowMS, nil, router.Event{})
}

func allocateAssignmentsWithOffers(ctx context.Context, tx *sql.Tx, nowMS int64, registry *router.Registry, cause router.Event) error {
	enabled, err := pluginEnabled(ctx, tx, "assignment")
	if err != nil || !enabled {
		return err
	}
	candidates, err := assignmentReadyCandidates(ctx, tx, nowMS)
	if err != nil {
		return err
	}
	rand.Shuffle(len(candidates), func(i, j int) {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	})

	ready := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		ready[candidate.origin.SessionID] = true
	}
	for _, candidate := range candidates {
		origin := candidate.origin
		if !ready[origin.SessionID] {
			continue
		}
		var reviewNext bool
		if err := tx.QueryRowContext(ctx, "SELECT review_next FROM assignment_turns WHERE project=?", candidate.presence.ProjectIdent).Scan(&reviewNext); err != nil && err != sql.ErrNoRows {
			return err
		}
		queued, err := queuedAssignmentTasks(ctx, tx, candidate.presence.ProjectIdent, origin.SessionID, reviewNext)
		if err != nil {
			return err
		}
		for _, task := range queued {
			eligible, err := assignmentEligible(ctx, tx, task, candidate)
			if err != nil {
				return err
			}
			if !eligible {
				continue
			}
			isReview := isReviewAssignment(task)
			if isReview {
				selected, err := reviewRank(ctx, tx, &task, origin)
				if err != nil {
					return err
				}
				if selected != (reviewPreference{}) && !candidate.presence.ReviewContextClear {
					continue
				}
				preferredPeer := false
				for _, peer := range candidates {
					if !ready[peer.origin.SessionID] || peer.presence.ProjectIdent != candidate.presence.ProjectIdent || peer.origin.SessionID == origin.SessionID {
						continue
					}
					peerEligible, err := assignmentEligible(ctx, tx, task, peer)
					if err != nil {
						return err
					}
					if !peerEligible {
						continue
					}
					rank, err := reviewRank(ctx, tx, &task, peer.origin)
					if err != nil {
						return err
					}
					if rank != (reviewPreference{}) && !peer.presence.ReviewContextClear {
						continue
					}
					if rank.betterThan(selected) {
						preferredPeer = true
						break
					}
				}
				if preferredPeer {
					continue
				}
			}
			newTask := false
			if registry != nil {
				encodedTask, err := json.Marshal(task)
				if err != nil {
					return err
				}
				newTask, err = routerNewAssignment(ctx, tx, encodedTask)
				if err != nil {
					return err
				}
			}
			encoded, err := json.Marshal(origin)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE task_mutation_context SET origin_json=? WHERE id=1", string(encoded)); err != nil {
				return err
			}
			var claimed *Task
			if registry == nil {
				claimed, err = transitionActiveTask(ctx, tx, task.ProjectIdent, task.ID, "in_progress", &origin, TaskTransitionPolicy{})
			} else {
				claimed, err = reserveAssignment(ctx, tx, task, origin, encoded)
			}
			if err != nil {
				if errors.Is(err, ErrTaskUnsupported) || errors.Is(err, ErrTaskOwnership) || errors.Is(err, ErrTaskConflict) {
					continue
				}
				return fmt.Errorf("assign task %s: %w", task.ID, err)
			}
			ready[origin.SessionID] = false
			if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO assignment_turns(project,review_next) VALUES(?,?)", candidate.presence.ProjectIdent, !isReview); err != nil {
				return err
			}
			if registry != nil {
				const comment = "Automatically assigned by amux to an idle agent. Evaluate whether this task is still required. If a user decision is needed, record the question and move the task to User review. Close obsolete work with a reason; otherwise start execution. Respect platform requirements."
				if _, err := insertTaskComment(ctx, tx, task.ID, "amux", "system", comment, nil, nil); err != nil {
					return err
				}
				claimed, err = readTask(ctx, tx, task.ProjectIdent, task.ID)
				if err != nil {
					return err
				}
				if err := createRouterAssignmentOffer(ctx, tx, registry, cause, claimed, origin, candidate.presence.Source, newTask); err != nil {
					return err
				}
			}
			break
		}
	}
	return nil
}

func reserveAssignment(ctx context.Context, tx *sql.Tx, task Task, owner AgentOrigin, ownerJSON []byte) (*Task, error) {
	if err := ordinaryWorkflowTask(ctx, tx, &task); err != nil {
		return nil, err
	}
	if err := ensureReviewer(ctx, tx, &task, &owner); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET owner_agent_id=?,owner_origin_json=?,updated_at=? WHERE id=?`,
		owner.SessionID, string(ownerJSON), time.Now().UnixMilli(), task.ID); err != nil {
		return nil, err
	}
	return readTask(ctx, tx, task.ProjectIdent, task.ID)
}

func assignmentReadyCandidates(ctx context.Context, q queryer, observed int64) ([]assignmentCandidate, error) {
	projects, err := readProjectIdents(ctx, q)
	if err != nil {
		return nil, err
	}
	presenceRows, observed, err := livePresenceSnapshot(ctx, q, observed)
	if err != nil {
		return nil, err
	}
	refused, err := reconcileRouterAssignmentRefusals(ctx, q, presenceRows)
	if err != nil {
		return nil, err
	}
	disabled, err := disabledProviders(ctx, q)
	if err != nil {
		return nil, err
	}
	busy := make(map[string]bool)
	rows, err := q.QueryContext(ctx, "SELECT DISTINCT json_extract(owner_origin_json,'$.session_id') FROM tasks WHERE (status IN ('in_progress','stalled') OR (status='todo' AND EXISTS(SELECT 1 FROM assignment_offers o WHERE o.task_id=tasks.id))) AND owner_origin_json IS NOT NULL")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var session string
		if err := rows.Scan(&session); err != nil {
			rows.Close()
			return nil, err
		}
		busy[session] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	candidates := make([]assignmentCandidate, 0)
	seenSessions := make(map[string]bool)
	byProject := make(map[string][]AgentPresence)
	for _, presence := range presenceRows {
		byProject[presence.ProjectIdent] = append(byProject[presence.ProjectIdent], presence)
	}
	for _, project := range projects {
		for _, presence := range byProject[project] {
			if presence.ProjectIdent != project || presence.State == "gone" {
				continue
			}
			origin, err := NormalizeOrigin(&presence.Origin)
			if err != nil {
				continue
			}
			if !presence.AutoAssign || presence.ProjectIdent != project || presence.Source == "" ||
				presence.State != "idle" || presence.Busy || presence.Draft || presence.LastActivityAt == nil ||
				observed-presence.SeenAt >= presenceGoneAfter ||
				busy[origin.SessionID] || refused[origin.SessionID] || seenSessions[origin.SessionID] ||
				disabled[origin.Provider] {
				continue
			}
			seenSessions[origin.SessionID] = true
			candidates = append(candidates, assignmentCandidate{presence: presence, origin: *origin})
		}
	}
	return candidates, nil
}

func readProjectIdents(ctx context.Context, q queryer) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT ident FROM projects WHERE archived_at IS NULL ORDER BY ident")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			return nil, err
		}
		result = append(result, project)
	}
	return result, rows.Err()
}

func queuedAssignmentTasks(ctx context.Context, q queryer, project, session string, reviewNext bool) ([]Task, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+taskSelect+` FROM tasks t
		LEFT JOIN amux_review_rework r ON r.source_task_id=t.id
		WHERE t.project_ident=? AND t.status='todo'
		ORDER BY CASE
			WHEN r.session_id=? THEN 0
			WHEN ? AND EXISTS(SELECT 1 FROM json_each(CASE WHEN json_valid(t.labels) THEN t.labels ELSE '[]' END) WHERE value='task-review') THEN 1
			ELSE 2
		END,t.created_at,t.id`, project, session, reviewNext)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *task)
	}
	return result, rows.Err()
}

func assignmentEligible(ctx context.Context, q queryer, task Task, candidate assignmentCandidate) (bool, error) {
	if task.Status != "todo" ||
		(task.OwnerAgentID != nil && task.OwnerOrigin == nil) ||
		task.Routing == nil || !json.Valid(task.Routing) {
		return false, nil
	}
	if task.Kind == "delegated" || task.DelegatedToTaskID != nil {
		ready, err := taskDelegationReady(ctx, q, task.ProjectIdent, task.ID)
		if err != nil || !ready {
			return false, err
		}
	}
	var routing struct {
		Mode   string                    `json:"mode"`
		Target *RegisteredTargetIdentity `json:"target"`
	}
	if err := json.Unmarshal(task.Routing, &routing); err != nil {
		return false, nil
	}
	switch routing.Mode {
	case "auto":
	case "session":
		if routing.Target == nil || routing.Target.Origin != candidate.origin {
			return false, nil
		}
	default:
		return false, nil
	}
	if task.Goal != nil && (candidate.presence.Goal == nil || *candidate.presence.Goal != *task.Goal) {
		return false, nil
	}
	if !taskplatform.Matches(task.Labels, candidate.origin.OS) {
		return false, nil
	}
	if task.OwnerOrigin != nil && (task.OwnerOrigin.SessionID != candidate.origin.SessionID || task.OwnerOrigin.InstanceID != candidate.origin.InstanceID) {
		return false, nil
	}
	var reworkSession sql.NullString
	err := q.QueryRowContext(ctx, `SELECT session_id FROM amux_review_rework WHERE source_task_id=?`, task.ID).Scan(&reworkSession)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if reworkSession.Valid && reworkSession.String != candidate.origin.SessionID {
		return false, nil
	}
	var released bool
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM assignment_releases WHERE task_id=? AND session_id=? AND instance_id=?) OR EXISTS(SELECT 1 FROM task_recovery_sessions WHERE task_id=? AND session_id=?)", task.ID, candidate.origin.SessionID, candidate.origin.InstanceID, task.ID, candidate.origin.SessionID).Scan(&released); err != nil {
		return false, err
	}
	return !released, nil
}

func isReviewAssignment(task Task) bool {
	return hasLabel(task.Labels, "task-review")
}
