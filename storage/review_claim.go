package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type reviewPreference struct {
	ProviderMismatch bool
	Contributor      bool
}

func (r reviewPreference) betterThan(other reviewPreference) bool {
	if r.Contributor != other.Contributor {
		return !r.Contributor
	}
	return !r.ProviderMismatch && other.ProviderMismatch
}

func reviewRank(ctx context.Context, q queryer, task *Task, origin AgentOrigin) (reviewPreference, error) {
	var preference reviewPreference
	if !hasLabel(task.Labels, "task-review") {
		return preference, nil
	}

	preferred := ""
	providerLabelFound := false
	for _, label := range task.Labels {
		if len(label) >= len("review-provider:") && label[:len("review-provider:")] == "review-provider:" {
			preferred = label[len("review-provider:"):]
			providerLabelFound = true
			break
		}
	}
	if !providerLabelFound {
		for _, label := range task.Labels {
			if len(label) <= len("review-of:") || label[:len("review-of:")] != "review-of:" {
				continue
			}
			sourceID := label[len("review-of:"):]
			var provider sql.NullString
			err := q.QueryRowContext(ctx, `SELECT COALESCE(json_extract(owner_origin_json,'$.provider'),
				(SELECT json_extract(origin_json,'$.provider') FROM task_comments
				 WHERE task_id=tasks.id AND author_type='agent'
				 AND json_extract(origin_json,'$.provider') IS NOT NULL
				 ORDER BY created_at DESC,id DESC LIMIT 1)) FROM tasks WHERE id=?`, sourceID).Scan(&provider)
			if err != nil && err != sql.ErrNoRows {
				return preference, err
			}
			if provider.Valid {
				if opposite := oppositeReviewProvider(provider.String); opposite != nil {
					preferred = *opposite
				}
			}
			break
		}
	}
	preference.ProviderMismatch = preferred != "" && origin.Provider != preferred
	for _, label := range task.Labels {
		if len(label) <= len("review-exclude:") || label[:len("review-exclude:")] != "review-exclude:" {
			continue
		}
		excluded := label[len("review-exclude:"):]
		if origin.SessionID == excluded {
			preference.Contributor = true
			break
		}
	}
	return preference, nil
}

func ensureReviewer(ctx context.Context, tx *sql.Tx, task *Task, origin *AgentOrigin) error {
	if task == nil || origin == nil || !hasLabel(task.Labels, "task-review") {
		return nil
	}
	selected, err := reviewRank(ctx, tx, task, *origin)
	if err != nil {
		return err
	}
	var observed int64
	if err := tx.QueryRowContext(ctx, "SELECT observed_at FROM amux_presence_clock WHERE id=1").Scan(&observed); err != nil {
		return err
	}
	if now := time.Now().UnixMilli(); now > observed {
		observed = now
	}
	candidates, err := assignmentReadyCandidates(ctx, tx, observed)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if candidate.presence.ProjectIdent != task.ProjectIdent {
			continue
		}
		eligible, err := assignmentEligible(ctx, tx, *task, candidate)
		if err != nil {
			return err
		}
		if !eligible {
			continue
		}
		peer, err := reviewRank(ctx, tx, task, candidate.origin)
		if err != nil {
			return err
		}
		if peer != (reviewPreference{}) && !candidate.presence.ReviewContextClear {
			continue
		}
		if peer.betterThan(selected) {
			return fmt.Errorf("%w: an available project reviewer has a better provider/independence preference", ErrTaskConflict)
		}
	}
	return nil
}
