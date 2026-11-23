package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestArtifactChunkQuotaThresholdsAndLiveProjectCount(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artifact, firstVersion, secondVersion := artifactChunkFixture(t, s, "chunk-quota-a")
	otherArtifact, otherFirst, _ := artifactChunkFixture(t, s, "chunk-quota-b")
	limits := defaultArtifactWriteLimits()
	limits.ChunkSoft, limits.ChunkHard = 1, 2

	old, replay, err := s.createArtifactChunkWithLimits(ctx, "chunk-quota-a", artifact.ArtifactID, ArtifactChunkInsert{
		ArtifactVersionID: firstVersion.ArtifactVersionID, ChildAddress: "item", Text: "old",
	}, &limits)
	if err != nil || replay || old == nil {
		t.Fatalf("first chunk: chunk=%+v replay=%v err=%v", old, replay, err)
	}
	if _, _, err := s.createArtifactChunkWithLimits(ctx, "chunk-quota-b", otherArtifact.ArtifactID, ArtifactChunkInsert{
		ArtifactVersionID: otherFirst.ArtifactVersionID, ChildAddress: "item", Text: "other project",
	}, &limits); err != nil {
		t.Fatalf("other project chunk: %v", err)
	}
	assertChunkQuota(t, s, "chunk-quota-a", limits, 1, false)

	newer, replay, err := s.createArtifactChunkWithLimits(ctx, "chunk-quota-a", artifact.ArtifactID, ArtifactChunkInsert{
		ArtifactVersionID: secondVersion.ArtifactVersionID, ChildAddress: "item", Text: "new",
	}, &limits)
	if err != nil || replay || newer == nil {
		t.Fatalf("replacement chunk: chunk=%+v replay=%v err=%v", newer, replay, err)
	}
	if err := s.MarkArtifactChunkSuperseded(ctx, "chunk-quota-a", artifact.ArtifactID, old.ChunkID, newer.ChunkID); err != nil {
		t.Fatal(err)
	}
	assertChunkQuota(t, s, "chunk-quota-a", limits, 1, false)

	second, replay, err := s.createArtifactChunkWithLimits(ctx, "chunk-quota-a", artifact.ArtifactID, ArtifactChunkInsert{
		ArtifactVersionID: secondVersion.ArtifactVersionID, ChildAddress: "another-item", Text: "second live",
	}, &limits)
	if err != nil || replay || second == nil {
		t.Fatalf("second live chunk: chunk=%+v replay=%v err=%v", second, replay, err)
	}
	assertChunkQuota(t, s, "chunk-quota-a", limits, 2, true)
	assertChunkQuota(t, s, "chunk-quota-b", limits, 1, false)

	_, _, err = s.createArtifactChunkWithLimits(ctx, "chunk-quota-a", artifact.ArtifactID, ArtifactChunkInsert{
		ArtifactVersionID: secondVersion.ArtifactVersionID, ChildAddress: "over-hard", Text: "rejected",
	}, &limits)
	var quotaErr *ArtifactQuotaError
	if !errors.As(err, &quotaErr) || quotaErr.Token() != "quota_chunk_exceeded" || quotaErr.Current != 2 || quotaErr.Limit != 2 {
		t.Fatalf("hard chunk quota error: %#v", err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_chunks WHERE artifact_id=? AND superseded_by_chunk_id IS NULL`, artifact.ArtifactID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("hard reject changed live chunk count: %d, %v", count, err)
	}

	replayed, replay, err := s.createArtifactChunkWithLimits(ctx, "chunk-quota-a", artifact.ArtifactID, ArtifactChunkInsert{
		ArtifactVersionID: secondVersion.ArtifactVersionID, ChildAddress: "another-item", Text: strings.Repeat("x", int(limits.ChunkTextMaxBytes)+1),
	}, &limits)
	if err != nil || !replay || replayed == nil || replayed.ChunkID != second.ChunkID || replayed.Text != second.Text {
		t.Fatalf("natural-address replay at hard quota: chunk=%+v replay=%v err=%v", replayed, replay, err)
	}
}

func TestArtifactChunkQuotaDefaultsAndEnvironment(t *testing.T) {
	defaults, err := parseArtifactWriteLimits(func(string) (string, bool) { return "", false })
	if err != nil || defaults.ChunkSoft != 100000 || defaults.ChunkHard != 200000 {
		t.Fatalf("chunk quota defaults: %+v err=%v", defaults, err)
	}
	values := map[string]string{"PROJECT_CHUNK_SOFT": "7", "PROJECT_CHUNK_HARD": "9"}
	configured, err := parseArtifactWriteLimits(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil || configured.ChunkSoft != 7 || configured.ChunkHard != 9 {
		t.Fatalf("chunk quota env values: %+v err=%v", configured, err)
	}
	values["PROJECT_CHUNK_SOFT"], values["PROJECT_CHUNK_HARD"] = "10", "9"
	if _, err := parseArtifactWriteLimits(func(key string) (string, bool) { value, ok := values[key]; return value, ok }); err == nil || !strings.Contains(err.Error(), "PROJECT_CHUNK_SOFT") {
		t.Fatalf("inverted chunk quota thresholds: %v", err)
	}
}

func assertChunkQuota(t *testing.T, s *Store, project string, limits ArtifactWriteLimits, current uint64, hard bool) {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	warnings, err := checkArtifactQuota(context.Background(), tx, project, "chunk", limits)
	_ = tx.Rollback()
	if hard {
		var quotaErr *ArtifactQuotaError
		if !errors.As(err, &quotaErr) || quotaErr.Current != current {
			t.Fatalf("project %s hard quota current=%d error=%v", project, current, err)
		}
		return
	}
	if err != nil || len(warnings) != 1 || warnings[0].Token() != "quota_chunk_soft" || warnings[0].Current != current {
		t.Fatalf("project %s chunk quota warnings=%+v err=%v current=%d", project, warnings, err, current)
	}
}
