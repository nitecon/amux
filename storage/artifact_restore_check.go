package storage

import (
	"context"
	"database/sql"
	"fmt"
)

type ArtifactRestoreFinding struct {
	Tag        string `json:"tag"`
	EntityKind string `json:"entity_kind"`
	EntityID   string `json:"entity_id"`
	Detail     string `json:"detail"`
}

type ArtifactRestoreReport struct {
	ArtifactPointerMismatches  []ArtifactRestoreFinding `json:"artifact_pointer_mismatches"`
	DanglingLinks              []ArtifactRestoreFinding `json:"dangling_links"`
	WorkflowRunInconsistencies []ArtifactRestoreFinding `json:"workflow_run_inconsistencies"`
	IdempotencyMappingIssues   []ArtifactRestoreFinding `json:"idempotency_mapping_issues"`
	ChunkRegenerationIssues    []ArtifactRestoreFinding `json:"chunk_regeneration_issues"`
}

type ArtifactRestoreCheckOptions struct {
	WorkflowRunStuckTTLHours uint32
	IdempotencySampleSize    uint32
}

func (r ArtifactRestoreReport) IsClean() bool {
	return len(r.ArtifactPointerMismatches)+len(r.DanglingLinks)+len(r.WorkflowRunInconsistencies)+len(r.IdempotencyMappingIssues)+len(r.ChunkRegenerationIssues) == 0
}

// CheckArtifactRestore reports restore inconsistencies without repairing them.
// nowMS is explicit so callers and tests control the stuck-run cutoff.
func (s *Store) CheckArtifactRestore(ctx context.Context, nowMS int64, options ArtifactRestoreCheckOptions) (*ArtifactRestoreReport, error) {
	if options.WorkflowRunStuckTTLHours == 0 {
		options.WorkflowRunStuckTTLHours = 24
	}
	if options.IdempotencySampleSize == 0 {
		options.IdempotencySampleSize = 100
	}
	r := &ArtifactRestoreReport{}

	rows, err := s.db.QueryContext(ctx, `SELECT a.artifact_id,a.current_version_id,a.accepted_version_id,
 cv.artifact_id, av.artifact_id
 FROM artifacts a
 LEFT JOIN artifact_versions cv ON cv.artifact_version_id=a.current_version_id
 LEFT JOIN artifact_versions av ON av.artifact_version_id=a.accepted_version_id
 WHERE a.current_version_id IS NOT NULL OR a.accepted_version_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var artifact string
		var current, accepted, currentOwner, acceptedOwner sql.NullString
		if err := rows.Scan(&artifact, &current, &accepted, &currentOwner, &acceptedOwner); err != nil {
			rows.Close()
			return nil, err
		}
		for _, p := range []struct {
			name      string
			id, owner sql.NullString
		}{{"current_version_id", current, currentOwner}, {"accepted_version_id", accepted, acceptedOwner}} {
			if !p.id.Valid {
				continue
			}
			detail := ""
			if !p.owner.Valid {
				detail = fmt.Sprintf("%s=%s references missing version", p.name, p.id.String)
			} else if p.owner.String != artifact {
				detail = fmt.Sprintf("%s=%s belongs to artifact %s", p.name, p.id.String, p.owner.String)
			}
			if detail != "" {
				r.ArtifactPointerMismatches = append(r.ArtifactPointerMismatches, ArtifactRestoreFinding{"restore:pointer_mismatch", "artifact", artifact, detail})
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT l.link_id,l.link_type,l.source_version_id,l.target_version_id,
 sv.artifact_version_id,tv.artifact_version_id
 FROM artifact_links l
 LEFT JOIN artifact_versions sv ON sv.artifact_version_id=l.source_version_id
 LEFT JOIN artifact_versions tv ON tv.artifact_version_id=l.target_version_id
 WHERE l.source_version_id IS NOT NULL OR l.target_version_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, typ string
		var src, tgt, srcFound, tgtFound sql.NullString
		if err := rows.Scan(&id, &typ, &src, &tgt, &srcFound, &tgtFound); err != nil {
			rows.Close()
			return nil, err
		}
		for _, p := range []struct {
			name      string
			id, found sql.NullString
		}{{"source_version_id", src, srcFound}, {"target_version_id", tgt, tgtFound}} {
			if p.id.Valid && !p.found.Valid {
				r.DanglingLinks = append(r.DanglingLinks, ArtifactRestoreFinding{"restore:dangling", "link", id, fmt.Sprintf("link_type=%s %s=%s missing", typ, p.name, p.id.String)})
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	cutoff := nowMS - int64(options.WorkflowRunStuckTTLHours)*3600000
	rows, err = s.db.QueryContext(ctx, `SELECT workflow_run_id,started_at FROM workflow_runs WHERE state='started' AND started_at<=?`, cutoff)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var started int64
		if err := rows.Scan(&id, &started); err != nil {
			rows.Close()
			return nil, err
		}
		r.WorkflowRunInconsistencies = append(r.WorkflowRunInconsistencies, ArtifactRestoreFinding{"restore:stuck_workflow_run", "workflow_run", id, fmt.Sprintf("state=started started_at=%d older than ttl_hours=%d", started, options.WorkflowRunStuckTTLHours)})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	generatedChecks := []struct{ column, table, pk, label string }{{"generated_version_ids", "artifact_versions", "artifact_version_id", "version"}, {"generated_contribution_ids", "artifact_contributions", "contribution_id", "contribution"}, {"generated_link_ids", "artifact_links", "link_id", "link"}, {"generated_chunk_ids", "artifact_chunks", "chunk_id", "chunk"}}
	for _, check := range generatedChecks {
		jsonValue := fmt.Sprintf("CASE WHEN json_valid(w.%s) THEN w.%s ELSE '[]' END", check.column, check.column)
		array := fmt.Sprintf("CASE WHEN json_type(%s)='array' AND NOT EXISTS (SELECT 1 FROM json_each(%s) item WHERE item.type<>'text') THEN %s ELSE '[]' END", jsonValue, jsonValue, jsonValue)
		q := fmt.Sprintf(`SELECT w.workflow_run_id,j.value FROM workflow_runs w,json_each(%s) j LEFT JOIN %s x ON x.%s=j.value WHERE w.state='succeeded' AND x.%s IS NULL`, array, check.table, check.pk, check.pk)
		rows, err = s.db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var run, id string
			if err := rows.Scan(&run, &id); err != nil {
				rows.Close()
				return nil, err
			}
			r.WorkflowRunInconsistencies = append(r.WorkflowRunInconsistencies, ArtifactRestoreFinding{"restore:dangling", "workflow_run", run, fmt.Sprintf("generated %s %s missing", check.label, id)})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}

	for _, t := range []struct{ table, pk, run, label string }{{"artifact_versions", "artifact_version_id", "created_via_workflow_run_id", "version"}, {"artifact_contributions", "contribution_id", "workflow_run_id", "contribution"}, {"artifact_links", "link_id", "created_via_workflow_run_id", "link"}, {"artifact_comments", "comment_id", "resolved_by_workflow_run_id", "comment"}} {
		q := fmt.Sprintf(`SELECT sample.entity_id,sample.run_id,sample.idempotency_key FROM (SELECT %s entity_id,%s run_id,idempotency_key FROM %s WHERE %s IS NOT NULL AND idempotency_key IS NOT NULL LIMIT ?) sample LEFT JOIN workflow_runs w ON w.workflow_run_id=sample.run_id WHERE w.workflow_run_id IS NULL`, t.pk, t.run, t.table, t.run)
		rows, err = s.db.QueryContext(ctx, q, options.IdempotencySampleSize)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var entity, run, key string
			if err := rows.Scan(&entity, &run, &key); err != nil {
				rows.Close()
				return nil, err
			}
			r.IdempotencyMappingIssues = append(r.IdempotencyMappingIssues, ArtifactRestoreFinding{"restore:idempotency_mismatch", t.label, entity, fmt.Sprintf("idempotency_key=%s references missing workflow_run=%s", key, run)})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}

	rows, err = s.db.QueryContext(ctx, `SELECT a.artifact_id,a.accepted_version_id FROM artifacts a WHERE a.accepted_version_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM artifact_chunks c WHERE c.artifact_version_id=a.accepted_version_id AND c.superseded_by_chunk_id IS NULL)`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var artifact, version string
		if err := rows.Scan(&artifact, &version); err != nil {
			rows.Close()
			return nil, err
		}
		r.ChunkRegenerationIssues = append(r.ChunkRegenerationIssues, ArtifactRestoreFinding{"restore:chunk_count_mismatch", "artifact", artifact, fmt.Sprintf("accepted_version_id=%s has zero live chunks", version)})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return r, nil
}
