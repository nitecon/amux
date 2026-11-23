package storage

import "context"

// CreateSpecArtifactWithInitialVersion inserts the spec and its imported
// version under one transaction, including both quota checks and the current
// version pointer update.
func (s *Store) CreateSpecArtifactWithInitialVersion(ctx context.Context, artifact ArtifactInsert, version ArtifactVersionInsert) (*ArtifactWrite, *ArtifactVersionWrite, error) {
	limits, err := artifactWriteLimits()
	if err != nil {
		return nil, nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	artifactWrite, err := createArtifactWriteTx(ctx, tx, artifact, limits)
	if err != nil {
		return nil, nil, err
	}
	versionWrite, err := createArtifactVersionTx(ctx, tx, artifactWrite.Record.ProjectIdent, artifactWrite.Record.ArtifactID, version, &limits, false)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return artifactWrite, versionWrite, nil
}
