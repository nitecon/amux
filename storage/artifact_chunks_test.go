package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func artifactChunkFixture(t *testing.T, s *Store, project string) (*Artifact, *ArtifactVersion, *ArtifactVersion) {
	t.Helper()
	ctx := context.Background()
	if err := s.InsertProject(ctx, fixtureProject(project)); err != nil {
		t.Fatal(err)
	}
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "user", DisplayName: "Chunk fixture"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CreateArtifact(ctx, ArtifactInsert{ProjectIdent: project, Kind: "spec", Title: "Chunk fixture", CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	body := "body"
	first, err := s.CreateArtifactVersion(ctx, project, artifact.ArtifactID, ArtifactVersionInsert{Body: &body, CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateArtifactVersion(ctx, project, artifact.ArtifactID, ArtifactVersionInsert{ParentVersionID: &first.Record.ArtifactVersionID, Body: &body, CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	return artifact, first.Record, second.Record
}

func TestArtifactChunksAddressScopeAndSupersession(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artifact, first, second := artifactChunkFixture(t, s, "chunks")
	meta := json.RawMessage(`{"status":"ready"}`)
	old, replayed, err := s.CreateArtifactChunk(ctx, "CHUNKS", artifact.ArtifactID, ArtifactChunkInsert{
		ArtifactVersionID: first.ArtifactVersionID, ChildAddress: "manifest.items[T1]", Text: "old",
		App: artifactStringPtr("docs"), Label: artifactStringPtr("api"), Kind: artifactStringPtr("spec"), Metadata: meta,
	})
	if err != nil || old == nil || replayed || old.Metadata == nil || string(old.Metadata) != string(meta) {
		t.Fatalf("insert old chunk: %+v replayed=%v err=%v", old, replayed, err)
	}
	duplicate, replayed, err := s.CreateArtifactChunk(ctx, "chunks", artifact.ArtifactID, ArtifactChunkInsert{
		ArtifactVersionID: first.ArtifactVersionID, ChildAddress: "manifest.items[T1]", Text: strings.Repeat("x", (8<<10)+1),
	})
	if err != nil || !replayed || duplicate == nil || duplicate.ChunkID != old.ChunkID || duplicate.Text != "old" {
		t.Fatalf("natural address replay: %+v replayed=%v err=%v", duplicate, replayed, err)
	}
	if _, _, err := s.CreateArtifactChunk(ctx, "chunks", artifact.ArtifactID, ArtifactChunkInsert{
		ArtifactVersionID: first.ArtifactVersionID, ChildAddress: "manifest.items[oversized]", Text: strings.Repeat("x", (8<<10)+1),
	}); !isSizeLimitToken(err, "chunk_text_too_large") {
		t.Fatalf("new oversized chunk: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO artifact_chunks(chunk_id,artifact_id,artifact_version_id,child_address,text,created_at) VALUES('duplicate',?,?,?,?,?)`, artifact.ArtifactID, first.ArtifactVersionID, "manifest.items[T1]", "raw duplicate", 1); err == nil {
		t.Fatal("raw duplicate natural address bypassed unique constraint")
	}
	newer, replayed, err := s.CreateArtifactChunk(ctx, "chunks", artifact.ArtifactID, ArtifactChunkInsert{
		ArtifactVersionID: second.ArtifactVersionID, ChildAddress: "manifest.items[T1]", Text: "new",
	})
	if err != nil || replayed || newer == nil {
		t.Fatalf("insert newer: %+v replayed=%v err=%v", newer, replayed, err)
	}
	if err := s.MarkArtifactChunkSuperseded(ctx, "chunks", artifact.ArtifactID, old.ChunkID, newer.ChunkID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetArtifactChunkByAddress(ctx, "chunks", artifact.ArtifactID, first.ArtifactVersionID, "manifest.items[T1]"); err != nil || got == nil || got.SupersededByChunkID == nil || *got.SupersededByChunkID != newer.ChunkID {
		t.Fatalf("address lookup lost supersession: %+v %v", got, err)
	}
	if got, err := s.GetArtifactChunkByAddress(ctx, "chunks", artifact.ArtifactID, first.ArtifactVersionID, "manifest.items[T1]"); err != nil || got == nil || got.Text != "old" || got.ArtifactVersionID != first.ArtifactVersionID {
		t.Fatalf("superseded history changed: %+v %v", got, err)
	}
	current, err := s.ListArtifactChunks(ctx, "chunks", artifact.ArtifactID, ArtifactChunkFilter{})
	if err != nil || len(current) != 1 || current[0].ChunkID != newer.ChunkID {
		t.Fatalf("current chunks: %+v %v", current, err)
	}
	history, err := s.ListArtifactChunks(ctx, "chunks", artifact.ArtifactID, ArtifactChunkFilter{IncludeSuperseded: true, ArtifactVersionID: first.ArtifactVersionID})
	if err != nil || len(history) != 1 || history[0].ChunkID != old.ChunkID {
		t.Fatalf("historical chunks: %+v %v", history, err)
	}
	if cross, err := s.ListArtifactChunks(ctx, "other-project", artifact.ArtifactID, ArtifactChunkFilter{IncludeSuperseded: true}); err != nil || len(cross) != 0 {
		t.Fatalf("cross-project list leaked: %+v %v", cross, err)
	}
	if cross, err := s.GetArtifactChunkByAddress(ctx, "other-project", artifact.ArtifactID, first.ArtifactVersionID, "manifest.items[T1]"); err != nil || cross != nil {
		t.Fatalf("cross-project address lookup leaked: %+v %v", cross, err)
	}
	if err := s.MarkArtifactChunkSuperseded(ctx, "other-project", artifact.ArtifactID, old.ChunkID, newer.ChunkID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetArtifactChunkByAddress(ctx, "chunks", artifact.ArtifactID, first.ArtifactVersionID, "manifest.items[T1]"); got == nil || got.SupersededByChunkID == nil {
		t.Fatalf("foreign project superseded chunk: %+v", got)
	}
}

func TestArtifactChunksMigrationPreservesExistingRows(t *testing.T) {
	s := testStore(t)
	artifact, first, _ := artifactChunkFixture(t, s, "chunk-migration")
	chunk, _, err := s.CreateArtifactChunk(context.Background(), "chunk-migration", artifact.ArtifactID, ArtifactChunkInsert{ArtifactVersionID: first.ArtifactVersionID, ChildAddress: "section", Text: "retained"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateArtifactChunks(context.Background(), tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetArtifactChunkByAddress(context.Background(), "chunk-migration", artifact.ArtifactID, first.ArtifactVersionID, "section")
	if err != nil || got == nil || got.ChunkID != chunk.ChunkID || got.Text != "retained" {
		t.Fatalf("migration did not preserve chunk: %+v %v", got, err)
	}
}
