package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTaskDelegationContractUpdateMirrorsMetadataAndNudgesTarget(t *testing.T) {
	s, ctx, actor, delegated := delegationUpdatesFixture(t)
	var err error
	title := "Revised plan (DELEGATED)"
	description := "Updated description"
	details := "Legacy details"
	specification := "Preferred specification"
	result, err := s.UpdateTaskDelegationContract(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID, TaskDelegationUpdate{
		Title: &title, Description: &description, Details: &details, Specification: &specification, Labels: []string{},
	}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceTask.Title != title || result.TargetTask.Title != "Revised plan" {
		t.Fatalf("unexpected mirrored titles source=%q target=%q", result.SourceTask.Title, result.TargetTask.Title)
	}
	if result.SourceTask.Description == nil || *result.SourceTask.Description != description || result.TargetTask.Description == nil || *result.TargetTask.Description != description {
		t.Fatalf("description not mirrored: source=%+v target=%+v", result.SourceTask.Description, result.TargetTask.Description)
	}
	if result.SourceTask.Details == nil || *result.SourceTask.Details != specification || result.TargetTask.Details == nil || *result.TargetTask.Details != specification {
		t.Fatalf("specification did not take precedence: source=%+v target=%+v", result.SourceTask.Details, result.TargetTask.Details)
	}
	if len(result.SourceTask.Labels) != 0 || len(result.TargetTask.Labels) != 0 || result.MessageID <= 0 {
		t.Fatalf("labels or nudge receipt incorrect: source=%v target=%v message=%d", result.SourceTask.Labels, result.TargetTask.Labels, result.MessageID)
	}

	var sourceComments, targetComments int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_comments WHERE task_id=? AND content=?", result.SourceTask.ID, "Delegated task contract was updated by the source project.").Scan(&sourceComments); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_comments WHERE task_id=? AND content=?", result.TargetTask.ID, "Delegated task contract was updated by the source project.").Scan(&targetComments); err != nil {
		t.Fatal(err)
	}
	if sourceComments != 1 || targetComments != 1 {
		t.Fatalf("contract notes missing: source=%d target=%d", sourceComments, targetComments)
	}
	var subject string
	if err := s.db.QueryRowContext(ctx, "SELECT subject FROM messages WHERE id=?", result.MessageID).Scan(&subject); err != nil || subject != "Delegated task updated" {
		t.Fatalf("wrong target nudge subject %q, err=%v", subject, err)
	}

	// Null text fields decode as absent in the source DTO, while [] explicitly
	// clears labels and nil labels serialize as JSON null.
	wire, err := json.Marshal(TaskDelegationUpdate{Labels: []string{}})
	if err != nil || !strings.Contains(string(wire), `"labels":[]`) {
		t.Fatalf("empty labels did not survive JSON encoding: %s err=%v", wire, err)
	}
	wire, err = json.Marshal(TaskDelegationUpdate{})
	if err != nil || !strings.Contains(string(wire), `"labels":null`) {
		t.Fatalf("nil labels must encode as source no-op null: %s err=%v", wire, err)
	}
	var nullUpdate TaskDelegationUpdate
	if err := json.Unmarshal([]byte(`{"description":null,"details":null,"specification":null,"labels":null}`), &nullUpdate); err != nil {
		t.Fatal(err)
	}
	if nullUpdate.Description != nil || nullUpdate.Details != nil || nullUpdate.Specification != nil || nullUpdate.Labels != nil {
		t.Fatalf("source null values must decode like absent fields: %+v", nullUpdate)
	}
	if _, err := s.UpdateTaskDelegationContract(ctx, result.SourceTask.ProjectIdent, result.SourceTask.ID, nullUpdate, actor); err != nil {
		t.Fatal(err)
	}
	unchanged, err := s.Task(ctx, result.TargetTask.ProjectIdent, result.TargetTask.ID)
	if err != nil || unchanged.Task.Description == nil || *unchanged.Task.Description != description || unchanged.Task.Details == nil || *unchanged.Task.Details != specification {
		t.Fatalf("absent/null-equivalent metadata unexpectedly cleared fields: %+v err=%v", unchanged, err)
	}
}

func TestTaskDelegationCommentMirroringIsAsymmetricAndNonrecursive(t *testing.T) {
	s, ctx, actor, delegated := delegationUpdatesFixture(t)
	var err error
	if _, err := s.AddTaskComment(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID, "source-user", "user", "please clarify", nil, nil); err != nil {
		t.Fatal(err)
	}
	targetComments, err := readTaskComments(ctx, s.db, delegated.TargetTask.ID, nil)
	if err != nil || !delegationUpdatesHasSystemComment(targetComments, "Sender project `tasks`, ticket `"+delegated.SourceTask.ID+"`:\n\nsource-user: please clarify") {
		t.Fatalf("source clarification was not mirrored as a target system comment: %+v err=%v", targetComments, err)
	}

	if _, err := s.AddTaskComment(ctx, delegated.TargetTask.ProjectIdent, delegated.TargetTask.ID, "target-agent", "agent", "progress update", nil, actor); err != nil {
		t.Fatal(err)
	}
	var sourceMirrorCount int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_comments WHERE task_id=? AND content LIKE '%New comment on delegated task%'", delegated.SourceTask.ID).Scan(&sourceMirrorCount); err != nil || sourceMirrorCount != 0 {
		t.Fatalf("target comment was duplicated onto source task: count=%d err=%v", sourceMirrorCount, err)
	}
	var nudgeCount int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE project_ident=? AND subject='Delegated task comment added' AND content LIKE '%target-agent: progress update%'", delegated.SourceTask.ProjectIdent).Scan(&nudgeCount); err != nil || nudgeCount != 1 {
		t.Fatalf("target comment did not nudge source project: count=%d err=%v", nudgeCount, err)
	}
	var before int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_comments WHERE task_id=?", delegated.TargetTask.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskComment(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID, "agent-gateway", "system", "system note", nil, nil); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_comments WHERE task_id=?", delegated.TargetTask.ID).Scan(&after); err != nil || after != before {
		t.Fatalf("system comment recursed to target: before=%d after=%d err=%v", before, after, err)
	}
}

func TestDelegationCommentCapsOnlyDerivedClarificationAtUTF8Boundary(t *testing.T) {
	s, ctx, _, delegated := delegationUpdatesFixture(t)
	const maxBytes = 65536
	content := strings.Repeat("x", maxBytes-4) + "💡"
	if len(content) != maxBytes {
		t.Fatalf("fixture content bytes=%d, want %d", len(content), maxBytes)
	}
	if _, err := s.AddTaskComment(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID, "source-user", "user", content, nil, nil); err != nil {
		t.Fatal(err)
	}
	sourceComments, err := readTaskComments(ctx, s.db, delegated.SourceTask.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	foundOriginal := false
	for _, comment := range sourceComments {
		if comment.Author == "source-user" {
			foundOriginal = comment.Content == content
		}
	}
	if !foundOriginal {
		t.Fatal("the original maximum-size comment was altered")
	}
	targetComments, err := readTaskComments(ctx, s.db, delegated.TargetTask.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	foundClarification := false
	for _, comment := range targetComments {
		if comment.AuthorType != "system" || !strings.HasPrefix(comment.Content, "Sender project ") {
			continue
		}
		foundClarification = true
		if len(comment.Content) > maxBytes || !utf8.ValidString(comment.Content) || !strings.HasSuffix(comment.Content, "\n…[truncated]") {
			t.Fatalf("derived clarification cap invalid: bytes=%d utf8=%t suffix=%t", len(comment.Content), utf8.ValidString(comment.Content), strings.HasSuffix(comment.Content, "\n…[truncated]"))
		}
	}
	if !foundClarification {
		t.Fatal("source comment did not create a target clarification")
	}

	if _, err := s.AddTaskComment(ctx, delegated.TargetTask.ProjectIdent, delegated.TargetTask.ID, "target-agent", "agent", content, nil, nil); err != nil {
		t.Fatal(err)
	}
	var nudge string
	if err := s.db.QueryRowContext(ctx, "SELECT content FROM messages WHERE project_ident=? AND subject='Delegated task comment added' ORDER BY id DESC LIMIT 1", delegated.SourceTask.ProjectIdent).Scan(&nudge); err != nil {
		t.Fatal(err)
	}
	if len(nudge) <= maxBytes || !strings.HasSuffix(nudge, "target-agent: "+content) || !utf8.ValidString(nudge) {
		t.Fatalf("target-to-source nudge was unexpectedly capped or corrupted: bytes=%d utf8=%t", len(nudge), utf8.ValidString(nudge))
	}
}

func TestChainedDelegationCommentAppliesBothRoleEffects(t *testing.T) {
	s, ctx, _, first := delegationUpdatesFixture(t)
	var err error
	if err := s.InsertProject(ctx, fixtureProject("third")); err != nil {
		t.Fatal(err)
	}
	downstream, err := s.InsertTask(ctx, TaskInsert{Project: "third", Title: "Follow-up", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO task_delegations(id,source_project_ident,source_task_id,target_project_ident,target_task_id,created_at)
		VALUES('chain-fixture','target',?,'third',?,0)`, first.TargetTask.ID, downstream.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskComment(ctx, first.TargetTask.ProjectIdent, first.TargetTask.ID, "middle-agent", "agent", "shared update", nil, nil); err != nil {
		t.Fatal(err)
	}
	downstreamComments, err := readTaskComments(ctx, s.db, downstream.ID, nil)
	if err != nil || !delegationUpdatesHasSystemComment(downstreamComments, "Sender project `target`, ticket `"+first.TargetTask.ID+"`:\n\nmiddle-agent: shared update") {
		t.Fatalf("middle task source role did not clarify downstream target: %+v err=%v", downstreamComments, err)
	}
	var upstreamNudges int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE project_ident='tasks' AND subject='Delegated task comment added' AND content LIKE '%middle-agent: shared update%' ").Scan(&upstreamNudges); err != nil || upstreamNudges != 1 {
		t.Fatalf("middle task target role did not nudge upstream source: count=%d err=%v", upstreamNudges, err)
	}
}

func TestTaskDelegationContractOSChangeKeepsOwnerFenceAndReleasesIncompatibleOwner(t *testing.T) {
	s, ctx, actor, delegated := delegationUpdatesFixture(t)
	var err error
	labels := []string{"os:windows"}
	if _, err := s.UpdateTaskDelegationContract(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID, TaskDelegationUpdate{Labels: labels}, nil); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("OS change without current source owner was accepted: %v", err)
	}
	result, err := s.UpdateTaskDelegationContract(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID, TaskDelegationUpdate{Labels: labels}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceTask.Status != "todo" || result.SourceTask.OwnerOrigin != nil || result.SourceTask.OwnerAgentID != nil || len(result.SourceTask.Labels) != 1 || result.SourceTask.Labels[0] != "os:windows" {
		t.Fatalf("incompatible source owner was not released with mirrored OS metadata: %+v", result.SourceTask)
	}
	comments, err := readTaskComments(ctx, s.db, result.SourceTask.ID, nil)
	if err != nil || !delegationUpdatesHasSystemComment(comments, "released the incompatible claim") {
		t.Fatalf("OS release explanation was not preserved: %+v err=%v", comments, err)
	}
}

func TestTaskDelegationUpdateAndCommentEffectsRollbackTogether(t *testing.T) {
	s, ctx, actor, delegated := delegationUpdatesFixture(t)
	var err error
	oldTitle := delegated.SourceTask.Title
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_delegation_target_update BEFORE UPDATE ON tasks
		WHEN OLD.id='`+delegated.TargetTask.ID+`' BEGIN SELECT RAISE(ABORT,'fixture target update failure'); END`); err != nil {
		t.Fatal(err)
	}
	newTitle := "Should roll back"
	if _, err := s.UpdateTaskDelegationContract(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID, TaskDelegationUpdate{Title: &newTitle}, actor); err == nil {
		t.Fatal("target update failure did not abort contract mirroring")
	}
	source, err := s.Task(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID)
	if err != nil || source.Task.Title != oldTitle {
		t.Fatalf("source metadata escaped rollback: %+v err=%v", source, err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER reject_delegation_target_update"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_delegation_comment_nudge BEFORE INSERT ON messages
		WHEN NEW.subject='Delegated task comment added' BEGIN SELECT RAISE(ABORT,'fixture nudge failure'); END`); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_comments WHERE task_id=?", delegated.TargetTask.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskComment(ctx, delegated.TargetTask.ProjectIdent, delegated.TargetTask.ID, "target-agent", "agent", "rollback me", nil, actor); err == nil {
		t.Fatal("nudge failure did not abort target comment")
	}
	var after int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_comments WHERE task_id=?", delegated.TargetTask.ID).Scan(&after); err != nil || after != before {
		t.Fatalf("target comment escaped nudge rollback: before=%d after=%d err=%v", before, after, err)
	}
}

func delegationUpdatesFixture(t *testing.T) (*Store, context.Context, *AgentOrigin, *TaskDelegationResult) {
	t.Helper()
	ctx := context.Background()
	s := taskStore(t)
	if err := s.InsertProject(ctx, fixtureProject("target")); err != nil {
		t.Fatal(err)
	}
	actor := taskActor()
	parent, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Parent", Reporter: "fixture"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	delegated, err := s.DelegateTask(ctx, TaskDelegationInput{
		SourceProject: "tasks", TargetProject: "target", Title: "Delegated work", Reporter: "fixture", ParentID: &parent.ID,
	}, actor)
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, actor, delegated
}

func delegationUpdatesHasSystemComment(comments []TaskComment, fragment string) bool {
	for _, comment := range comments {
		if comment.AuthorType == "system" && strings.Contains(comment.Content, fragment) {
			return true
		}
	}
	return false
}
