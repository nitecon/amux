package storage

import (
	"context"
	"time"

	projectid "github.com/nitecon/amux/identity/project"
)

func (s *Store) ResolveProjectMessage(ctx context.Context, project string, messageID int64, resolvedBy string) (bool, error) {
	return updateProjectMessageResolution(ctx, s.db, project, messageID, time.Now().UTC().UnixMilli(), resolvedBy, true)
}

func (s *Store) ReopenProjectMessage(ctx context.Context, project string, messageID int64) (bool, error) {
	return updateProjectMessageResolution(ctx, s.db, project, messageID, 0, "", false)
}

// updateProjectMessageResolution is shared with transactional human-reply
// authoring so insertion and optional root resolution remain atomic.
func updateProjectMessageResolution(ctx context.Context, q queryer, project string, messageID, resolvedAt int64, resolvedBy string, resolved bool) (bool, error) {
	project = projectid.NormalizeIdent(project)
	var result interface{ RowsAffected() (int64, error) }
	var err error
	if resolved {
		result, err = q.ExecContext(ctx, `UPDATE messages SET resolved_at=?,resolved_by=? WHERE id=? AND project_ident=? AND resolved_at IS NULL`, resolvedAt, resolvedBy, messageID, project)
	} else {
		result, err = q.ExecContext(ctx, `UPDATE messages SET resolved_at=NULL,resolved_by=NULL WHERE id=? AND project_ident=? AND resolved_at IS NOT NULL`, messageID, project)
	}
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed != 0, err
}
