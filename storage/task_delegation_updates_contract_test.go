package storage

import (
	"errors"
	"reflect"
	"testing"
)

func TestTaskDelegationUpdateUsesOnlySourceLookupAndAllowsOrdinaryOwnedMetadata(t *testing.T) {
	s, ctx, parent, actor := delegationContractFixture(t)
	delegated, err := s.DelegateTask(ctx, delegationContractInput(parent.ID), actor)
	if err != nil {
		t.Fatal(err)
	}
	before := delegationUpdateCounts(t, s)
	for _, lookup := range []struct{ project, id string }{
		{delegated.TargetTask.ProjectIdent, delegated.SourceTask.ID},
		{delegated.SourceTask.ProjectIdent, delegated.TargetTask.ID},
	} {
		if _, err := s.UpdateTaskDelegationContract(ctx, lookup.project, lookup.id, TaskDelegationUpdate{}, actor); !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("non-source lookup (%s,%s) err=%v, want task not found", lookup.project, lookup.id, err)
		}
	}
	if after := delegationUpdateCounts(t, s); after != before {
		t.Fatalf("source-only lookup failures left effects: before=%+v after=%+v", before, after)
	}

	title := "Owner-independent metadata edit (DELEGATED)"
	description := "Updated without changing the active claim."
	updated, err := s.UpdateTaskDelegationContract(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID, TaskDelegationUpdate{
		Title: &title, Description: &description,
	}, nil)
	if err != nil {
		t.Fatalf("ordinary metadata update was incorrectly blocked by task ownership: %v", err)
	}
	if updated.SourceTask.Title != title || updated.TargetTask.Title != "Owner-independent metadata edit" || updated.SourceTask.OwnerOrigin == nil || *updated.SourceTask.OwnerOrigin != *actor {
		t.Fatalf("metadata update changed title mirroring or ownership: source=%+v target=%+v", updated.SourceTask, updated.TargetTask)
	}

	labels := []string{"os:windows"}
	stableSource := updated.SourceTask
	stableTarget := updated.TargetTask
	stableCounts := delegationUpdateCounts(t, s)
	if _, err := s.UpdateTaskDelegationContract(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID, TaskDelegationUpdate{Labels: labels}, nil); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("OS retag without the current owner was accepted: %v", err)
	}
	afterSource, err := s.Task(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterTarget, err := s.Task(ctx, delegated.TargetTask.ProjectIdent, delegated.TargetTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stableSource, afterSource.Task) || !reflect.DeepEqual(stableTarget, afterTarget.Task) || delegationUpdateCounts(t, s) != stableCounts {
		t.Fatal("rejected OS retag changed either task or emitted effects")
	}
}

func TestTaskDelegationUpdateSecondSideCommentAndNudgeFailuresRollbackEverything(t *testing.T) {
	for _, failure := range []string{"target task update", "target contract comment", "target project nudge"} {
		t.Run(failure, func(t *testing.T) {
			s, ctx, parent, actor := delegationContractFixture(t)
			delegated, err := s.DelegateTask(ctx, delegationContractInput(parent.ID), actor)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.TransitionTask(ctx, delegated.TargetTask.ProjectIdent, delegated.TargetTask.ID, "in_progress", actor); err != nil {
				t.Fatal(err)
			}
			beforeSource, err := s.Task(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeTarget, err := s.Task(ctx, delegated.TargetTask.ProjectIdent, delegated.TargetTask.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeCounts := delegationUpdateCounts(t, s)
			switch failure {
			case "target task update":
				if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_delegation_contract_target BEFORE UPDATE ON tasks
					WHEN OLD.id='`+delegated.TargetTask.ID+`' BEGIN SELECT RAISE(ABORT,'injected target update failure'); END`); err != nil {
					t.Fatal(err)
				}
			case "target contract comment":
				if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_delegation_contract_comment BEFORE INSERT ON task_comments
					WHEN NEW.task_id='`+delegated.TargetTask.ID+`' AND NEW.content='Delegated task contract was updated by the source project.'
					BEGIN SELECT RAISE(ABORT,'injected target comment failure'); END`); err != nil {
					t.Fatal(err)
				}
			case "target project nudge":
				if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_delegation_contract_nudge BEFORE INSERT ON messages
					WHEN NEW.subject='Delegated task updated' BEGIN SELECT RAISE(ABORT,'injected target nudge failure'); END`); err != nil {
					t.Fatal(err)
				}
			}

			labels := []string{"os:windows"}
			title := "Rollback owner, events and both contract copies"
			if _, err := s.UpdateTaskDelegationContract(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID, TaskDelegationUpdate{
				Title: &title, Labels: labels,
			}, actor); err == nil {
				t.Fatal("injected late failure did not abort mirrored contract update")
			}
			afterSource, err := s.Task(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID)
			if err != nil {
				t.Fatal(err)
			}
			afterTarget, err := s.Task(ctx, delegated.TargetTask.ProjectIdent, delegated.TargetTask.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeSource.Task, afterSource.Task) || !reflect.DeepEqual(beforeTarget.Task, afterTarget.Task) {
				t.Fatalf("partial metadata/owner change escaped rollback:\nsource before=%+v\nsource after=%+v\ntarget before=%+v\ntarget after=%+v", beforeSource.Task, afterSource.Task, beforeTarget.Task, afterTarget.Task)
			}
			if after := delegationUpdateCounts(t, s); after != beforeCounts {
				t.Fatalf("partial comments/events/nudge escaped rollback: before=%+v after=%+v", beforeCounts, after)
			}
		})
	}
}

func TestSourceCommentAndDerivedClarificationRollbackTogether(t *testing.T) {
	s, ctx, parent, actor := delegationContractFixture(t)
	delegated, err := s.DelegateTask(ctx, delegationContractInput(parent.ID), actor)
	if err != nil {
		t.Fatal(err)
	}
	beforeSource, err := s.Task(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeTarget, err := s.Task(ctx, delegated.TargetTask.ProjectIdent, delegated.TargetTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeCounts := delegationUpdateCounts(t, s)
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_delegated_clarification BEFORE INSERT ON task_comments
		WHEN NEW.task_id='`+delegated.TargetTask.ID+`' AND NEW.content LIKE 'Sender project %'
		BEGIN SELECT RAISE(ABORT,'injected derived clarification failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskComment(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID, "source-agent", "agent", "must roll back with its mirror", nil, actor); err == nil {
		t.Fatal("derived clarification failure did not abort the original source comment")
	}
	afterSource, err := s.Task(ctx, delegated.SourceTask.ProjectIdent, delegated.SourceTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterTarget, err := s.Task(ctx, delegated.TargetTask.ProjectIdent, delegated.TargetTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeSource.Task, afterSource.Task) || !reflect.DeepEqual(beforeTarget.Task, afterTarget.Task) {
		t.Fatal("source or target task changed after derived clarification rollback")
	}
	if after := delegationUpdateCounts(t, s); after != beforeCounts {
		t.Fatalf("original comment or derived effects escaped rollback: before=%+v after=%+v", beforeCounts, after)
	}
}

type delegationUpdateEffectCounts struct {
	Events, Outbox, Comments, Messages int64
}

func delegationUpdateCounts(t *testing.T, s *Store) delegationUpdateEffectCounts {
	t.Helper()
	var counts delegationUpdateEffectCounts
	for _, row := range []struct {
		query string
		out   *int64
	}{
		{"SELECT COUNT(*) FROM task_events", &counts.Events},
		{"SELECT COUNT(*) FROM router_task_outbox", &counts.Outbox},
		{"SELECT COUNT(*) FROM task_comments", &counts.Comments},
		{"SELECT COUNT(*) FROM messages", &counts.Messages},
	} {
		if err := s.db.QueryRow(row.query).Scan(row.out); err != nil {
			t.Fatal(err)
		}
	}
	return counts
}
