package storage

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestArtifactLinksMigrationPreservesExistingRustRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "artifact-links.sqlite")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	artifact, version, _ := artifactChunkFixture(t, s, "artifact-link-migration")
	_, err = s.db.ExecContext(ctx, `INSERT INTO artifact_links(link_id,link_type,source_kind,source_id,source_version_id,source_child_address,
target_kind,target_id,target_version_id,target_child_address,created_by_actor_id,created_via_workflow_run_id,idempotency_key,supersedes_link_id,created_at)
VALUES('rust-link','task_generated_from_spec','task','task-rust',NULL,NULL,'artifact_version',?,?, 'manifest.items[T1]',?,NULL,NULL,NULL,123)`, version.ArtifactVersionID, version.ArtifactVersionID, artifact.CreatedByActorID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM amux_go_migrations WHERE version=41"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	link, err := s.ArtifactLink(ctx, "rust-link")
	if err != nil || link == nil || link.CreatedAt != 123 || link.SourceID != "task-rust" || link.TargetVersionID == nil || *link.TargetVersionID != version.ArtifactVersionID {
		t.Fatalf("Rust-format artifact link was not retained: %+v %v", link, err)
	}
	var recorded int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_go_migrations WHERE version=41").Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("migration 41 record count=%d err=%v", recorded, err)
	}
	if artifact.ArtifactID == "" {
		t.Fatal("fixture artifact was not created")
	}
}

func TestArtifactLinkValidationProjectResolutionAndVersionAnchors(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a, va, _ := artifactChunkFixture(t, s, "link-validation-a")
	b, vb, _ := artifactChunkFixture(t, s, "link-validation-b")
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "user", DisplayName: "Link actor"})
	if err != nil {
		t.Fatal(err)
	}
	base := ArtifactLinkInsert{LinkType: "spec_implements_design", SourceKind: "artifact_version", SourceID: va.ArtifactVersionID,
		TargetKind: "artifact_version", TargetID: vb.ArtifactVersionID, CreatedByActorID: actor.ActorID}
	if _, err := s.CreateArtifactLink(ctx, base); err == nil || err.Error() != "audit-path link requires source_version_id" {
		t.Fatalf("missing audit source version: %v", err)
	}
	base.SourceVersionID = &va.ArtifactVersionID
	if _, err := s.CreateArtifactLink(ctx, base); err == nil || err.Error() != "audit-path link requires target_version_id" {
		t.Fatalf("missing audit target version: %v", err)
	}
	base.TargetVersionID = &vb.ArtifactVersionID
	if _, err := s.CreateArtifactLink(ctx, base); err != nil {
		t.Fatalf("valid cross-project audit link rejected: %v", err)
	}

	workflowRunID := uuid.NewString()
	withoutKey := ArtifactLinkInsert{LinkType: "doc_referenced_by_spec", SourceKind: "artifact", SourceID: a.ArtifactID,
		TargetKind: "external_url", TargetID: "https://example.test/doc", CreatedByActorID: actor.ActorID, CreatedViaWorkflowRunID: &workflowRunID}
	if _, err := s.CreateArtifactLink(ctx, withoutKey); err == nil || err.Error() != "workflow-emitted links require idempotency_key" {
		t.Fatalf("workflow link without key accepted: %v", err)
	}

	foreignSupersedes := ArtifactLinkInsert{LinkType: "supersedes_artifact", SourceKind: "artifact", SourceID: a.ArtifactID,
		TargetKind: "artifact", TargetID: b.ArtifactID, CreatedByActorID: actor.ActorID}
	if _, err := s.CreateArtifactLink(ctx, foreignSupersedes); err == nil || err.Error() != "supersedes_artifact requires artifacts in the same project" {
		t.Fatalf("cross-project artifact supersession accepted: %v", err)
	}
	foreignSupersedes.TargetID = a.ArtifactID
	if _, err := s.CreateArtifactLink(ctx, foreignSupersedes); err != nil {
		t.Fatalf("same-project artifact supersession rejected: %v", err)
	}
	if _, err := s.CreateArtifactLink(ctx, ArtifactLinkInsert{LinkType: "doc_referenced_by_spec", SourceKind: "external_url",
		SourceID: "https://example.test/source", TargetKind: "external_url", TargetID: "https://example.test/target", CreatedByActorID: actor.ActorID}); err == nil || err.Error() != "link project not resolvable" {
		t.Fatalf("unanchored link accepted: %v", err)
	}
}

func TestArtifactLinkVisibilityUsesBothEndpointsAndVersionAnchors(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a, va, _ := artifactChunkFixture(t, s, "link-visible-a")
	b, vb, _ := artifactChunkFixture(t, s, "link-visible-b")
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "user", DisplayName: "Visibility actor"})
	if err != nil {
		t.Fatal(err)
	}
	create := func(input ArtifactLinkInsert) *ArtifactLink {
		t.Helper()
		write, err := s.CreateArtifactLink(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		return write.Record
	}
	aSource := create(ArtifactLinkInsert{LinkType: "doc_referenced_by_spec", SourceKind: "artifact", SourceID: a.ArtifactID,
		TargetKind: "external_url", TargetID: "https://example.test/a-source", CreatedByActorID: actor.ActorID})
	aTarget := create(ArtifactLinkInsert{LinkType: "doc_referenced_by_spec", SourceKind: "external_url", SourceID: "https://example.test/a-target",
		TargetKind: "artifact", TargetID: a.ArtifactID, CreatedByActorID: actor.ActorID})
	aVersion := create(ArtifactLinkInsert{LinkType: "doc_referenced_by_spec", SourceKind: "external_url", SourceID: "https://example.test/a-version",
		SourceVersionID: &va.ArtifactVersionID, TargetKind: "external_url", TargetID: "https://example.test/external", CreatedByActorID: actor.ActorID})
	aTargetVersion := create(ArtifactLinkInsert{LinkType: "doc_referenced_by_spec", SourceKind: "external_url", SourceID: "https://example.test/external",
		TargetKind: "external_url", TargetID: "https://example.test/a-target-version", TargetVersionID: &va.ArtifactVersionID, CreatedByActorID: actor.ActorID})
	cross := create(ArtifactLinkInsert{LinkType: "doc_referenced_by_spec", SourceKind: "artifact", SourceID: a.ArtifactID,
		TargetKind: "artifact", TargetID: b.ArtifactID, CreatedByActorID: actor.ActorID})
	bVersion := create(ArtifactLinkInsert{LinkType: "doc_referenced_by_spec", SourceKind: "external_url", SourceID: "https://example.test/external",
		TargetKind: "external_url", TargetID: "https://example.test/b-version", TargetVersionID: &vb.ArtifactVersionID, CreatedByActorID: actor.ActorID})
	_, err = s.db.ExecContext(ctx, `INSERT INTO artifact_links(link_id,link_type,source_kind,source_id,target_kind,target_id,created_by_actor_id,created_at)
VALUES('external-only','doc_referenced_by_spec','external_url','https://example.test/x','external_url','https://example.test/y',?,1)`, actor.ActorID)
	if err != nil {
		t.Fatal(err)
	}
	listA, err := s.ListArtifactLinks(ctx, "LINK-VISIBLE-A", ArtifactLinkFilter{})
	if err != nil {
		t.Fatal(err)
	}
	listB, err := s.ListArtifactLinks(ctx, "link-visible-b", ArtifactLinkFilter{LinkType: " doc_referenced_by_spec "})
	if err != nil {
		t.Fatal(err)
	}
	ids := func(items []ArtifactLink) map[string]bool {
		result := make(map[string]bool, len(items))
		for _, item := range items {
			result[item.LinkID] = true
		}
		return result
	}
	idsA, idsB := ids(listA), ids(listB)
	for _, link := range []*ArtifactLink{aSource, aTarget, aVersion, aTargetVersion, cross} {
		if !idsA[link.LinkID] {
			t.Errorf("project A did not see %s", link.LinkID)
		}
	}
	if len(listA) != 5 || idsA["external-only"] || idsA[bVersion.LinkID] {
		t.Fatalf("project A link visibility leaked: %+v", listA)
	}
	if !idsB[cross.LinkID] || !idsB[bVersion.LinkID] || idsB[aSource.LinkID] || idsB["external-only"] || len(listB) != 2 {
		t.Fatalf("project B link visibility incorrect: %+v", listB)
	}
	for project, want := range map[string]int{"link-visible-a": len(listA), "link-visible-b": len(listB), "unrelated": 0} {
		count, err := s.CountArtifactLinks(ctx, project)
		if err != nil || count != int64(want) {
			t.Errorf("project %s link quota count = %d, %v; want %d", project, count, err, want)
		}
	}
}

func TestArtifactLinkWorkflowMutationReplayIsRaceSafe(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artifact, version, _ := artifactChunkFixture(t, s, "link-replay")
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", DisplayName: "Link writer"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.StartWorkflowRun(ctx, WorkflowRunInsert{ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_task_generation", CoordinatorActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	key := "link-operation-1"
	input := ArtifactLinkInsert{LinkType: "supersedes_version", SourceKind: "artifact_version", SourceID: version.ArtifactVersionID,
		SourceVersionID: &version.ArtifactVersionID, TargetKind: "artifact_version", TargetID: version.ArtifactVersionID,
		TargetVersionID: &version.ArtifactVersionID, CreatedByActorID: actor.ActorID,
		CreatedViaWorkflowRunID: &run.Record.WorkflowRunID, IdempotencyKey: &key}
	const callers = 24
	results := make(chan *ArtifactLinkWrite, callers)
	errors := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.CreateArtifactLink(ctx, input)
			if err != nil {
				errors <- err
				return
			}
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	var linkID string
	created := 0
	for result := range results {
		if linkID == "" {
			linkID = result.Record.LinkID
		}
		if result.Record.LinkID != linkID {
			t.Fatalf("concurrent operation created two links: %s and %s", linkID, result.Record.LinkID)
		}
		if !result.Replayed {
			created++
		}
	}
	if linkID == "" || created != 1 {
		t.Fatalf("concurrent replay winners: link=%q created=%d, want exactly one", linkID, created)
	}
	// Rust replays by operation identity after validation and returns the first
	// durable row even when a retry carries different link endpoints.
	changed := input
	changed.LinkType, changed.SourceKind, changed.SourceID = "doc_referenced_by_spec", "external_url", "https://example.test/unresolvable"
	changed.SourceVersionID, changed.TargetKind, changed.TargetID, changed.TargetVersionID = nil, "external_url", "https://example.test/other", nil
	replay, err := s.CreateArtifactLink(ctx, changed)
	if err != nil || replay == nil || !replay.Replayed || replay.Record.LinkID != linkID || replay.Record.SourceID != version.ArtifactVersionID {
		t.Fatalf("operation replay changed the original artifact link: %+v %v", replay, err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_links WHERE created_via_workflow_run_id=? AND idempotency_key=?`, run.Record.WorkflowRunID, key).Scan(&count); err != nil || count != 1 {
		t.Fatalf("stored links for operation=%d err=%v", count, err)
	}
}

func TestArtifactLinkRunKeyIndexAllowsRunlessKeysAndRejectsDuplicateRunKeys(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artifact, _, _ := artifactChunkFixture(t, s, "link-key-index")
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "user", DisplayName: "Link index actor"})
	if err != nil {
		t.Fatal(err)
	}
	version, err := s.CreateArtifactVersion(ctx, "link-key-index", artifact.ArtifactID, ArtifactVersionInsert{CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	versionID := version.Record.ArtifactVersionID
	key := "same-key"
	workflow, err := s.StartWorkflowRun(ctx, WorkflowRunInsert{ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_task_generation", CoordinatorActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	insert := func(id string, run, key *string) error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO artifact_links(link_id,link_type,source_kind,source_id,source_version_id,target_kind,target_id,target_version_id,created_by_actor_id,created_via_workflow_run_id,idempotency_key,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,1)`, id, "supersedes_version", "artifact_version", versionID, versionID, "artifact_version", versionID, versionID, actor.ActorID, run, key)
		return err
	}
	if err := insert("run-key-one", &workflow.Record.WorkflowRunID, &key); err != nil {
		t.Fatal(err)
	}
	if err := insert("run-key-two", &workflow.Record.WorkflowRunID, &key); err == nil {
		t.Fatal("duplicate (run,key) bypassed the unique index")
	}
	runlessKey := "repeat"
	if err := insert("runless-one", nil, &runlessKey); err != nil {
		t.Fatal(err)
	}
	if err := insert("runless-two", nil, &runlessKey); err != nil {
		t.Fatalf("partial index constrained runless key: %v", err)
	}
}
