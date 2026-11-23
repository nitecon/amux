package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestArtifactCommentProjectVersionParentReplayAndLifecycle(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artifact, _, _ := artifactChunkFixture(t, s, "discourse")
	body := "review me"
	reviewVersion, err := s.CreateArtifactVersion(ctx, artifact.ProjectIdent, artifact.ArtifactID, ArtifactVersionInsert{
		CreatedByActorID: artifact.CreatedByActorID, VersionState: "under_review", Body: &body,
	})
	if err != nil {
		t.Fatal(err)
	}
	version := reviewVersion.Record
	otherArtifact, otherVersion, _ := artifactChunkFixture(t, s, "other-discourse")
	key := "comment-once"
	address := "manifest.items[stable-id]"
	comment, replayed, err := s.AddArtifactComment(ctx, "DISCOURSE", artifact.ArtifactID, ArtifactCommentInsert{
		TargetKind: "artifact_version", TargetID: version.ArtifactVersionID, ChildAddress: &address,
		ActorID: artifact.CreatedByActorID, Body: "review this item", IdempotencyKey: &key,
	})
	if err != nil || comment == nil || replayed || comment.State != "open" || comment.TargetID != version.ArtifactVersionID ||
		comment.ChildAddress == nil || *comment.ChildAddress != address {
		t.Fatalf("add version comment: %+v replayed=%v err=%v", comment, replayed, err)
	}
	again, replayed, err := s.AddArtifactComment(ctx, "discourse", artifact.ArtifactID, ArtifactCommentInsert{
		TargetKind: "artifact_version", TargetID: version.ArtifactVersionID, ActorID: artifact.CreatedByActorID, Body: "replay body", IdempotencyKey: &key,
	})
	if err != nil || !replayed || again.CommentID != comment.CommentID || again.Body != comment.Body {
		t.Fatalf("comment replay: %+v replayed=%v err=%v", again, replayed, err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO artifact_comments(comment_id,artifact_id,target_kind,target_id,child_address,actor_id,body,idempotency_key,created_at)
 VALUES('duplicate',?,'artifact_version',?,?,?,?,?,1)`, artifact.ArtifactID, version.ArtifactVersionID, address, artifact.CreatedByActorID, "duplicate comment", key); err == nil {
		t.Fatal("duplicate anchor and actor key bypassed the unique index")
	}
	if _, _, err := s.AddArtifactComment(ctx, "discourse", artifact.ArtifactID, ArtifactCommentInsert{TargetKind: "artifact", TargetID: artifact.ArtifactID, ChildAddress: &address, ActorID: artifact.CreatedByActorID, Body: "invalid anchor"}); err == nil {
		t.Fatal("accepted child address on non-version target")
	}
	if _, _, err := s.AddArtifactComment(ctx, "discourse", artifact.ArtifactID, ArtifactCommentInsert{TargetKind: "artifact_version", TargetID: otherVersion.ArtifactVersionID, ActorID: artifact.CreatedByActorID, Body: "cross artifact"}); err == nil {
		t.Fatal("accepted version target from another artifact")
	}
	foreignParent, _, err := s.AddArtifactComment(ctx, otherArtifact.ProjectIdent, otherArtifact.ArtifactID, ArtifactCommentInsert{
		TargetKind: "artifact_version", TargetID: otherVersion.ArtifactVersionID,
		ActorID: otherArtifact.CreatedByActorID, Body: "foreign parent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddArtifactComment(ctx, "discourse", artifact.ArtifactID, ArtifactCommentInsert{
		TargetKind: "artifact_version", TargetID: version.ArtifactVersionID, ParentCommentID: &foreignParent.CommentID,
		ActorID: artifact.CreatedByActorID, Body: "foreign parent must fail",
	}); err == nil {
		t.Fatal("accepted comment parent from another artifact")
	}
	if _, _, err := s.AddArtifactComment(ctx, "elsewhere", artifact.ArtifactID, ArtifactCommentInsert{TargetKind: "artifact", TargetID: artifact.ArtifactID, ActorID: artifact.CreatedByActorID, Body: "cross project"}); err == nil {
		t.Fatal("accepted artifact from another project")
	}
	reply, replayed, err := s.AddArtifactComment(ctx, "discourse", artifact.ArtifactID, ArtifactCommentInsert{TargetKind: "contribution", TargetID: "missing", ParentCommentID: &comment.CommentID, ActorID: artifact.CreatedByActorID, Body: "invalid target"})
	if err == nil || reply != nil || replayed {
		t.Fatalf("accepted unresolved contribution target: %+v %v", reply, err)
	}
	workflow, err := s.StartWorkflowRun(ctx, WorkflowRunInsert{ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_iteration", CoordinatorActorID: artifact.CreatedByActorID})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.ResolveArtifactComment(ctx, "discourse", artifact.ArtifactID, comment.CommentID, ArtifactCommentResolve{ActorID: artifact.CreatedByActorID, WorkflowRunID: &workflow.Record.WorkflowRunID, Note: artifactStringPtr("addressed")})
	if err != nil || resolved.State != "resolved" || resolved.ResolvedAt == nil || resolved.ResolutionNote == nil || *resolved.ResolutionNote != "addressed" || resolved.ResolvedByWorkflowRun == nil || *resolved.ResolvedByWorkflowRun != workflow.Record.WorkflowRunID {
		t.Fatalf("resolve comment: %+v %v", resolved, err)
	}
	resolvedAgain, err := s.ResolveArtifactComment(ctx, "discourse", artifact.ArtifactID, comment.CommentID, ArtifactCommentResolve{ActorID: "other-resolver"})
	if err != nil || resolvedAgain.ResolvedByActorID == nil || *resolvedAgain.ResolvedByActorID != artifact.CreatedByActorID {
		t.Fatalf("resolve replay changed attribution: %+v %v", resolvedAgain, err)
	}
	open, err := s.ReopenArtifactComment(ctx, "discourse", artifact.ArtifactID, comment.CommentID, artifact.CreatedByActorID, "reopen note", artifactStringPtr("reopen-once"))
	if err != nil || open.State != "open" || open.ResolvedAt != nil || open.ResolutionNote != nil {
		t.Fatalf("reopen comment: %+v %v", open, err)
	}
	items, err := s.ArtifactComments(ctx, "discourse", artifact.ArtifactID)
	var foundReply bool
	for _, item := range items {
		if item.ParentCommentID != nil && *item.ParentCommentID == comment.CommentID && item.Body == "reopen note" {
			foundReply = true
		}
	}
	if err != nil || len(items) != 2 || !foundReply {
		t.Fatalf("reopen note thread: %+v %v", items, err)
	}
	if _, err := s.ResolveArtifactComment(ctx, "discourse", artifact.ArtifactID, comment.CommentID, ArtifactCommentResolve{ActorID: artifact.CreatedByActorID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReopenArtifactComment(ctx, "discourse", artifact.ArtifactID, comment.CommentID, artifact.CreatedByActorID, "duplicate reopen note", artifactStringPtr("reopen-once")); err != nil {
		t.Fatal(err)
	}
	items, err = s.ArtifactComments(ctx, "discourse", artifact.ArtifactID)
	if err != nil || len(items) != 2 {
		t.Fatalf("reopen replay duplicated its note: %+v %v", items, err)
	}
	if hidden, err := s.ArtifactComment(ctx, "elsewhere", artifact.ArtifactID, comment.CommentID); err != nil || hidden != nil {
		t.Fatalf("cross-project comment lookup leaked: %+v %v", hidden, err)
	}
	acceptance, err := s.AcceptArtifactVersion(ctx, artifact.ProjectIdent, artifact.ArtifactID, version.ArtifactVersionID, artifact.CreatedByActorID, nil, artifactStringPtr("accept-once"))
	if err != nil || acceptance == nil || acceptance.ContributionKind != "state_transition" {
		t.Fatalf("accept reviewed version: %+v %v", acceptance, err)
	}
	accepted, err := s.Artifact(ctx, artifact.ProjectIdent, artifact.ArtifactID)
	if err != nil || accepted == nil || accepted.AcceptedVersionID == nil || *accepted.AcceptedVersionID != version.ArtifactVersionID {
		t.Fatalf("accepted review pointer: %+v %v", accepted, err)
	}
}

func TestArtifactContributionTargetsReadSetAndReplay(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artifact, version, _ := artifactChunkFixture(t, s, "contributions")
	_, foreignVersion, _ := artifactChunkFixture(t, s, "foreign-contributions")
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "user", DisplayName: "Contributor"})
	if err != nil {
		t.Fatal(err)
	}
	key := "review-once"
	readSet, _ := json.Marshal(map[string]any{
		"versions":       []string{version.ArtifactVersionID, foreignVersion.ArtifactVersionID},
		"manifest_items": []string{"manifest.items[deferred-T011-address]"},
	})
	contribution, replayed, err := s.AddArtifactContribution(ctx, "CONTRIBUTIONS", artifact.ArtifactID, ArtifactContributionInsert{
		TargetKind: "artifact_version", TargetID: version.ArtifactVersionID, ContributionKind: "review", Phase: artifactStringPtr("pass_1"),
		Role: "reviewer", ActorID: actor.ActorID, ReadSet: readSet, Body: "review outcome", IdempotencyKey: &key,
	})
	if err != nil || contribution == nil || replayed || contribution.TargetID != version.ArtifactVersionID || contribution.BodyFormat != "markdown" || string(contribution.ReadSet) != string(readSet) {
		t.Fatalf("add contribution: %+v replayed=%v err=%v", contribution, replayed, err)
	}
	again, replayed, err := s.AddArtifactContribution(ctx, "contributions", artifact.ArtifactID, ArtifactContributionInsert{
		TargetKind: "artifact", TargetID: artifact.ArtifactID, ContributionKind: "note", Role: "author", ActorID: actor.ActorID,
		Body: "changed replay", IdempotencyKey: &key,
	})
	if err != nil || !replayed || again.ContributionID != contribution.ContributionID || again.Body != contribution.Body {
		t.Fatalf("contribution replay: %+v replayed=%v err=%v", again, replayed, err)
	}
	if _, _, err := s.AddArtifactContribution(ctx, "contributions", artifact.ArtifactID, ArtifactContributionInsert{TargetKind: "artifact_version", TargetID: "missing", ContributionKind: "review", Role: "reviewer", ActorID: actor.ActorID, Body: "bad target"}); err == nil {
		t.Fatal("accepted missing target")
	}
	if _, _, err := s.AddArtifactContribution(ctx, "contributions", artifact.ArtifactID, ArtifactContributionInsert{
		TargetKind: "artifact_version", TargetID: foreignVersion.ArtifactVersionID, ContributionKind: "review",
		Role: "reviewer", ActorID: actor.ActorID, Body: "cross artifact",
	}); err == nil {
		t.Fatal("accepted contribution to another artifact's version")
	}
	badReadSet := json.RawMessage(`{"versions":["missing-version"]}`)
	if _, _, err := s.AddArtifactContribution(ctx, "contributions", artifact.ArtifactID, ArtifactContributionInsert{TargetKind: "artifact", TargetID: artifact.ArtifactID, ContributionKind: "note", Role: "author", ActorID: actor.ActorID, ReadSet: badReadSet, Body: "bad read set"}); err == nil {
		t.Fatal("accepted unresolved version read_set reference")
	}
	if _, _, err := s.AddArtifactContribution(ctx, "other-project", artifact.ArtifactID, ArtifactContributionInsert{TargetKind: "artifact", TargetID: artifact.ArtifactID, ContributionKind: "note", Role: "author", ActorID: actor.ActorID, Body: "cross project"}); err == nil {
		t.Fatal("accepted artifact from another project")
	}
	run, err := s.StartWorkflowRun(ctx, WorkflowRunInsert{ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_iteration", CoordinatorActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	runKey := "run-contribution"
	runInput := ArtifactContributionInsert{TargetKind: "artifact_version", TargetID: version.ArtifactVersionID, ContributionKind: "synthesis", Role: "analyst", ActorID: actor.ActorID, WorkflowRunID: &run.Record.WorkflowRunID, Body: "synthesis", IdempotencyKey: &runKey}
	runContribution, replayed, err := s.AddArtifactContribution(ctx, "contributions", artifact.ArtifactID, runInput)
	if err != nil || runContribution == nil || replayed {
		t.Fatalf("run contribution: %+v replayed=%v err=%v", runContribution, replayed, err)
	}
	runAgain, replayed, err := s.AddArtifactContribution(ctx, "contributions", artifact.ArtifactID, runInput)
	if err != nil || !replayed || runAgain.ContributionID != runContribution.ContributionID {
		t.Fatalf("run contribution replay: %+v replayed=%v err=%v", runAgain, replayed, err)
	}
	items, err := s.ArtifactContributions(ctx, "contributions", artifact.ArtifactID)
	if err != nil || len(items) != 2 {
		t.Fatalf("list contributions: %+v %v", items, err)
	}
	if hidden, err := s.ArtifactContribution(ctx, "other-project", artifact.ArtifactID, contribution.ContributionID); err != nil || hidden != nil {
		t.Fatalf("cross-project contribution lookup leaked: %+v %v", hidden, err)
	}
}

func TestArtifactDiscourseStartupMigrationPersistsAcrossOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "discourse.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, fixtureProject("discourse-reopen")); err != nil {
		t.Fatal(err)
	}
	artifact, version, _ := artifactChunkFixture(t, s, "discourse-reopen")
	comment, replayed, err := s.AddArtifactComment(ctx, "discourse-reopen", artifact.ArtifactID, ArtifactCommentInsert{
		TargetKind: "artifact_version", TargetID: version.ArtifactVersionID, ActorID: artifact.CreatedByActorID, Body: "preserved comment",
	})
	if err != nil || comment == nil || replayed {
		t.Fatalf("create comment: %+v replayed=%v err=%v", comment, replayed, err)
	}
	contribution, replayed, err := s.AddArtifactContribution(ctx, "discourse-reopen", artifact.ArtifactID, ArtifactContributionInsert{
		TargetKind: "artifact_version", TargetID: version.ArtifactVersionID, ContributionKind: "note", Role: "author", ActorID: artifact.CreatedByActorID, Body: "preserved contribution",
	})
	if err != nil || contribution == nil || replayed {
		t.Fatalf("create contribution: %+v replayed=%v err=%v", contribution, replayed, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	gotComment, err := reopened.ArtifactComment(ctx, "discourse-reopen", artifact.ArtifactID, comment.CommentID)
	if err != nil || gotComment == nil || gotComment.Body != "preserved comment" {
		t.Fatalf("comment after reopen: %+v %v", gotComment, err)
	}
	gotContribution, err := reopened.ArtifactContribution(ctx, "discourse-reopen", artifact.ArtifactID, contribution.ContributionID)
	if err != nil || gotContribution == nil || gotContribution.Body != "preserved contribution" {
		t.Fatalf("contribution after reopen: %+v %v", gotContribution, err)
	}
	var migration int
	if err := reopened.db.QueryRowContext(ctx, "SELECT version FROM amux_go_migrations WHERE version=?", artifactDiscourseMigration).Scan(&migration); err != nil || migration != artifactDiscourseMigration {
		t.Fatalf("discourse migration marker=%d err=%v", migration, err)
	}
}

func TestArtifactDiscourseMigrationPreservesExistingRustRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rust-discourse.sqlite")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	artifact, version, _ := artifactChunkFixture(t, s, "rust-discourse")
	contributionReadSet := `{"versions":["` + version.ArtifactVersionID + `"],"manifest_items":["manifest.items[deferred-item]"]}`
	_, err = s.db.ExecContext(ctx, `INSERT INTO artifact_contributions(
contribution_id,artifact_id,target_kind,target_id,contribution_kind,phase,role,actor_id,workflow_run_id,read_set,body_format,body,idempotency_key,created_at)
VALUES('rust-contribution',?,'artifact_version',?,'review','pass_1','reviewer',?,NULL,?,'markdown','Rust review body','rust-operation',123)`,
		artifact.ArtifactID, version.ArtifactVersionID, artifact.CreatedByActorID, contributionReadSet)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO artifact_comments(
comment_id,artifact_id,target_kind,target_id,child_address,parent_comment_id,actor_id,body,state,resolved_by_actor_id,resolved_by_workflow_run_id,resolved_at,resolution_note,idempotency_key,created_at,updated_at)
VALUES('rust-comment',?,'artifact_version',?,'manifest.items[deferred-item]',NULL,?,'Rust anchored comment','resolved',?,NULL,456,'resolved in Rust','rust-comment-operation',124,456)`,
		artifact.ArtifactID, version.ArtifactVersionID, artifact.CreatedByActorID, artifact.CreatedByActorID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO artifact_comments(
comment_id,artifact_id,target_kind,target_id,child_address,parent_comment_id,actor_id,body,state,created_at,updated_at)
VALUES('rust-reply',?,'contribution','rust-contribution',NULL,'rust-comment',?,'Rust reply on contribution','open',125,125)`,
		artifact.ArtifactID, artifact.CreatedByActorID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM amux_go_migrations WHERE version=?`, artifactDiscourseMigration); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	comments, err := reopened.ArtifactComments(ctx, artifact.ProjectIdent, artifact.ArtifactID)
	if err != nil || len(comments) != 2 {
		t.Fatalf("Rust comments after migration: %+v err=%v", comments, err)
	}
	if comments[0].CommentID != "rust-comment" || comments[0].TargetKind != "artifact_version" || comments[0].TargetID != version.ArtifactVersionID || comments[0].ChildAddress == nil || *comments[0].ChildAddress != "manifest.items[deferred-item]" || comments[0].State != "resolved" || comments[0].ResolvedAt == nil || *comments[0].ResolvedAt != 456 {
		t.Fatalf("Rust version anchor/lifecycle was not preserved: %+v", comments[0])
	}
	if comments[1].CommentID != "rust-reply" || comments[1].TargetKind != "contribution" || comments[1].TargetID != "rust-contribution" || comments[1].ParentCommentID == nil || *comments[1].ParentCommentID != "rust-comment" {
		t.Fatalf("Rust contribution/parent relationship was not preserved: %+v", comments[1])
	}
	contributions, err := reopened.ArtifactContributions(ctx, artifact.ProjectIdent, artifact.ArtifactID)
	if err != nil || len(contributions) != 1 {
		t.Fatalf("Rust contributions after migration: %+v err=%v", contributions, err)
	}
	contribution := contributions[0]
	if contribution.ContributionID != "rust-contribution" || contribution.TargetKind != "artifact_version" || contribution.TargetID != version.ArtifactVersionID || contribution.Role != "reviewer" || contribution.Body != "Rust review body" || string(contribution.ReadSet) != contributionReadSet {
		t.Fatalf("Rust contribution target/read-set was not preserved: %+v", contribution)
	}
	var recorded int
	if err := reopened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM amux_go_migrations WHERE version=?`, artifactDiscourseMigration).Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("discourse migration marker count=%d err=%v", recorded, err)
	}
}
