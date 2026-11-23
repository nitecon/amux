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

func TestDelegateTaskCreatesAtomicCrossProjectHierarchyAndUnassignedTarget(t *testing.T) {
	s, ctx, parent, actor := delegationContractFixture(t)
	assertDelegationProjectForeignKeys(t, s.db)
	input := delegationContractInput(parent.ID)
	result, err := s.DelegateTask(ctx, input, actor)
	if err != nil {
		t.Fatal(err)
	}

	source, target := &result.SourceTask, &result.TargetTask
	if source.ProjectIdent != input.SourceProject || source.Kind != "delegated" || source.DelegatedToProjectIdent == nil || *source.DelegatedToProjectIdent != input.TargetProject || source.DelegatedToTaskID == nil || *source.DelegatedToTaskID != target.ID {
		t.Fatalf("wrong source tracking task: %+v", source)
	}
	if source.Parent == nil || source.Parent.ID != parent.ID || source.Parent.ProjectIdent != input.SourceProject {
		t.Fatalf("source task is not attached to its source-project parent: %+v", source.Parent)
	}
	if source.OwnerOrigin == nil || *source.OwnerOrigin != *actor || source.OwnerAgentID == nil || *source.OwnerAgentID != actor.SessionID {
		t.Fatalf("native source ownership was not retained: %+v", source)
	}
	if source.Goal != nil {
		t.Fatalf("delegation copied parent goal onto source tracker: %q", *source.Goal)
	}
	if target.ProjectIdent != input.TargetProject || target.Status != "todo" || target.OwnerOrigin != nil || target.OwnerAgentID != nil || target.StartedAt != nil {
		t.Fatalf("delegated target was not queued unowned: %+v", target)
	}
	if target.Parent == nil || target.Parent.ID != source.ID || target.Parent.ProjectIdent != input.SourceProject {
		t.Fatalf("target is not attached to the cross-project source tracker: %+v", target.Parent)
	}
	if target.Goal != nil {
		t.Fatalf("delegation copied source goal onto target: %q", *target.Goal)
	}
	if result.MessageID <= 0 {
		t.Fatalf("delegation omitted durable target-project notice ID: %d", result.MessageID)
	}

	for _, task := range []*Task{source, target} {
		events, err := s.TaskEvents(ctx, EventQuery{TaskID: &task.ID})
		if err != nil {
			t.Fatal(err)
		}
		var created *TaskEvent
		for i := range events {
			if events[i].Kind == "task_created" {
				created = &events[i]
				break
			}
		}
		if created == nil {
			t.Fatalf("no creation event for delegated task %s", task.ID)
		}
		var snapshot Task
		if err := json.Unmarshal(created.Task, &snapshot); err != nil || snapshot.Parent == nil || snapshot.Parent.ID != task.Parent.ID || snapshot.Parent.ProjectIdent != task.Parent.ProjectIdent {
			t.Fatalf("creation event missed parent snapshot for %s: %+v %v", task.ID, snapshot.Parent, err)
		}
		assertDelegationReadySnapshot(t, created.Task, false)
		if task.ID == source.ID {
			if snapshot.Kind != "delegated" || snapshot.DelegatedToProjectIdent == nil || *snapshot.DelegatedToProjectIdent != input.TargetProject || snapshot.DelegatedToTaskID == nil || *snapshot.DelegatedToTaskID != target.ID {
				t.Fatalf("source creation event missed delegated link snapshot: %+v", snapshot)
			}
		} else if snapshot.OwnerOrigin != nil || snapshot.OwnerAgentID != nil || snapshot.Status != "todo" {
			t.Fatalf("target creation event must remain queued and unowned: %+v", snapshot)
		}
	}
	for _, task := range []*Task{source, target} {
		var outboxTaskJSON string
		err := s.db.QueryRowContext(ctx, `SELECT task_json
			FROM router_task_outbox WHERE kind='created' AND json_extract(task_json,'$.id')=? ORDER BY id DESC LIMIT 1`, task.ID).Scan(&outboxTaskJSON)
		if err != nil {
			t.Fatalf("read created router outbox snapshot for %s: %v", task.ID, err)
		}
		var outboxTask Task
		if err := json.Unmarshal([]byte(outboxTaskJSON), &outboxTask); err != nil || outboxTask.Parent == nil || outboxTask.Parent.ID != task.Parent.ID || outboxTask.Parent.ProjectIdent != task.Parent.ProjectIdent {
			t.Fatalf("created router outbox snapshot missed hierarchy for %s: %+v %v", task.ID, outboxTask.Parent, err)
		}
		assertDelegationReadySnapshot(t, json.RawMessage(outboxTaskJSON), false)
		if task.ID == source.ID {
			if outboxTask.Kind != "delegated" || outboxTask.DelegatedToProjectIdent == nil || *outboxTask.DelegatedToProjectIdent != input.TargetProject || outboxTask.DelegatedToTaskID == nil || *outboxTask.DelegatedToTaskID != target.ID {
				t.Fatalf("source router outbox missed delegated link snapshot: %+v", outboxTask)
			}
		} else if outboxTask.OwnerOrigin != nil || outboxTask.OwnerAgentID != nil || outboxTask.Status != "todo" {
			t.Fatalf("target router outbox must remain queued and unowned: %+v", outboxTask)
		}
	}
	var delegationRows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_delegations WHERE source_project_ident=? AND source_task_id=?
		AND target_project_ident=? AND target_task_id=?`, source.ProjectIdent, source.ID, target.ProjectIdent, target.ID).Scan(&delegationRows); err != nil || delegationRows != 1 {
		t.Fatalf("delegation link count=%d err=%v", delegationRows, err)
	}
}

func assertDelegationReadySnapshot(t *testing.T, raw json.RawMessage, want bool) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	value, ok := fields["delegation_ready"]
	if !ok {
		t.Fatal("delegation task snapshot omitted delegation_ready")
	}
	var ready bool
	if err := json.Unmarshal(value, &ready); err != nil || ready != want {
		t.Fatalf("delegation_ready=%s err=%v, want %t", value, err, want)
	}
}

func TestDelegateTaskRequiresValidSourceParentWithoutEffects(t *testing.T) {
	s, ctx, _, actor := delegationContractFixture(t)
	otherParent, err := s.InsertTask(ctx, TaskInsert{Project: "target", Title: "Wrong-project parent", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := delegationContractCounts(t, s, ctx)
	for name, parentID := range map[string]*string{
		"missing":             nil,
		"unknown":             stringPointer("missing-parent"),
		"wrong project scope": &otherParent.ID,
	} {
		t.Run(name, func(t *testing.T) {
			input := delegationContractInputPtr(parentID)
			if _, err := s.DelegateTask(ctx, input, actor); err == nil {
				t.Fatal("invalid/missing source parent was accepted for a native delegation")
			}
			if after := delegationContractCounts(t, s, ctx); after != before {
				t.Fatalf("invalid parent created partial delegation effects: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestDelegateTaskRegisteredRequesterRequiresCurrentSameOriginAndCompatibleOS(t *testing.T) {
	t.Run("current same-origin registration is returned exactly", func(t *testing.T) {
		s, ctx, parent, actor := delegationContractFixture(t)
		if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='adhoc' WHERE ident='tasks'"); err != nil {
			t.Fatal(err)
		}
		generation := "99999999-9999-4999-8999-999999999999"
		registered, err := s.RegisterTarget(ctx, "tasks", TargetRegistration{Origin: *actor, Generation: generation}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		input := delegationContractInput(parent.ID)
		input.Requester = &registered.Target
		result, err := s.DelegateTask(ctx, input, actor)
		if err != nil {
			t.Fatal(err)
		}
		var requester RegisteredTargetIdentity
		if err := json.Unmarshal(result.SourceTask.Requester, &requester); err != nil || requester != registered.Target {
			t.Fatalf("source task requester receipt=%+v want=%+v err=%v", requester, registered.Target, err)
		}
		events, err := s.TaskEvents(ctx, EventQuery{TaskID: &result.SourceTask.ID})
		if err != nil {
			t.Fatal(err)
		}
		var createdTask *Task
		for i := range events {
			if events[i].Kind == "task_created" {
				createdTask = new(Task)
				if err := json.Unmarshal(events[i].Task, createdTask); err != nil {
					t.Fatal(err)
				}
				break
			}
		}
		var eventRequester RegisteredTargetIdentity
		if createdTask == nil || json.Unmarshal(createdTask.Requester, &eventRequester) != nil || eventRequester != registered.Target {
			t.Fatalf("source creation event dropped requester subscription: %+v", createdTask)
		}
		var outboxJSON string
		if err := s.db.QueryRowContext(ctx, `SELECT task_json FROM router_task_outbox WHERE kind='created' AND json_extract(task_json,'$.id')=? ORDER BY id DESC LIMIT 1`, result.SourceTask.ID).Scan(&outboxJSON); err != nil {
			t.Fatal(err)
		}
		var outboxTask Task
		var outboxRequester RegisteredTargetIdentity
		if err := json.Unmarshal([]byte(outboxJSON), &outboxTask); err != nil || json.Unmarshal(outboxTask.Requester, &outboxRequester) != nil || outboxRequester != registered.Target {
			t.Fatalf("source router outbox dropped requester subscription: %+v %v", outboxTask.Requester, err)
		}
		if len(result.TargetTask.Requester) != 0 {
			t.Fatalf("requester authority leaked to destination task: %s", result.TargetTask.Requester)
		}
	})

	for _, test := range []struct {
		name      string
		requester func(*AgentOrigin, string) RegisteredTargetIdentity
		seenAt    func() time.Time
		labels    []string
	}{
		{
			name: "foreign registered origin",
			requester: func(_ *AgentOrigin, generation string) RegisteredTargetIdentity {
				foreign := *taskActor()
				foreign.SessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
				foreign.InstanceID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
				return RegisteredTargetIdentity{Origin: foreign, Generation: generation}
			},
			seenAt: time.Now,
		},
		{
			name: "stale generation",
			requester: func(actor *AgentOrigin, _ string) RegisteredTargetIdentity {
				return RegisteredTargetIdentity{Origin: *actor, Generation: "88888888-8888-4888-8888-888888888888"}
			},
			seenAt: time.Now,
		},
		{
			name: "expired registration",
			requester: func(actor *AgentOrigin, generation string) RegisteredTargetIdentity {
				return RegisteredTargetIdentity{Origin: *actor, Generation: generation}
			},
			seenAt: func() time.Time {
				return time.Now().Add(-time.Duration(targetRegistrationLease)*time.Millisecond - time.Second)
			},
		},
		{
			name: "OS mismatch",
			requester: func(actor *AgentOrigin, generation string) RegisteredTargetIdentity {
				return RegisteredTargetIdentity{Origin: *actor, Generation: generation}
			},
			seenAt: time.Now,
			labels: []string{"os:windows"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, ctx, parent, actor := delegationContractFixture(t)
			if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='adhoc' WHERE ident='tasks'"); err != nil {
				t.Fatal(err)
			}
			generation := "99999999-9999-4999-8999-999999999999"
			registrationOrigin := *actor
			if test.name == "foreign registered origin" {
				registrationOrigin = *taskActor()
				registrationOrigin.SessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
				registrationOrigin.InstanceID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
			}
			if _, err := s.RegisterTarget(ctx, "tasks", TargetRegistration{Origin: registrationOrigin, Generation: generation}, test.seenAt()); err != nil {
				t.Fatal(err)
			}
			requester := test.requester(actor, generation)
			input := delegationContractInput(parent.ID)
			input.Requester, input.Labels = &requester, test.labels
			before := delegationContractCounts(t, s, ctx)
			if _, err := s.DelegateTask(ctx, input, actor); err == nil {
				t.Fatal("invalid requester was accepted")
			}
			if after := delegationContractCounts(t, s, ctx); after != before {
				t.Fatalf("invalid requester created delegation effects: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestDelegateTaskRejectsRequesterAfterSourceBindingChanges(t *testing.T) {
	for _, change := range []string{"canonical remote rebound", "source project archived"} {
		t.Run(change, func(t *testing.T) {
			s, ctx, parent, actor := delegationContractFixture(t)
			const originalRemote = "github.com/example/source-original"
			if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='repo',canonical_remote=? WHERE ident='tasks'", originalRemote); err != nil {
				t.Fatal(err)
			}
			generation := "99999999-9999-4999-8999-999999999999"
			registered, err := s.RegisterTarget(ctx, "tasks", TargetRegistration{
				Origin: *actor, Generation: generation, CanonicalRemote: stringPointer(originalRemote),
			}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			input := delegationContractInput(parent.ID)
			input.Requester = &registered.Target

			if change == "canonical remote rebound" {
				if _, err := s.db.ExecContext(ctx, "UPDATE projects SET canonical_remote='github.com/example/source-replacement' WHERE ident='tasks'"); err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.SetProjectArchived(ctx, "tasks", true); err != nil {
				t.Fatal(err)
			}

			before := delegationContractCounts(t, s, ctx)
			if _, err := s.DelegateTask(ctx, input, actor); err == nil {
				t.Fatal("requester registration remained authoritative after source binding changed")
			}
			if after := delegationContractCounts(t, s, ctx); after != before {
				t.Fatalf("stale requester binding created delegation effects: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestDelegateTaskLateNoticeFailureRollsBackEntireTaskTree(t *testing.T) {
	s, ctx, parent, actor := delegationContractFixture(t)
	before := delegationContractCounts(t, s, ctx)
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_delegation_notice BEFORE INSERT ON messages
		WHEN NEW.subject='Delegated task created' BEGIN SELECT RAISE(ABORT,'injected late delegation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DelegateTask(ctx, delegationContractInput(parent.ID), actor); err == nil {
		t.Fatal("injected late notice failure did not abort delegation")
	}
	if after := delegationContractCounts(t, s, ctx); after != before {
		t.Fatalf("late failure left partial delegation state: before=%+v after=%+v", before, after)
	}
}

func TestDelegatedTargetCompletionWaitsForSourceAcceptance(t *testing.T) {
	s, ctx, parent, actor := delegationContractFixture(t)
	result, err := s.DelegateTask(ctx, delegationContractInput(parent.ID), actor)
	if err != nil {
		t.Fatal(err)
	}
	targetDone, err := s.UpdateTaskWorkflow(ctx, result.TargetTask.ProjectIdent, result.TargetTask.ID, TaskWorkflowUpdate{
		Status: "done", Changeset: json.RawMessage("null"), Policy: TaskTransitionPolicy{HumanRequest: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if targetDone.Status != "done" {
		t.Fatalf("target did not complete: %+v", targetDone)
	}
	source, err := s.Task(ctx, result.SourceTask.ProjectIdent, result.SourceTask.ID)
	if err != nil || source.Task.Status != "todo" {
		t.Fatalf("target completion auto-accepted or closed the source tracker: %+v %v", source, err)
	}
	var completedAt sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT completed_at FROM task_delegations WHERE source_task_id=?", result.SourceTask.ID).Scan(&completedAt); err != nil {
		t.Fatal(err)
	}
	if completedAt.Valid {
		t.Fatal("target completion recorded source acceptance")
	}

	sourceDone, err := s.UpdateTaskWorkflow(ctx, result.SourceTask.ProjectIdent, result.SourceTask.ID, TaskWorkflowUpdate{
		Status: "done", Changeset: json.RawMessage("null"), Policy: TaskTransitionPolicy{HumanRequest: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sourceDone.Status != "done" {
		t.Fatalf("source acceptance did not close tracker: %+v", sourceDone)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT completed_at FROM task_delegations WHERE source_task_id=?", result.SourceTask.ID).Scan(&completedAt); err != nil || !completedAt.Valid {
		t.Fatalf("source acceptance did not complete delegation: %v %v", completedAt, err)
	}
	target, err := s.Task(ctx, result.TargetTask.ProjectIdent, result.TargetTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsSystemComment(target.Comments, "confirmed") {
		t.Fatalf("accepted delegation did not leave confirmation on target: %+v", target.Comments)
	}
}

func TestDelegatedSourceEarlyCloseDoesNotAcceptUnfinishedTarget(t *testing.T) {
	s, ctx, parent, actor := delegationContractFixture(t)
	result, err := s.DelegateTask(ctx, delegationContractInput(parent.ID), actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTaskWorkflow(ctx, result.SourceTask.ProjectIdent, result.SourceTask.ID, TaskWorkflowUpdate{
		Status: "done", Changeset: json.RawMessage("null"), Policy: TaskTransitionPolicy{HumanRequest: true},
	}, nil); err != nil {
		t.Fatal(err)
	}
	var completedAt sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT completed_at FROM task_delegations WHERE source_task_id=?", result.SourceTask.ID).Scan(&completedAt); err != nil {
		t.Fatal(err)
	}
	if completedAt.Valid {
		t.Fatal("closing source before target completion accepted the delegation")
	}
	if _, err := s.UpdateTaskWorkflow(ctx, result.TargetTask.ProjectIdent, result.TargetTask.ID, TaskWorkflowUpdate{
		Status: "done", Changeset: json.RawMessage("null"), Policy: TaskTransitionPolicy{HumanRequest: true},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT completed_at FROM task_delegations WHERE source_task_id=?", result.SourceTask.ID).Scan(&completedAt); err != nil {
		t.Fatal(err)
	}
	if completedAt.Valid {
		t.Fatal("later target completion silently accepted an already-closed source tracker")
	}
}

func TestDelegationMigrationPreservesImportedRustHistoryAndNullsPurgedMessage(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "delegation-profile.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"tasks", "target"} {
		if err := s.InsertProject(ctx, fixtureProject(project)); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Parent", Reporter: "fixture"}, taskActor())
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.DelegateTask(ctx, delegationContractInput(parent.ID), taskActor())
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	if result.MessageID <= 0 {
		s.Close()
		t.Fatal("fixture delegation has no referenced message")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacy.SetMaxOpenConns(1)
	var delegationID, sourceProject, sourceID, targetProject, targetID string
	var requesterID, requesterHost sql.NullString
	var createdAt int64
	if err := legacy.QueryRowContext(ctx, `SELECT id,source_project_ident,source_task_id,target_project_ident,target_task_id,
		requester_agent_id,requester_hostname,created_at FROM task_delegations WHERE source_task_id=?`, result.SourceTask.ID).
		Scan(&delegationID, &sourceProject, &sourceID, &targetProject, &targetID, &requesterID, &requesterHost, &createdAt); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `PRAGMA foreign_keys=OFF;
		DROP TABLE task_delegations;
		CREATE TABLE task_delegations (
			id TEXT PRIMARY KEY,
			source_project_ident TEXT NOT NULL REFERENCES projects(ident),
			source_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			target_project_ident TEXT NOT NULL REFERENCES projects(ident),
			target_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			requester_agent_id TEXT,
			requester_hostname TEXT,
			created_at INTEGER NOT NULL,
			completed_at INTEGER,
			completion_message_id INTEGER REFERENCES messages(id) ON DELETE CASCADE
		);
		CREATE UNIQUE INDEX idx_task_delegations_source ON task_delegations(source_project_ident,source_task_id);
		CREATE UNIQUE INDEX idx_task_delegations_target ON task_delegations(target_project_ident,target_task_id);`); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `INSERT INTO task_delegations(id,source_project_ident,source_task_id,target_project_ident,target_task_id,
		requester_agent_id,requester_hostname,created_at,completed_at,completion_message_id) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		delegationID, sourceProject, sourceID, targetProject, targetID, nullableDBValue(requesterID), nullableDBValue(requesterHost), createdAt, 123, result.MessageID); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	var deleteRule string
	if err := reopened.db.QueryRowContext(ctx, `SELECT on_delete FROM pragma_foreign_key_list('task_delegations') WHERE "from"='completion_message_id'`).Scan(&deleteRule); err != nil || strings.ToUpper(deleteRule) != "SET NULL" {
		t.Fatalf("legacy completion FK action=%q err=%v", deleteRule, err)
	}
	assertDelegationProjectForeignKeys(t, reopened.db)
	if _, err := reopened.db.ExecContext(ctx, "DELETE FROM messages WHERE id=?", result.MessageID); err != nil {
		t.Fatal(err)
	}
	var retainedCompletedAt int64
	var retainedMessage sql.NullInt64
	if err := reopened.db.QueryRowContext(ctx, "SELECT completed_at,completion_message_id FROM task_delegations WHERE id=?", delegationID).Scan(&retainedCompletedAt, &retainedMessage); err != nil || retainedCompletedAt != 123 || retainedMessage.Valid {
		t.Fatalf("message purge lost delegation history: completed_at=%d message=%+v err=%v", retainedCompletedAt, retainedMessage, err)
	}
}

type delegationCounts struct {
	Tasks, Parents, Links, Comments, Messages, Events, Outbox int64
}

func delegationContractCounts(t *testing.T, s *Store, ctx context.Context) delegationCounts {
	t.Helper()
	var counts delegationCounts
	for _, query := range []struct {
		name string
		into *int64
		sql  string
	}{
		{"tasks", &counts.Tasks, "SELECT COUNT(*) FROM tasks"},
		{"parents", &counts.Parents, "SELECT COUNT(*) FROM task_subtasks"},
		{"delegations", &counts.Links, "SELECT COUNT(*) FROM task_delegations"},
		{"comments", &counts.Comments, "SELECT COUNT(*) FROM task_comments"},
		{"messages", &counts.Messages, "SELECT COUNT(*) FROM messages"},
		{"events", &counts.Events, "SELECT COUNT(*) FROM task_events"},
		{"outbox", &counts.Outbox, "SELECT COUNT(*) FROM router_task_outbox"},
	} {
		if err := s.db.QueryRowContext(ctx, query.sql).Scan(query.into); err != nil {
			t.Fatalf("count %s: %v", query.name, err)
		}
	}
	return counts
}

func nullableDBValue(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func assertDelegationProjectForeignKeys(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT "from","table","to" FROM pragma_foreign_key_list('task_delegations')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	projects := map[string]bool{}
	for rows.Next() {
		var from, table, to string
		if err := rows.Scan(&from, &table, &to); err != nil {
			t.Fatal(err)
		}
		if table == "projects" && to == "ident" {
			projects[from] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !projects["source_project_ident"] || !projects["target_project_ident"] {
		t.Fatalf("delegation project foreign keys missing: %+v", projects)
	}
}

func delegationContractFixture(t *testing.T) (*Store, context.Context, *Task, *AgentOrigin) {
	t.Helper()
	s := taskStore(t)
	ctx := context.Background()
	if err := s.InsertProject(ctx, fixtureProject("target")); err != nil {
		t.Fatal(err)
	}
	actor := taskActor()
	goal := "release"
	parent, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Parent", Reporter: "fixture", Goal: &goal}, actor)
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, parent, actor
}

func delegationContractInput(parentID string) TaskDelegationInput {
	return delegationContractInputPtr(&parentID)
}

func delegationContractInputPtr(parentID *string) TaskDelegationInput {
	description := "Delegation context"
	details := "Build the dependency and report its verification."
	return TaskDelegationInput{
		SourceProject: "tasks", TargetProject: "target", Title: "Build dependency", Reporter: "fixture",
		ParentID: parentID, Description: &description, Details: &details,
	}
}

func containsSystemComment(comments []TaskComment, fragment string) bool {
	for _, comment := range comments {
		if comment.AuthorType == "system" && strings.Contains(strings.ToLower(comment.Content), strings.ToLower(fragment)) {
			return true
		}
	}
	return false
}
