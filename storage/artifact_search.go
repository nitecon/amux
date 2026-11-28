package storage

import (
	"context"
	"database/sql"
	"strings"

	projectid "github.com/nitecon/amux/identity/project"
)

// ArtifactFilters limits a project-scoped artifact search.
type ArtifactFilters struct {
	Kind, Subkind, LifecycleState, Label, ActorID, Query string
}

// SearchArtifacts returns artifact summaries in the same stable order as the
// Rust read API. Every query remains scoped to the normalized project.
func (s *Store) SearchArtifacts(ctx context.Context, projectIdent string, filter ArtifactFilters) ([]Artifact, error) {
	query := artifactSelect + ` WHERE project_ident=?`
	args := []any{projectid.NormalizeIdent(projectIdent)}
	for _, f := range []struct{ col, value string }{{"kind", filter.Kind}, {"subkind", filter.Subkind}, {"lifecycle_state", filter.LifecycleState}} {
		if value := strings.TrimSpace(f.value); value != "" {
			query += ` AND ` + f.col + `=?`
			args = append(args, value)
		}
	}
	if value := strings.TrimSpace(filter.Label); value != "" {
		query += ` AND COALESCE(labels,'') LIKE ?`
		args = append(args, `%"`+value+`"%`)
	}
	if value := strings.TrimSpace(filter.ActorID); value != "" {
		query += ` AND (created_by_actor_id=:actor
 OR EXISTS (SELECT 1 FROM artifact_contributions c WHERE c.artifact_id=artifacts.artifact_id AND c.actor_id=:actor)
 OR EXISTS (SELECT 1 FROM artifact_comments c WHERE c.artifact_id=artifacts.artifact_id AND c.actor_id=:actor)
 OR EXISTS (SELECT 1 FROM artifact_links l WHERE l.created_by_actor_id=:actor AND (l.source_id=artifacts.artifact_id OR l.target_id=artifacts.artifact_id)))`
		args = append(args, sql.Named("actor", value))
	}
	if value := strings.TrimSpace(filter.Query); value != "" {
		query += ` AND (artifact_id LIKE :query OR title LIKE :query OR kind LIKE :query OR COALESCE(subkind,'') LIKE :query OR COALESCE(labels,'') LIKE :query
 OR EXISTS (SELECT 1 FROM artifact_versions v WHERE v.artifact_id=artifacts.artifact_id AND (COALESCE(v.body,'') LIKE :query OR COALESCE(v.structured_payload,'') LIKE :query))
 OR EXISTS (SELECT 1 FROM artifact_contributions c WHERE c.artifact_id=artifacts.artifact_id AND c.body LIKE :query)
 OR EXISTS (SELECT 1 FROM artifact_links l
   WHERE (l.source_id=artifacts.artifact_id OR l.target_id=artifacts.artifact_id
      OR l.source_version_id IN (SELECT v.artifact_version_id FROM artifact_versions v WHERE v.artifact_id=artifacts.artifact_id)
      OR l.target_version_id IN (SELECT v.artifact_version_id FROM artifact_versions v WHERE v.artifact_id=artifacts.artifact_id))
   AND (l.source_id LIKE :query OR l.target_id LIKE :query OR COALESCE(l.source_version_id,'') LIKE :query OR COALESCE(l.target_version_id,'') LIKE :query OR l.link_type LIKE :query)))`
		args = append(args, sql.Named("query", "%"+value+"%"))
	}
	query += ` ORDER BY updated_at DESC,title ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Artifact{}
	for rows.Next() {
		item, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}
