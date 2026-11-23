package storage

import (
	"context"
	"database/sql"
	"time"

	"github.com/nitecon/amux/projects"
)

// MarkDesignReviewCollecting applies the state transition performed after a
// design-review round starts. Run creation remains a separate operation, as in
// the gateway route.
func (s *Store) MarkDesignReviewCollecting(ctx context.Context, projectIdent, artifactID string) (*Artifact, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	projectIdent = projects.NormalizeIdent(projectIdent)
	result, err := tx.ExecContext(ctx, `UPDATE artifacts
SET lifecycle_state='active', review_state='collecting_reviews', updated_at=?
WHERE project_ident=? AND artifact_id=? AND kind='design_review'`, time.Now().UnixMilli(), projectIdent, artifactID)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed == 0 {
		return nil, nil
	}
	artifact, err := scanArtifact(tx.QueryRowContext(ctx, artifactSelect+` WHERE project_ident=? AND artifact_id=?`, projectIdent, artifactID))
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return artifact, nil
}
