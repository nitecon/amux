package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/projects"
)

// artifactChunksMigration follows the artifact base schema (migration 37).
// Registration in store.go is coordinated separately with the artifact owner.
const artifactChunksMigration = 38

// ArtifactChunk is a project artifact fragment anchored to one immutable version.
type ArtifactChunk struct {
	ChunkID             string          `json:"chunk_id"`
	ArtifactID          string          `json:"artifact_id"`
	ArtifactVersionID   string          `json:"artifact_version_id"`
	ChildAddress        string          `json:"child_address"`
	Text                string          `json:"text"`
	EmbeddingModel      *string         `json:"embedding_model"`
	EmbeddingVector     []byte          `json:"embedding_vector"`
	App                 *string         `json:"app"`
	Label               *string         `json:"label"`
	Kind                *string         `json:"kind"`
	Metadata            json.RawMessage `json:"metadata"`
	SupersededByChunkID *string         `json:"superseded_by_chunk_id"`
	CreatedAt           int64           `json:"created_at"`
}

type ArtifactChunkInsert struct {
	ArtifactVersionID string
	ChildAddress      string
	Text              string
	EmbeddingModel    *string
	EmbeddingVector   []byte
	App               *string
	Label             *string
	Kind              *string
	Metadata          json.RawMessage
}

type ArtifactChunkFilter struct {
	ArtifactVersionID string
	App               string
	Label             string
	Kind              string
	Query             string
	IncludeSuperseded bool
}

// migrateArtifactChunks creates the Rust-compatible natural address and soft
// supersession schema. Callers own the transaction and migration registration.
func migrateArtifactChunks(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS artifact_chunks (
 chunk_id TEXT PRIMARY KEY,
 artifact_id TEXT NOT NULL REFERENCES artifacts(artifact_id) ON DELETE CASCADE,
 artifact_version_id TEXT NOT NULL REFERENCES artifact_versions(artifact_version_id),
 child_address TEXT NOT NULL,
 text TEXT NOT NULL,
 embedding_model TEXT,
 embedding_vector BLOB,
 app TEXT,
 label TEXT,
 kind TEXT,
 metadata_json TEXT,
 superseded_by_chunk_id TEXT REFERENCES artifact_chunks(chunk_id),
 created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifact_chunks_natural_key
 ON artifact_chunks(artifact_version_id, child_address);
CREATE INDEX IF NOT EXISTS idx_artifact_chunks_artifact_current
 ON artifact_chunks(artifact_id, superseded_by_chunk_id);
CREATE INDEX IF NOT EXISTS idx_artifact_chunks_app_label
 ON artifact_chunks(app, label, kind);`)
	if err != nil {
		return fmt.Errorf("migrate artifact chunks: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)`, artifactChunksMigration); err != nil {
		return fmt.Errorf("record artifact chunks migration: %w", err)
	}
	return nil
}

const artifactChunkSelect = `SELECT chunk_id,artifact_id,artifact_version_id,child_address,text,embedding_model,embedding_vector,app,label,kind,metadata_json,superseded_by_chunk_id,created_at FROM artifact_chunks`

func scanArtifactChunk(row interface{ Scan(...any) error }) (*ArtifactChunk, error) {
	var item ArtifactChunk
	var metadata sql.NullString
	if err := row.Scan(&item.ChunkID, &item.ArtifactID, &item.ArtifactVersionID, &item.ChildAddress, &item.Text, &item.EmbeddingModel, &item.EmbeddingVector, &item.App, &item.Label, &item.Kind, &metadata, &item.SupersededByChunkID, &item.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if metadata.Valid {
		if !json.Valid([]byte(metadata.String)) {
			return nil, fmt.Errorf("decode artifact chunk metadata: invalid JSON")
		}
		item.Metadata = json.RawMessage(metadata.String)
	}
	return &item, nil
}

// CreateArtifactChunk inserts a chunk, replaying the existing natural address
// for the same artifact version just as the Rust gateway does.
func (s *Store) CreateArtifactChunk(ctx context.Context, projectIdent, artifactID string, input ArtifactChunkInsert) (*ArtifactChunk, bool, error) {
	return s.createArtifactChunkWithLimits(ctx, projectIdent, artifactID, input, nil)
}

func (s *Store) createArtifactChunkWithLimits(ctx context.Context, projectIdent, artifactID string, input ArtifactChunkInsert, suppliedLimits *ArtifactWriteLimits) (*ArtifactChunk, bool, error) {
	projectIdent = projects.NormalizeIdent(projectIdent)
	if projectIdent == "" || artifactID == "" || input.ArtifactVersionID == "" || input.ChildAddress == "" {
		return nil, false, fmt.Errorf("project, artifact, version, and child address are required")
	}
	if len(input.Metadata) > 0 && !json.Valid(input.Metadata) {
		return nil, false, fmt.Errorf("artifact chunk metadata must be valid JSON")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM artifacts a JOIN artifact_versions v ON v.artifact_id=a.artifact_id WHERE a.project_ident=? AND a.artifact_id=? AND v.artifact_version_id=?`, projectIdent, artifactID, input.ArtifactVersionID).Scan(&exists)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if existing, err := scanArtifactChunk(tx.QueryRowContext(ctx, artifactChunkSelect+` WHERE artifact_version_id=? AND child_address=?`, input.ArtifactVersionID, input.ChildAddress)); err != nil {
		return nil, false, err
	} else if existing != nil {
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return existing, true, nil
	}
	var limits ArtifactWriteLimits
	if suppliedLimits == nil {
		limits, err = artifactWriteLimits()
		if err != nil {
			return nil, false, err
		}
	} else {
		limits = *suppliedLimits
	}
	if err := checkArtifactTextSize("chunk_text", input.Text, limits.ChunkTextMaxBytes); err != nil {
		return nil, false, err
	}
	if _, err := checkArtifactQuota(ctx, tx, projectIdent, "chunk", limits); err != nil {
		return nil, false, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UnixMilli()
	_, err = tx.ExecContext(ctx, `INSERT INTO artifact_chunks(chunk_id,artifact_id,artifact_version_id,child_address,text,embedding_model,embedding_vector,app,label,kind,metadata_json,superseded_by_chunk_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,NULL,?)`, id.String(), artifactID, input.ArtifactVersionID, input.ChildAddress, input.Text, input.EmbeddingModel, input.EmbeddingVector, input.App, input.Label, input.Kind, nullJSON(input.Metadata), now)
	if err != nil {
		return nil, false, err
	}
	item, err := scanArtifactChunk(tx.QueryRowContext(ctx, artifactChunkSelect+` WHERE chunk_id=?`, id.String()))
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return item, false, nil
}

func (s *Store) GetArtifactChunkByAddress(ctx context.Context, projectIdent, artifactID, versionID, address string) (*ArtifactChunk, error) {
	return scanArtifactChunk(s.db.QueryRowContext(ctx, artifactChunkSelect+` WHERE artifact_id=? AND artifact_version_id=? AND child_address=? AND EXISTS(SELECT 1 FROM artifacts WHERE artifact_id=? AND project_ident=?)`, artifactID, versionID, address, artifactID, projects.NormalizeIdent(projectIdent)))
}

func (s *Store) ListArtifactChunks(ctx context.Context, projectIdent, artifactID string, filter ArtifactChunkFilter) ([]ArtifactChunk, error) {
	query := artifactChunkSelect + ` WHERE artifact_id=? AND EXISTS(SELECT 1 FROM artifacts WHERE artifact_id=? AND project_ident=?)`
	args := []any{artifactID, artifactID, projects.NormalizeIdent(projectIdent)}
	if !filter.IncludeSuperseded {
		query += ` AND superseded_by_chunk_id IS NULL`
	}
	if filter.ArtifactVersionID != "" {
		query += ` AND artifact_version_id=?`
		args = append(args, filter.ArtifactVersionID)
	}
	for _, exact := range []struct{ column, value string }{{"app", filter.App}, {"label", filter.Label}, {"kind", filter.Kind}} {
		if exact.value != "" {
			query += ` AND ` + exact.column + `=?`
			args = append(args, exact.value)
		}
	}
	if filter.Query != "" {
		query += ` AND (child_address LIKE ? OR text LIKE ? OR COALESCE(metadata_json,'') LIKE ?)`
		like := "%" + filter.Query + "%"
		args = append(args, like, like, like)
	}
	query += ` ORDER BY created_at DESC, child_address ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ArtifactChunk{}
	for rows.Next() {
		item, err := scanArtifactChunk(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *item)
	}
	return result, rows.Err()
}

// MarkArtifactChunkSuperseded retains the old row and points it to its newer
// counterpart. Both rows must belong to the requested project and artifact.
func (s *Store) MarkArtifactChunkSuperseded(ctx context.Context, projectIdent, artifactID, oldChunkID, newChunkID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM artifacts a JOIN artifact_chunks old ON old.artifact_id=a.artifact_id JOIN artifact_chunks newer ON newer.artifact_id=a.artifact_id WHERE a.project_ident=? AND a.artifact_id=? AND old.chunk_id=? AND newer.chunk_id=?`, projects.NormalizeIdent(projectIdent), artifactID, oldChunkID, newChunkID).Scan(&exists)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE artifact_chunks SET superseded_by_chunk_id=? WHERE chunk_id=?`, newChunkID, oldChunkID); err != nil {
		return err
	}
	return tx.Commit()
}
