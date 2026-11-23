package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/nitecon/amux/projects"
)

var errSpecSourceTaskFound = errors.New("spec source task already exists")

// FindOrCreateSpecSourceTask serializes Rust-compatible source-tuple recovery
// with task insertion. A later link failure can safely be retried without
// creating another task for the same accepted manifest item.
func (s *Store) FindOrCreateSpecSourceTask(ctx context.Context, projectIdent, artifactID, versionID, itemID string, input TaskInsert, origin *AgentOrigin) (*Task, error) {
	projectIdent = projects.NormalizeIdent(projectIdent)
	input.Project = projects.NormalizeIdent(input.Project)
	if projectIdent == "" || input.Project != projectIdent || artifactID == "" || versionID == "" || itemID == "" {
		return nil, fmt.Errorf("invalid spec task source")
	}
	if input.Details == nil || !hasExactDetailsLine(*input.Details, "source_spec_artifact_id: "+artifactID) ||
		!hasExactDetailsLine(*input.Details, "source_spec_version_id: "+versionID) ||
		!hasExactDetailsLine(*input.Details, "manifest_item_id: "+itemID) {
		return nil, fmt.Errorf("spec task details must include the source tuple")
	}
	var task *Task
	err := s.taskMutation(ctx, origin, func(tx *sql.Tx, actor *AgentOrigin) error {
		var sourceExists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM artifact_versions v
JOIN artifacts a ON a.artifact_id=v.artifact_id
WHERE a.project_ident=? AND a.artifact_id=? AND v.artifact_version_id=?`, projectIdent, artifactID, versionID).Scan(&sourceExists); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("spec source version not found in project")
			}
			return err
		}
		existing, err := findSpecSourceTask(ctx, tx, projectIdent, artifactID, versionID, itemID)
		if err != nil {
			return err
		}
		if existing != nil {
			task = existing
			return errSpecSourceTaskFound
		}
		task, err = insertTaskRecord(ctx, tx, input, actor, "general")
		return err
	})
	if errors.Is(err, errSpecSourceTaskFound) {
		return task, nil
	}
	return task, err
}

func findSpecSourceTask(ctx context.Context, q queryer, projectIdent, artifactID, versionID, itemID string) (*Task, error) {
	// Rust-compatible source fields are standalone lines in the legacy details
	// block. instr avoids LIKE wildcard interpretation and prefix collisions.
	line := func(field, value string) string { return "\n" + field + ": " + value + "\n" }
	artifactLine := line("source_spec_artifact_id", artifactID)
	versionLine := line("source_spec_version_id", versionID)
	itemLine := line("manifest_item_id", itemID)
	return scanTask(q.QueryRowContext(ctx, "SELECT "+taskSelect+` FROM tasks t
WHERE t.project_ident=?
AND instr(char(10)||replace(t.details,char(13)||char(10),char(10))||char(10), ?) > 0
AND instr(char(10)||replace(t.details,char(13)||char(10),char(10))||char(10), ?) > 0
AND instr(char(10)||replace(t.details,char(13)||char(10),char(10))||char(10), ?) > 0
ORDER BY t.created_at ASC LIMIT 1`, projects.NormalizeIdent(projectIdent), artifactLine, versionLine, itemLine))
}

func hasExactDetailsLine(details, expected string) bool {
	details = strings.ReplaceAll(details, "\r\n", "\n")
	for _, line := range strings.Split(details, "\n") {
		if line == expected {
			return true
		}
	}
	return false
}
