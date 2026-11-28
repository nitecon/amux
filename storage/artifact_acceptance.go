package storage

import (
	"context"
	"fmt"

	projectid "github.com/nitecon/amux/identity/project"
)

// AcceptArtifactVersion accepts an eligible version of a project spec and
// atomically updates its state, both artifact pointers, and audit contribution.
func (s *Store) AcceptArtifactVersion(ctx context.Context, projectIdent, artifactID, versionID, actorID string, workflowRunID, idempotencyKey *string) (*ArtifactContribution, error) {
	projectIdent = projectid.NormalizeIdent(projectIdent)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	currentArtifact, err := scanArtifact(tx.QueryRowContext(ctx, artifactSelect+` WHERE project_ident=? AND artifact_id=?`, projectIdent, artifactID))
	if err != nil {
		return nil, err
	}
	if currentArtifact == nil {
		return nil, fmt.Errorf("artifact not found")
	}
	if currentArtifact.Kind != "spec" {
		return nil, fmt.Errorf("artifact is not a spec")
	}
	if _, err := validateWorkflowRunForMutation(ctx, tx, artifactID, workflowRunID); err != nil {
		return nil, err
	}
	version, err := scanArtifactVersion(tx.QueryRowContext(ctx, artifactVersionSelect+` WHERE artifact_id=? AND artifact_version_id=?`, artifactID, versionID))
	if err != nil {
		return nil, err
	}
	if version == nil {
		return nil, fmt.Errorf("version not found")
	}
	if version.VersionState != "draft" && version.VersionState != "under_review" && version.VersionState != "accepted" {
		return nil, fmt.Errorf("version cannot be accepted from current state")
	}
	phase := "acceptance"
	contributionInput := ArtifactContributionInsert{
		TargetKind: "artifact_version", TargetID: versionID, ContributionKind: "state_transition",
		Phase: &phase, Role: "coordinator", ActorID: actorID,
		WorkflowRunID: workflowRunID, BodyFormat: "markdown", Body: "accepted artifact version",
		IdempotencyKey: idempotencyKey,
	}
	existing, err := findArtifactContributionReplayTx(ctx, tx, artifactID, contributionInput)
	if err != nil {
		return nil, err
	}
	if existing != nil && (existing.TargetKind != contributionInput.TargetKind || existing.TargetID != versionID || existing.ContributionKind != contributionInput.ContributionKind || existing.Phase == nil || *existing.Phase != phase) {
		return nil, fmt.Errorf("idempotency key was already used for another contribution")
	}
	if version.VersionState != "accepted" {
		if _, err := tx.ExecContext(ctx, `UPDATE artifact_versions SET version_state='accepted' WHERE artifact_id=? AND artifact_version_id=?`, artifactID, versionID); err != nil {
			return nil, err
		}
	}
	if existing == nil {
		if err := setArtifactVersionPointersTx(ctx, tx, projectIdent, artifactID, &versionID, &versionID); err != nil {
			return nil, err
		}
	} else {
		// Rust's current accept_artifact_version rewrites both pointers on every
		// replay. Follow the rebuild's stale-replay invariant instead: repair a
		// NULL pointer, but never move a non-NULL pointer backward.
		var current, accepted *string
		if currentArtifact.CurrentVersionID == nil {
			current = &versionID
		}
		if currentArtifact.AcceptedVersionID == nil {
			accepted = &versionID
		}
		if current != nil || accepted != nil {
			if err := setArtifactVersionPointersTx(ctx, tx, projectIdent, artifactID, current, accepted); err != nil {
				return nil, err
			}
		}
	}
	contribution, _, err := addArtifactContributionTx(ctx, tx, projectIdent, artifactID, contributionInput)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return contribution, nil
}
