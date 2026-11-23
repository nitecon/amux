package storage

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactWriteLimitsDefaultsAndEnvironment(t *testing.T) {
	defaults, err := parseArtifactWriteLimits(func(string) (string, bool) { return "", false })
	if err != nil || defaults.VersionBodyMaxBytes != 1<<20 || defaults.StructuredPayloadMaxBytes != 512<<10 || defaults.ContributionBodyMaxBytes != 256<<10 || defaults.CommentBodyMaxBytes != 32<<10 || defaults.ReadSetMaxRefs != 256 || defaults.LabelsMaxCount != 32 || defaults.LabelMaxBytes != 64 || defaults.ArtifactSoft != 5000 || defaults.ArtifactHard != 10000 || defaults.VersionSoft != 50000 || defaults.VersionHard != 100000 || defaults.LinkSoft != 200000 || defaults.LinkHard != 400000 {
		t.Fatalf("unexpected artifact limits: %+v, %v", defaults, err)
	}
	values := map[string]string{
		"ARTIFACT_VERSION_BODY_MAX_BYTES": "41", "ARTIFACT_VERSION_STRUCTURED_PAYLOAD_MAX_BYTES": "42",
		"CONTRIBUTION_BODY_MAX_BYTES": "43", "COMMENT_BODY_MAX_BYTES": "44", "READ_SET_MAX_REFS": "45",
		"ARTIFACT_LABELS_MAX_COUNT": "3", "ARTIFACT_LABEL_MAX_BYTES": "4",
		"PROJECT_ARTIFACT_SOFT": " 5 ", "PROJECT_ARTIFACT_HARD": "6",
		"PROJECT_VERSION_SOFT": "7", "PROJECT_VERSION_HARD": "8",
		"PROJECT_LINK_SOFT": "9", "PROJECT_LINK_HARD": "10",
	}
	configured, err := parseArtifactWriteLimits(func(key string) (string, bool) { v, ok := values[key]; return v, ok })
	if err != nil || configured.VersionBodyMaxBytes != 41 || configured.StructuredPayloadMaxBytes != 42 || configured.ContributionBodyMaxBytes != 43 || configured.CommentBodyMaxBytes != 44 || configured.ReadSetMaxRefs != 45 || configured.LabelsMaxCount != 3 || configured.LabelMaxBytes != 4 || configured.ArtifactSoft != 5 || configured.ArtifactHard != 6 || configured.VersionSoft != 7 || configured.VersionHard != 8 || configured.LinkSoft != 9 || configured.LinkHard != 10 {
		t.Fatalf("environment values not applied: %+v, %v", configured, err)
	}
	values["PROJECT_ARTIFACT_SOFT"] = "7"
	if _, err := parseArtifactWriteLimits(func(key string) (string, bool) { v, ok := values[key]; return v, ok }); err == nil {
		t.Fatal("inverted thresholds should fail")
	}
	values["PROJECT_ARTIFACT_SOFT"] = "0"
	if _, err := parseArtifactWriteLimits(func(key string) (string, bool) { v, ok := values[key]; return v, ok }); err == nil {
		t.Fatal("zero limit should fail")
	}
	values["PROJECT_ARTIFACT_SOFT"] = "5"
	values["CONTRIBUTION_BODY_MAX_BYTES"] = "not-a-number"
	if _, err := parseArtifactWriteLimits(func(key string) (string, bool) { v, ok := values[key]; return v, ok }); err == nil ||
		!strings.Contains(err.Error(), "CONTRIBUTION_BODY_MAX_BYTES") {
		t.Fatalf("malformed contribution size limit: %v", err)
	}
	values["CONTRIBUTION_BODY_MAX_BYTES"] = "43"
	values["CHUNK_TEXT_MAX_BYTES"] = "0"
	if _, err := parseArtifactWriteLimits(func(key string) (string, bool) { v, ok := values[key]; return v, ok }); err == nil ||
		!strings.Contains(err.Error(), "CHUNK_TEXT_MAX_BYTES") {
		t.Fatalf("zero chunk size limit: %v", err)
	}
	delete(values, "CHUNK_TEXT_MAX_BYTES")
	values["PROJECT_LINK_SOFT"], values["PROJECT_LINK_HARD"] = "11", "10"
	if _, err := parseArtifactWriteLimits(func(key string) (string, bool) { v, ok := values[key]; return v, ok }); err == nil || !strings.Contains(err.Error(), "PROJECT_LINK_SOFT") {
		t.Fatalf("inverted link thresholds should fail clearly: %v", err)
	}
}

func TestDiscourseBodyLimitsRejectWritesButAllowOversizedReplays(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "discourse-limits.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.InsertProject(ctx, fixtureProject("discourse-limits")); err != nil {
		t.Fatal(err)
	}
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", DisplayName: "Limit test"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CreateArtifact(ctx, ArtifactInsert{ProjectIdent: "discourse-limits", Kind: "spec", Title: "Discourse", CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	limits := defaultArtifactWriteLimits()
	limits.CommentBodyMaxBytes = 5
	commentKey := "comment-key"
	commentInput := ArtifactCommentInsert{TargetKind: "artifact", TargetID: artifact.ArtifactID, ActorID: actor.ActorID, Body: "first", IdempotencyKey: &commentKey}
	firstComment, replayed, err := s.addArtifactCommentWithLimits(ctx, "discourse-limits", artifact.ArtifactID, commentInput, &limits)
	if err != nil || firstComment == nil || replayed {
		t.Fatalf("initial comment: %+v replayed=%v err=%v", firstComment, replayed, err)
	}
	commentInput.Body = "oversized comment"
	replayedComment, replayed, err := s.addArtifactCommentWithLimits(ctx, "discourse-limits", artifact.ArtifactID, commentInput, &limits)
	if err != nil || replayedComment == nil || !replayed || replayedComment.CommentID != firstComment.CommentID {
		t.Fatalf("oversized comment replay should return stored row: %+v replayed=%v err=%v", replayedComment, replayed, err)
	}
	commentInput.IdempotencyKey = artifactStringPtr("new-comment-key")
	commentInput.Body = "second"
	_, _, err = s.addArtifactCommentWithLimits(ctx, "discourse-limits", artifact.ArtifactID, commentInput, &limits)
	var sizeErr *ArtifactSizeLimitError
	if !errors.As(err, &sizeErr) || sizeErr.Kind != "comment_body" || sizeErr.Limit != 5 || sizeErr.Actual != 6 || sizeErr.Token() != "comment_body_too_large" {
		t.Fatalf("new oversized comment error = %#v", err)
	}

	limits.ContributionBodyMaxBytes = 5
	contributionKey := "contribution-key"
	contributionInput := ArtifactContributionInsert{TargetKind: "artifact", TargetID: artifact.ArtifactID, ContributionKind: "note", Role: "author", ActorID: actor.ActorID, BodyFormat: "markdown", Body: "first", IdempotencyKey: &contributionKey}
	firstContribution, replayed, err := s.addArtifactContributionWithLimits(ctx, "discourse-limits", artifact.ArtifactID, contributionInput, &limits)
	if err != nil || firstContribution == nil || replayed {
		t.Fatalf("initial contribution: %+v replayed=%v err=%v", firstContribution, replayed, err)
	}
	contributionInput.Body = "oversized contribution"
	replayedContribution, replayed, err := s.addArtifactContributionWithLimits(ctx, "discourse-limits", artifact.ArtifactID, contributionInput, &limits)
	if err != nil || replayedContribution == nil || !replayed || replayedContribution.ContributionID != firstContribution.ContributionID {
		t.Fatalf("oversized contribution replay should return stored row: %+v replayed=%v err=%v", replayedContribution, replayed, err)
	}
	contributionInput.IdempotencyKey = artifactStringPtr("new-contribution-key")
	if _, _, err := s.addArtifactContributionWithLimits(ctx, "discourse-limits", artifact.ArtifactID, contributionInput, &limits); !isSizeLimitToken(err, "contribution_body_too_large") {
		t.Fatalf("new oversized contribution error = %v", err)
	}
	limits = defaultArtifactWriteLimits()
	limits.ReadSetMaxRefs = 1
	contributionInput.Body = "small"
	contributionInput.ReadSet = json.RawMessage(`{"manifest_items":["one","two"]}`)
	contributionInput.IdempotencyKey = artifactStringPtr("too-many-refs")
	if _, _, err := s.addArtifactContributionWithLimits(ctx, "discourse-limits", artifact.ArtifactID, contributionInput, &limits); !isSizeLimitToken(err, "read_set_too_large") {
		t.Fatalf("oversized contribution read_set error = %v", err)
	}
}

func isSizeLimitToken(err error, token string) bool {
	var limitErr *ArtifactSizeLimitError
	return errors.As(err, &limitErr) && limitErr.Token() == token
}

func TestArtifactQuotaCountsAndWarningThresholds(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "limits.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.InsertProject(ctx, fixtureProject("quota-test")); err != nil {
		t.Fatal(err)
	}
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", DisplayName: "Quota test"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateArtifact(ctx, ArtifactInsert{ProjectIdent: "quota-test", Kind: "spec", Title: "A", CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	archived, err := s.CreateArtifact(ctx, ArtifactInsert{ProjectIdent: "quota-test", Kind: "spec", Title: "B", CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE artifacts SET lifecycle_state='archived' WHERE artifact_id=?`, archived.ArtifactID); err != nil {
		t.Fatal(err)
	}
	body := "version"
	if _, err := s.CreateArtifactVersion(ctx, "quota-test", a.ArtifactID, ArtifactVersionInsert{Body: &body, CreatedByActorID: actor.ActorID}); err != nil {
		t.Fatal(err)
	}
	limits := defaultArtifactWriteLimits()
	limits.ArtifactSoft, limits.ArtifactHard = 1, 2
	limits.VersionSoft, limits.VersionHard = 1, 2
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	belowSoft, err := checkArtifactQuota(ctx, tx, "unrelated-project", "artifact", limits)
	if err != nil || len(belowSoft) != 0 {
		_ = tx.Rollback()
		t.Fatalf("quota warned below the soft threshold: %+v %v", belowSoft, err)
	}
	artifactWarnings, err := checkArtifactQuota(ctx, tx, "quota-test", "artifact", limits)
	if err != nil {
		t.Fatal(err)
	}
	versionWarnings, err := checkArtifactQuota(ctx, tx, "quota-test", "version", limits)
	_ = tx.Rollback()
	if err != nil || len(artifactWarnings) != 1 || artifactWarnings[0].Token() != "quota_artifact_soft" || artifactWarnings[0].Current != 1 || len(versionWarnings) != 1 || versionWarnings[0].Token() != "quota_version_soft" || versionWarnings[0].Current != 1 {
		t.Fatalf("unexpected quota warnings: artifact=%+v version=%+v err=%v", artifactWarnings, versionWarnings, err)
	}
	limits.ArtifactHard = 1
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = checkArtifactQuota(ctx, tx, "quota-test", "artifact", limits)
	_ = tx.Rollback()
	var quotaErr *ArtifactQuotaError
	if !errors.As(err, &quotaErr) || quotaErr.Token() != "quota_artifact_exceeded" || quotaErr.Current != 1 {
		t.Fatalf("hard quota did not reject at threshold: %#v", err)
	}
}

func TestArtifactWriteSizeChecksUseBytesAndCompactPayload(t *testing.T) {
	limits := defaultArtifactWriteLimits()
	limits.LabelsMaxCount = 1
	limits.LabelMaxBytes = 2
	if err := checkArtifactLabels([]string{"é"}, limits); err != nil {
		t.Fatalf("two UTF-8 bytes should fit: %v", err)
	}
	err := checkArtifactLabels([]string{"abc"}, limits)
	var sizeErr *ArtifactSizeLimitError
	if !errors.As(err, &sizeErr) || sizeErr.Kind != "label_too_long" || sizeErr.Limit != 2 || sizeErr.Actual != 3 {
		t.Fatalf("unexpected label size error: %#v", err)
	}
	if sizeErr.Token() != "label_too_long" {
		t.Fatalf("label token should be canonical, got %q", sizeErr.Token())
	}
	limits.LabelsMaxCount = 0
	err = checkArtifactLabels([]string{"a"}, limits)
	if !errors.As(err, &sizeErr) || sizeErr.Token() != "labels_too_many" {
		t.Fatalf("unexpected labels count token: %#v", err)
	}
	body := "123"
	limits.VersionBodyMaxBytes = 2
	if err := checkArtifactVersionSize(&body, nil, limits); !errors.As(err, &sizeErr) || sizeErr.Kind != "artifact_version_body" {
		t.Fatalf("unexpected body size error: %#v", err)
	}
	limits.VersionBodyMaxBytes = 3
	limits.StructuredPayloadMaxBytes = 7
	if err := checkArtifactVersionSize(&body, json.RawMessage(` { "a" : 1 } `), limits); err != nil {
		t.Fatalf("payload should be measured after JSON serialization: %v", err)
	}
}

func TestArtifactWritesEnforceLimitsAndVersionReplayPrecedesSizeCheck(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "write-limits.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.InsertProject(ctx, fixtureProject("write-limits")); err != nil {
		t.Fatal(err)
	}
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", DisplayName: "Limits test"})
	if err != nil {
		t.Fatal(err)
	}
	tooManyLabels := make([]string, 33)
	if _, err := s.CreateArtifactWrite(ctx, ArtifactInsert{ProjectIdent: "write-limits", Kind: "spec", Title: "Rejected", Labels: tooManyLabels, CreatedByActorID: actor.ActorID}); err == nil {
		t.Fatal("artifact with too many labels was accepted")
	} else {
		var limitErr *ArtifactSizeLimitError
		if !errors.As(err, &limitErr) || limitErr.Token() != "labels_too_many" {
			t.Fatalf("unexpected label limit error: %v", err)
		}
	}
	tooLongLabel := strings.Repeat("x", 65)
	if _, err := s.CreateArtifactWrite(ctx, ArtifactInsert{ProjectIdent: "write-limits", Kind: "spec", Title: "Rejected label", Labels: []string{tooLongLabel}, CreatedByActorID: actor.ActorID}); err == nil {
		t.Fatal("artifact with an oversized label was accepted")
	} else {
		var limitErr *ArtifactSizeLimitError
		if !errors.As(err, &limitErr) || limitErr.Token() != "label_too_long" {
			t.Fatalf("unexpected label byte limit error: %v", err)
		}
	}
	write, err := s.CreateArtifactWrite(ctx, ArtifactInsert{ProjectIdent: "write-limits", Kind: "spec", Title: "Accepted", CreatedByActorID: actor.ActorID})
	if err != nil || write == nil || write.Record == nil || len(write.Warnings) != 0 {
		t.Fatalf("artifact write result: %+v %v", write, err)
	}
	artifactLimits := defaultArtifactWriteLimits()
	artifactLimits.ArtifactSoft, artifactLimits.ArtifactHard = 1, 1
	if _, err := s.createArtifactWriteWithLimits(ctx, ArtifactInsert{ProjectIdent: "write-limits", Kind: "spec", Title: "Quota reject", CreatedByActorID: actor.ActorID}, artifactLimits); err == nil {
		t.Fatal("artifact at hard quota was accepted")
	} else {
		var quotaErr *ArtifactQuotaError
		if !errors.As(err, &quotaErr) || quotaErr.Token() != "quota_artifact_exceeded" {
			t.Fatalf("unexpected artifact hard quota error: %v", err)
		}
	}
	var artifactCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE project_ident='write-limits' AND lifecycle_state!='archived'`).Scan(&artifactCount); err != nil || artifactCount != 1 {
		t.Fatalf("hard quota changed artifact rows: count=%d err=%v", artifactCount, err)
	}
	oversize := strings.Repeat("x", (1<<20)+1)
	if _, err := s.CreateArtifactVersion(ctx, "write-limits", write.Record.ArtifactID, ArtifactVersionInsert{Body: &oversize, CreatedByActorID: actor.ActorID}); err == nil {
		t.Fatal("oversize artifact version body was accepted")
	} else {
		var limitErr *ArtifactSizeLimitError
		if !errors.As(err, &limitErr) || limitErr.Token() != "artifact_version_body_too_large" {
			t.Fatalf("unexpected version body limit error: %v", err)
		}
	}
	run, err := s.StartWorkflowRun(ctx, WorkflowRunInsert{ArtifactID: write.Record.ArtifactID, WorkflowKind: "spec_iteration", CoordinatorActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	body, key := "small body", "replay-key"
	input := ArtifactVersionInsert{Body: &body, CreatedByActorID: actor.ActorID, CreatedViaWorkflowRunID: &run.Record.WorkflowRunID, IdempotencyKey: &key}
	versionLimits := defaultArtifactWriteLimits()
	versionLimits.VersionBodyMaxBytes = uint64(len(body))
	versionLimits.VersionSoft, versionLimits.VersionHard = 1, 1
	first, err := s.createArtifactVersionWithLimits(ctx, "write-limits", write.Record.ArtifactID, input, &versionLimits, false)
	if err != nil || first == nil || first.Replayed {
		t.Fatalf("initial version: %+v %v", first, err)
	}
	secondBody, secondKey := "second", "second-key"
	_, err = s.createArtifactVersionWithLimits(ctx, "write-limits", write.Record.ArtifactID, ArtifactVersionInsert{Body: &secondBody, CreatedByActorID: actor.ActorID, CreatedViaWorkflowRunID: &run.Record.WorkflowRunID, IdempotencyKey: &secondKey}, &versionLimits, false)
	var quotaErr *ArtifactQuotaError
	if !errors.As(err, &quotaErr) || quotaErr.Token() != "quota_version_exceeded" {
		t.Fatalf("new version at hard quota should be rejected: %v", err)
	}
	input.Body = &oversize
	replay, err := s.createArtifactVersionWithLimits(ctx, "write-limits", write.Record.ArtifactID, input, &versionLimits, false)
	if err != nil || replay == nil || !replay.Replayed || replay.Record.ArtifactVersionID != first.Record.ArtifactVersionID || len(replay.Warnings) != 0 {
		t.Fatalf("oversize replay should return existing version without quota checks: %+v %v", replay, err)
	}
}
