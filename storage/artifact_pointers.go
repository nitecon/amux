package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/nitecon/amux/projects"
)

// SetArtifactVersionPointers updates the supplied pointers together. A nil
// pointer keeps the existing value.
func (s *Store) SetArtifactVersionPointers(ctx context.Context, project, artifactID string, current, accepted *string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setArtifactVersionPointersTx(ctx, tx, project, artifactID, current, accepted); err != nil {
		return err
	}
	return tx.Commit()
}

func setArtifactVersionPointersTx(ctx context.Context, tx *sql.Tx, project, artifactID string, current, accepted *string) error {
	project = projects.NormalizeIdent(project)
	var found int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM artifacts WHERE project_ident=? AND artifact_id=?", project, artifactID).Scan(&found); err != nil {
		return err
	}
	for _, pointer := range []struct {
		name string
		id   *string
	}{{"current_version_id", current}, {"accepted_version_id", accepted}} {
		if pointer.id == nil {
			continue
		}
		var owner string
		err := tx.QueryRowContext(ctx, "SELECT artifact_id FROM artifact_versions WHERE artifact_version_id=?", *pointer.id).Scan(&owner)
		if err == sql.ErrNoRows {
			return fmt.Errorf("%s references missing version", pointer.name)
		}
		if err != nil {
			return err
		}
		if owner != artifactID {
			return fmt.Errorf("%s belongs to another artifact", pointer.name)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE artifacts SET current_version_id=COALESCE(?,current_version_id),
 accepted_version_id=COALESCE(?,accepted_version_id),updated_at=? WHERE project_ident=? AND artifact_id=?`,
		current, accepted, time.Now().UnixMilli(), project, artifactID); err != nil {
		return err
	}
	return nil
}
