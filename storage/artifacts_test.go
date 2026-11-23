package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArtifactBaseMigrationPreservesRustRowsAndVersionAttribution(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rust.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE projects(ident TEXT PRIMARY KEY,channel_name TEXT NOT NULL,room_id TEXT NOT NULL,last_msg_id TEXT,created_at INTEGER NOT NULL);
INSERT INTO projects VALUES('legacy','fixture','room',NULL,10);
CREATE TABLE artifact_actors(actor_id TEXT PRIMARY KEY,actor_type TEXT NOT NULL,agent_system TEXT,agent_system_label TEXT,agent_id TEXT,host TEXT,display_name TEXT NOT NULL,runtime_metadata TEXT,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE artifacts(artifact_id TEXT PRIMARY KEY,project_ident TEXT NOT NULL REFERENCES projects(ident),kind TEXT NOT NULL,subkind TEXT,title TEXT NOT NULL,labels TEXT,lifecycle_state TEXT NOT NULL DEFAULT 'draft',review_state TEXT NOT NULL DEFAULT 'none',implementation_state TEXT NOT NULL DEFAULT 'not_applicable',current_version_id TEXT REFERENCES artifact_versions(artifact_version_id) DEFERRABLE INITIALLY DEFERRED,accepted_version_id TEXT REFERENCES artifact_versions(artifact_version_id) DEFERRABLE INITIALLY DEFERRED,created_by_actor_id TEXT NOT NULL REFERENCES artifact_actors(actor_id),created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE workflow_runs(workflow_run_id TEXT PRIMARY KEY);
CREATE TABLE artifact_versions(artifact_version_id TEXT PRIMARY KEY,artifact_id TEXT NOT NULL REFERENCES artifacts(artifact_id) ON DELETE CASCADE,version_label TEXT,parent_version_id TEXT REFERENCES artifact_versions(artifact_version_id),body_format TEXT NOT NULL,body TEXT,structured_payload TEXT,source_format TEXT,created_by_actor_id TEXT NOT NULL REFERENCES artifact_actors(actor_id),created_via_workflow_run_id TEXT REFERENCES workflow_runs(workflow_run_id) DEFERRABLE INITIALLY DEFERRED,version_state TEXT NOT NULL DEFAULT 'draft',idempotency_key TEXT,body_purged_at INTEGER,created_at INTEGER NOT NULL);
INSERT INTO artifact_actors VALUES('actor-rust','agent','codex',NULL,'worker-7','host-a','Legacy worker',NULL,20,21);
INSERT INTO workflow_runs VALUES('run-rust');
INSERT INTO artifacts VALUES('artifact-rust','legacy','spec','api','Legacy spec','["compat"]','active','none','in_progress','version-rust',NULL,'actor-rust',30,31);
INSERT INTO artifact_versions VALUES('version-rust','artifact-rust','v1',NULL,'markdown','legacy body','{"retained":true}','markdown','actor-rust','run-rust','accepted','key-rust',NULL,30);
PRAGMA user_version=12;`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	artifact, err := s.Artifact(ctx, "legacy", "artifact-rust")
	if err != nil || artifact == nil || artifact.Title != "Legacy spec" || artifact.CreatedByActorID != "actor-rust" || artifact.CurrentVersionID == nil || *artifact.CurrentVersionID != "version-rust" || len(artifact.Labels) != 1 || artifact.Labels[0] != "compat" {
		t.Fatalf("legacy Rust artifact changed: %+v %v", artifact, err)
	}
	version, err := s.ArtifactVersion(ctx, "legacy", "artifact-rust", "version-rust")
	if err != nil || version == nil || version.CreatedByActorID != "actor-rust" || version.CreatedViaWorkflowRunID == nil || *version.CreatedViaWorkflowRunID != "run-rust" || version.Body == nil || *version.Body != "legacy body" || string(version.StructuredPayload) != `{"retained":true}` {
		t.Fatalf("legacy Rust version attribution changed: %+v %v", version, err)
	}
	legacyRun, err := s.WorkflowRun(ctx, "run-rust")
	if err != nil || legacyRun == nil || legacyRun.WorkflowRunID != "run-rust" || legacyRun.State != "started" {
		t.Fatalf("minimal legacy workflow_runs row was not retained/readable: %+v %v", legacyRun, err)
	}
	var userVersion int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil || userVersion != 12 {
		t.Fatalf("migration changed Rust user_version: %d %v", userVersion, err)
	}
	var migration int
	if err := s.db.QueryRowContext(ctx, "SELECT version FROM amux_go_migrations WHERE version=?", artifactBaseMigration).Scan(&migration); err != nil || migration != artifactBaseMigration {
		t.Fatalf("artifact migration marker: %d %v", migration, err)
	}
	task, err := s.InsertTask(ctx, TaskInsert{Project: "legacy", Title: "legacy task", Reporter: "tester"}, nil)
	if err != nil || task == nil || task.Status != "todo" {
		t.Fatalf("legacy task table was unusable: %+v %v", task, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	preserved, err := reopened.Task(ctx, "legacy", task.ID)
	if err != nil || preserved == nil || preserved.Task.Title != "legacy task" {
		t.Fatalf("reapplying the artifact schema changed the task: %+v %v", preserved, err)
	}
	preservedVersion, err := reopened.ArtifactVersion(ctx, "legacy", "artifact-rust", "version-rust")
	if err != nil || preservedVersion == nil || preservedVersion.CreatedByActorID != "actor-rust" {
		t.Fatalf("reapplying the schema changed the artifact version: %+v %v", preservedVersion, err)
	}

	t.Run("reject invalid review state on retained schema", func(t *testing.T) {
		review, err := reopened.CreateArtifact(ctx, ArtifactInsert{
			ProjectIdent: "legacy", Kind: "design_review", Title: "Imported profile review", CreatedByActorID: "actor-rust",
		})
		if err != nil {
			t.Fatal(err)
		}
		invalid := "unknown-review-state"
		if _, err := reopened.UpdateDesignReviewState(ctx, "legacy", review.ArtifactID, nil, &invalid, "actor-rust", "fixture", nil, "invalid-transition"); err == nil || !strings.Contains(err.Error(), "invalid artifact review state") {
			t.Fatalf("invalid state on retained schema: %v", err)
		}
		contributions, err := reopened.ArtifactContributions(ctx, "legacy", review.ArtifactID)
		if err != nil || len(contributions) != 0 {
			t.Fatalf("invalid state wrote an audit contribution: %+v err=%v", contributions, err)
		}
		stored, err := reopened.Artifact(ctx, "legacy", review.ArtifactID)
		if err != nil || stored == nil || stored.ReviewState != review.ReviewState {
			t.Fatalf("invalid state changed the retained artifact: %+v err=%v", stored, err)
		}
	})

}

func TestArtifactAndVersionsUseProjectScopeAndImmutableSnapshots(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.InsertProject(ctx, fixtureProject("ArtifactDemo")); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, fixtureProject("other")); err != nil {
		t.Fatal(err)
	}
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", AgentSystem: artifactStringPtr("codex"), AgentID: artifactStringPtr("worker"), Host: artifactStringPtr("host"), DisplayName: "Worker"})
	if err != nil {
		t.Fatal(err)
	}
	actorAgain, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", AgentSystem: artifactStringPtr("codex"), AgentID: artifactStringPtr("worker"), Host: artifactStringPtr("host"), DisplayName: "Worker renamed"})
	if err != nil || actorAgain.ActorID != actor.ActorID || actorAgain.DisplayName != "Worker renamed" {
		t.Fatalf("actor identity did not resolve idempotently: %+v %v", actorAgain, err)
	}
	differentActor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", AgentSystem: artifactStringPtr("codex"), AgentID: artifactStringPtr("different-worker"), Host: artifactStringPtr("host"), DisplayName: "Worker"})
	if err != nil || differentActor.ActorID == actor.ActorID {
		t.Fatalf("distinct agent did not resolve to a distinct actor: %+v %v", differentActor, err)
	}
	artifact, err := s.CreateArtifact(ctx, ArtifactInsert{ProjectIdent: "ARTIFACTDEMO", Kind: "spec", Title: "API contract", Labels: []string{"api", "v1"}, CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ProjectIdent != "artifactdemo" || artifact.ImplementationState != "not_started" || artifact.LifecycleState != "draft" || artifact.ReviewState != "none" || len(artifact.Labels) != 2 {
		t.Fatalf("artifact defaults mismatch: %+v", artifact)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE artifacts SET lifecycle_state='active', review_state='collecting_reviews', implementation_state='in_progress' WHERE artifact_id=?`, artifact.ArtifactID); err != nil {
		t.Fatal(err)
	}
	states, err := s.Artifact(ctx, "artifactdemo", artifact.ArtifactID)
	if err != nil || states == nil || states.LifecycleState != "active" || states.ReviewState != "collecting_reviews" || states.ImplementationState != "in_progress" {
		t.Fatalf("independent artifact states: %+v %v", states, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE artifacts SET lifecycle_state='banana' WHERE artifact_id=?`, artifact.ArtifactID); err == nil || !strings.Contains(strings.ToLower(err.Error()), "check constraint") {
		t.Fatalf("invalid artifact lifecycle state: %v", err)
	}
	body := "Initial body"
	payload := json.RawMessage(`{"schema":1}`)
	version, err := s.CreateArtifactVersion(ctx, "artifactdemo", artifact.ArtifactID, ArtifactVersionInsert{Body: &body, StructuredPayload: payload, CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	if version.Record.BodyFormat != "markdown" || version.Record.VersionState != "draft" || string(version.Record.StructuredPayload) != string(payload) || version.Replayed {
		t.Fatalf("version defaults mismatch: %+v", version)
	}
	updated, err := s.Artifact(ctx, "artifactdemo", artifact.ArtifactID)
	if err != nil || updated.CurrentVersionID == nil || *updated.CurrentVersionID != version.Record.ArtifactVersionID {
		t.Fatalf("artifact current version pointer: %+v %v", updated, err)
	}
	detail, err := s.ArtifactDetail(ctx, "artifactdemo", artifact.ArtifactID)
	if err != nil || detail == nil || detail.CurrentVersion == nil || detail.CurrentVersion.ArtifactVersionID != version.Record.ArtifactVersionID || detail.AcceptedVersion != nil {
		t.Fatalf("artifact detail did not resolve current/accepted versions: %+v %v", detail, err)
	}
	versions, err := s.ArtifactVersions(ctx, "artifactdemo", artifact.ArtifactID)
	if err != nil || len(versions) != 1 || versions[0].ArtifactVersionID != version.Record.ArtifactVersionID {
		t.Fatalf("project-scoped version listing: %+v %v", versions, err)
	}
	if cross, err := s.Artifact(ctx, "other", artifact.ArtifactID); err != nil || cross != nil {
		t.Fatalf("cross-project artifact visible: %+v %v", cross, err)
	}
	if cross, err := s.ArtifactVersion(ctx, "other", artifact.ArtifactID, version.Record.ArtifactVersionID); err != nil || cross != nil {
		t.Fatalf("cross-project version visible: %+v %v", cross, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE artifact_versions SET body='rewritten' WHERE artifact_version_id=?", version.Record.ArtifactVersionID); err == nil {
		t.Fatal("immutable version body update succeeded")
	}
	var userVersion int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil || userVersion != 0 {
		t.Fatalf("Go artifact migration changed SQLite user_version: %d %v", userVersion, err)
	}
}

func TestArtifactVersionRejectsForeignParentAndAllowsSourcePurge(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.InsertProject(ctx, fixtureProject("artifact-test")); err != nil {
		t.Fatal(err)
	}
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "user", DisplayName: "Operator"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.CreateArtifact(ctx, ArtifactInsert{ProjectIdent: "artifact-test", Kind: "design_review", Title: "First", CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateArtifact(ctx, ArtifactInsert{ProjectIdent: "artifact-test", Kind: "spec", Title: "Second", CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	body := "immutable"
	version, err := s.CreateArtifactVersion(ctx, "artifact-test", first.ArtifactID, ArtifactVersionInsert{Body: &body, CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateArtifactVersion(ctx, "artifact-test", second.ArtifactID, ArtifactVersionInsert{ParentVersionID: &version.Record.ArtifactVersionID, CreatedByActorID: actor.ActorID}); err == nil {
		t.Fatal("foreign artifact parent accepted")
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE artifact_versions SET body='changed' WHERE artifact_version_id=?", version.Record.ArtifactVersionID); err == nil {
		t.Fatal("immutable body mutation succeeded")
	}
	original, err := s.ArtifactVersion(ctx, "artifact-test", first.ArtifactID, version.Record.ArtifactVersionID)
	if err != nil || original == nil || original.Body == nil || *original.Body != body {
		t.Fatalf("failed write modified immutable body: %v", err)
	}
	for _, change := range []string{"body_format='openapi'", "parent_version_id=artifact_version_id"} {
		if _, err := s.db.ExecContext(ctx, "UPDATE artifact_versions SET "+change+" WHERE artifact_version_id=?", version.Record.ArtifactVersionID); err == nil {
			t.Fatalf("immutable metadata mutation succeeded: %s", change)
		}
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE artifact_versions SET version_state='under_review' WHERE artifact_version_id=?", version.Record.ArtifactVersionID); err != nil {
		t.Fatalf("mutable version state could not transition: %v", err)
	}
	purgedAt := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx, "UPDATE artifact_versions SET body=NULL,body_purged_at=? WHERE artifact_version_id=?", purgedAt, version.Record.ArtifactVersionID); err != nil {
		t.Fatalf("Rust retention purge rejected: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE artifact_versions SET body='restored' WHERE artifact_version_id=?", version.Record.ArtifactVersionID); err == nil {
		t.Fatal("purged body was repopulated")
	}
}

func TestCreateArtifactVersionReplayRepairsNullCurrentPointer(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.InsertProject(ctx, fixtureProject("version-repair")); err != nil {
		t.Fatal(err)
	}
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", DisplayName: "Agent"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CreateArtifact(ctx, ArtifactInsert{ProjectIdent: "version-repair", Kind: "spec", Title: "Repair", CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.StartWorkflowRun(ctx, WorkflowRunInsert{ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_iteration", CoordinatorActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	runID, key, body := run.Record.WorkflowRunID, "version-key", "version body"
	input := ArtifactVersionInsert{Body: &body, CreatedByActorID: actor.ActorID, CreatedViaWorkflowRunID: &runID, IdempotencyKey: &key}
	first, err := s.CreateArtifactVersion(ctx, "version-repair", artifact.ArtifactID, input)
	if err != nil || first == nil || first.Replayed {
		t.Fatalf("initial version write: %+v %v", first, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE artifacts SET current_version_id=NULL WHERE artifact_id=?", artifact.ArtifactID); err != nil {
		t.Fatal(err)
	}
	changedBody := "replay body must not replace the original"
	input.Body = &changedBody
	replay, err := s.CreateArtifactVersion(ctx, "version-repair", artifact.ArtifactID, input)
	if err != nil || replay == nil || !replay.Replayed || replay.Record.ArtifactVersionID != first.Record.ArtifactVersionID || replay.Record.Body == nil || *replay.Record.Body != body {
		t.Fatalf("version replay did not return original row: %+v %v", replay, err)
	}
	current, err := s.Artifact(ctx, "version-repair", artifact.ArtifactID)
	if err != nil || current.CurrentVersionID == nil || *current.CurrentVersionID != first.Record.ArtifactVersionID {
		t.Fatalf("replay did not repair null pointer: %+v %v", current, err)
	}
}

func TestCreateArtifactVersionStaleReplayDoesNotRegressCurrentPointer(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.InsertProject(ctx, fixtureProject("version-stale")); err != nil {
		t.Fatal(err)
	}
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", DisplayName: "Agent"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CreateArtifact(ctx, ArtifactInsert{ProjectIdent: "version-stale", Kind: "spec", Title: "Stale replay", CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.StartWorkflowRun(ctx, WorkflowRunInsert{ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_iteration", CoordinatorActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	runID, firstKey, secondKey := run.Record.WorkflowRunID, "first-version", "second-version"
	firstBody, secondBody := "older", "newer"
	first, err := s.CreateArtifactVersion(ctx, "version-stale", artifact.ArtifactID, ArtifactVersionInsert{Body: &firstBody, CreatedByActorID: actor.ActorID, CreatedViaWorkflowRunID: &runID, IdempotencyKey: &firstKey})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateArtifactVersion(ctx, "version-stale", artifact.ArtifactID, ArtifactVersionInsert{ParentVersionID: &first.Record.ArtifactVersionID, Body: &secondBody, CreatedByActorID: actor.ActorID, CreatedViaWorkflowRunID: &runID, IdempotencyKey: &secondKey})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.CreateArtifactVersion(ctx, "version-stale", artifact.ArtifactID, ArtifactVersionInsert{Body: &firstBody, CreatedByActorID: actor.ActorID, CreatedViaWorkflowRunID: &runID, IdempotencyKey: &firstKey})
	if err != nil || replay == nil || !replay.Replayed || replay.Record.ArtifactVersionID != first.Record.ArtifactVersionID {
		t.Fatalf("stale version replay: %+v %v", replay, err)
	}
	current, err := s.Artifact(ctx, "version-stale", artifact.ArtifactID)
	if err != nil || current.CurrentVersionID == nil || *current.CurrentVersionID != second.Record.ArtifactVersionID {
		t.Fatalf("stale replay regressed current pointer: %+v %v", current, err)
	}
}

func TestArtifactVersionReplayKeyUsesArtifactRunAndKeyTuple(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.InsertProject(ctx, fixtureProject("version-keys")); err != nil {
		t.Fatal(err)
	}
	actor, err := s.ResolveArtifactActor(ctx, ArtifactActorIdentity{ActorType: "agent", DisplayName: "Agent"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CreateArtifact(ctx, ArtifactInsert{ProjectIdent: "version-keys", Kind: "spec", Title: "Keys", CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	body := "body"
	key := "same-key"
	runOne, err := s.StartWorkflowRun(ctx, WorkflowRunInsert{ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_iteration", CoordinatorActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	runTwo, err := s.StartWorkflowRun(ctx, WorkflowRunInsert{ArtifactID: artifact.ArtifactID, WorkflowKind: "spec_acceptance", CoordinatorActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	runOneID, runTwoID := runOne.Record.WorkflowRunID, runTwo.Record.WorkflowRunID
	input := ArtifactVersionInsert{Body: &body, CreatedByActorID: actor.ActorID, CreatedViaWorkflowRunID: &runOneID, IdempotencyKey: &key}
	first, err := s.CreateArtifactVersion(ctx, "version-keys", artifact.ArtifactID, input)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.CreateArtifactVersion(ctx, "version-keys", artifact.ArtifactID, input)
	if err != nil || replay == nil || !replay.Replayed || replay.Record.ArtifactVersionID != first.Record.ArtifactVersionID {
		t.Fatalf("same artifact/run/key was not replayed: %+v %v", replay, err)
	}
	input.CreatedViaWorkflowRunID = &runTwoID
	otherRun, err := s.CreateArtifactVersion(ctx, "version-keys", artifact.ArtifactID, input)
	if err != nil || otherRun == nil || otherRun.Replayed || otherRun.Record.ArtifactVersionID == first.Record.ArtifactVersionID {
		t.Fatalf("different workflow run collided: %+v %v", otherRun, err)
	}
	otherArtifact, err := s.CreateArtifact(ctx, ArtifactInsert{ProjectIdent: "version-keys", Kind: "spec", Title: "Other artifact", CreatedByActorID: actor.ActorID})
	if err != nil {
		t.Fatal(err)
	}
	input.CreatedViaWorkflowRunID = &runOneID
	otherArtifactVersion, err := s.CreateArtifactVersion(ctx, "version-keys", otherArtifact.ArtifactID, input)
	if err != nil || otherArtifactVersion == nil || otherArtifactVersion.Replayed || otherArtifactVersion.Record.ArtifactVersionID == first.Record.ArtifactVersionID {
		t.Fatalf("different artifact collided on replay key: %+v %v", otherArtifactVersion, err)
	}
	input.CreatedViaWorkflowRunID = nil
	noRunFirst, err := s.CreateArtifactVersion(ctx, "version-keys", artifact.ArtifactID, input)
	if err != nil {
		t.Fatal(err)
	}
	noRunReplay, err := s.CreateArtifactVersion(ctx, "version-keys", artifact.ArtifactID, input)
	if err != nil || noRunReplay == nil || noRunReplay.Replayed || noRunReplay.Record.ArtifactVersionID == noRunFirst.Record.ArtifactVersionID {
		t.Fatalf("key without workflow run was unexpectedly replayed: %+v %v", noRunReplay, err)
	}
	input.CreatedViaWorkflowRunID = &runOneID
	input.IdempotencyKey = nil
	noKeyFirst, err := s.CreateArtifactVersion(ctx, "version-keys", artifact.ArtifactID, input)
	if err != nil {
		t.Fatal(err)
	}
	noKeyReplay, err := s.CreateArtifactVersion(ctx, "version-keys", artifact.ArtifactID, input)
	if err != nil || noKeyReplay == nil || noKeyReplay.Replayed || noKeyReplay.Record.ArtifactVersionID == noKeyFirst.Record.ArtifactVersionID {
		t.Fatalf("workflow run without key was unexpectedly replayed: %+v %v", noKeyReplay, err)
	}
}

func artifactStringPtr(value string) *string { return &value }

func TestArtifactVersionPointersValidateOwnershipAndDiverge(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	artifact, first, second := artifactChunkFixture(t, s, "artifact-pointers")
	_, foreign, _ := artifactChunkFixture(t, s, "foreign-pointers")
	project, id := artifact.ProjectIdent, artifact.ArtifactID
	if err := s.SetArtifactVersionPointers(ctx, project, id, &first.ArtifactVersionID, &first.ArtifactVersionID); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"missing-version", foreign.ArtifactVersionID} {
		if err := s.SetArtifactVersionPointers(ctx, project, id, &second.ArtifactVersionID, &bad); err == nil {
			t.Fatalf("invalid accepted version %q was allowed", bad)
		}
		detail, err := s.ArtifactDetail(ctx, project, id)
		if err != nil || detail == nil || detail.CurrentVersion == nil || detail.AcceptedVersion == nil ||
			detail.CurrentVersion.ArtifactVersionID != first.ArtifactVersionID || detail.AcceptedVersion.ArtifactVersionID != first.ArtifactVersionID {
			t.Fatalf("invalid pointer changed durable state: %+v %v", detail, err)
		}
	}
	if err := s.SetArtifactVersionPointers(ctx, project, id, &second.ArtifactVersionID, nil); err != nil {
		t.Fatal(err)
	}
	detail, err := s.ArtifactDetail(ctx, project, id)
	if err != nil || detail == nil || detail.CurrentVersion == nil || detail.AcceptedVersion == nil ||
		detail.CurrentVersion.ArtifactVersionID != second.ArtifactVersionID || detail.AcceptedVersion.ArtifactVersionID != first.ArtifactVersionID {
		t.Fatalf("current and accepted pointers did not diverge: %+v %v", detail, err)
	}
}
