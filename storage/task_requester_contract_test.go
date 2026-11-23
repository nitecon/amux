package storage

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

const requesterContractRemote = "github.com/example/requester-contract"

func requesterContractSetup(t *testing.T) (*Store, context.Context, AgentOrigin, AgentOrigin, AgentOrigin) {
	t.Helper()
	ctx := context.Background()
	s := taskStore(t)
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET canonical_remote=? WHERE ident='tasks'", requesterContractRemote); err != nil {
		t.Fatal(err)
	}
	requester := AgentOrigin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", OS: "linux"}
	executor := AgentOrigin{SessionID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", InstanceID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Provider: "claude", OS: "linux"}
	peer := AgentOrigin{SessionID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", InstanceID: "ffffffff-ffff-4fff-8fff-ffffffffffff", Provider: "antigravity", OS: "linux"}
	return s, ctx, requester, executor, peer
}

func registerRequesterContractTarget(t *testing.T, s *Store, origin AgentOrigin, generation string, seenAt time.Time) RegisteredTarget {
	t.Helper()
	remote := requesterContractRemote
	target, err := s.RegisterTarget(context.Background(), "tasks", TargetRegistration{
		Origin: origin, Generation: generation, CanonicalRemote: &remote,
	}, seenAt)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func requesterContractCounts(t *testing.T, s *Store) [5]int64 {
	t.Helper()
	var counts [5]int64
	queries := []string{
		"SELECT COUNT(*) FROM tasks",
		"SELECT COUNT(*) FROM task_events",
		"SELECT COUNT(*) FROM router_task_outbox",
		"SELECT COUNT(*) FROM task_comments",
		"SELECT COUNT(*) FROM amux_conversation_commands",
	}
	for i, query := range queries {
		if err := s.db.QueryRow(query).Scan(&counts[i]); err != nil {
			t.Fatal(err)
		}
	}
	return counts
}

func TestTaskRequesterCreationIsIndependentAndNativeCreationIsSelfOnly(t *testing.T) {
	s, ctx, requester, executor, peer := requesterContractSetup(t)
	requesterTarget := registerRequesterContractTarget(t, s, requester, "11111111-1111-4111-8111-111111111111", time.Now())
	peerTarget := registerRequesterContractTarget(t, s, peer, "22222222-2222-4222-8222-222222222222", time.Now())

	created, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Human task with requester", Reporter: "fixture", Requester: &requesterTarget.Target}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.OwnerOrigin != nil || created.OwnerAgentID != nil || !jsonEqualRequester(created.Requester, requesterTarget.Target) {
		t.Fatalf("requester changed task ownership or was not stored exactly: %+v", created)
	}
	events, err := s.TaskEvents(ctx, EventQuery{TaskID: &created.ID})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Task
	for _, event := range events {
		if event.Kind == "task_created" {
			if err := json.Unmarshal(event.Task, &snapshot); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if snapshot.ID != created.ID || snapshot.OwnerOrigin != nil || snapshot.OwnerAgentID != nil || !jsonEqualRequester(snapshot.Requester, requesterTarget.Target) {
		t.Fatalf("creation event missed independent requester snapshot: %+v", snapshot)
	}

	before := requesterContractCounts(t, s)
	if _, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Foreign requester rejected", Reporter: "fixture", Requester: &requesterTarget.Target}, &executor); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("native caller selected another target as requester: %v", err)
	}
	if after := requesterContractCounts(t, s); after != before {
		t.Fatalf("rejected native requester left effects: before=%v after=%v", before, after)
	}
	self, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Self requester", Reporter: "fixture", Requester: &peerTarget.Target}, &peer)
	if err != nil || self.OwnerOrigin == nil || *self.OwnerOrigin != peer || !jsonEqualRequester(self.Requester, peerTarget.Target) {
		t.Fatalf("native self-requester task: %+v %v", self, err)
	}
	reporterOnly, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Reporter is not requester", Reporter: requester.SessionID}, &requester)
	if err != nil || reporterOnly.OwnerOrigin == nil || *reporterOnly.OwnerOrigin != requester || len(reporterOnly.Requester) != 0 {
		t.Fatalf("reporter identity implicitly became requester: %+v %v", reporterOnly, err)
	}
}

func TestTaskRequesterOwnerAndPriorRequesterCanSetOrClearWithoutChangingWork(t *testing.T) {
	s, ctx, requester, executor, peer := requesterContractSetup(t)
	requesterTarget := registerRequesterContractTarget(t, s, requester, "11111111-1111-4111-8111-111111111111", time.Now())
	executorTarget := registerRequesterContractTarget(t, s, executor, "22222222-2222-4222-8222-222222222222", time.Now())
	peerTarget := registerRequesterContractTarget(t, s, peer, "33333333-3333-4333-8333-333333333333", time.Now())
	foreign := AgentOrigin{SessionID: "99999999-9999-4999-8999-999999999999", InstanceID: "88888888-8888-4888-8888-888888888888", Provider: "codex", OS: "linux"}
	foreignTarget := registerRequesterContractTarget(t, s, foreign, "44444444-4444-4444-8444-444444444444", time.Now())
	humanTask, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Human selects requester", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	humanSelected, err := s.SetTaskRequester(ctx, "tasks", humanTask.ID, &peerTarget.Target, nil)
	if err != nil || !jsonEqualRequester(humanSelected.Requester, peerTarget.Target) || humanSelected.OwnerOrigin != nil {
		t.Fatalf("human could not select another registered requester independently: %+v %v", humanSelected, err)
	}
	task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Requester handoff", Reporter: "fixture", Requester: &requesterTarget.Target}, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", &executor)
	if err != nil {
		t.Fatal(err)
	}

	updated, err := s.SetTaskRequester(ctx, "tasks", task.ID, &requesterTarget.Target, &requester)
	if err != nil || !jsonEqualRequester(updated.Requester, requesterTarget.Target) {
		t.Fatalf("prior requester could not reaffirm its subscription: %+v %v", updated, err)
	}
	updated, err = s.SetTaskRequester(ctx, "tasks", task.ID, nil, &requester)
	if err != nil || len(updated.Requester) != 0 {
		t.Fatalf("prior requester could not clear subscription: %+v %v", updated, err)
	}
	if _, err := s.SetTaskRequester(ctx, "tasks", task.ID, &requesterTarget.Target, &requester); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("former requester retained authority after clear: %v", err)
	}
	updated, err = s.SetTaskRequester(ctx, "tasks", task.ID, &executorTarget.Target, &executor)
	if err != nil || !jsonEqualRequester(updated.Requester, executorTarget.Target) {
		t.Fatalf("current owner could not set its own subscription: %+v %v", updated, err)
	}
	updated, err = s.SetTaskRequester(ctx, "tasks", task.ID, nil, &executor)
	if err != nil || len(updated.Requester) != 0 {
		t.Fatalf("current owner could not clear subscription: %+v %v", updated, err)
	}
	before := requesterContractCounts(t, s)
	if _, err := s.SetTaskRequester(ctx, "tasks", task.ID, &foreignTarget.Target, &foreign); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("foreign native caller changed requester: %v", err)
	}
	if after := requesterContractCounts(t, s); after != before {
		t.Fatalf("rejected foreign write left effects: before=%v after=%v", before, after)
	}
	current, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || current.Task.Status != claimed.Status || current.Task.OwnerOrigin == nil || *current.Task.OwnerOrigin != executor || current.Task.StartedAt == nil || !reflect.DeepEqual(current.Task.StartedAt, claimed.StartedAt) {
		t.Fatalf("requester write changed execution state: task=%+v err=%v", current, err)
	}
}

func TestTaskRequesterCanClearAfterDoneOrUserReview(t *testing.T) {
	for _, status := range []string{"done", "user_review"} {
		t.Run(status, func(t *testing.T) {
			s, ctx, requester, _, _ := requesterContractSetup(t)
			requesterTarget := registerRequesterContractTarget(t, s, requester, "11111111-1111-4111-8111-111111111111", time.Now())
			task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Clear after " + status, Reporter: "fixture", Requester: &requesterTarget.Target}, nil)
			if err != nil {
				t.Fatal(err)
			}
			update := TaskWorkflowUpdate{Status: status, Changeset: json.RawMessage(`null`), Policy: TaskTransitionPolicy{HumanRequest: true}}
			if status == "user_review" {
				update.ReviewAction = stringPointer("Choose the next step")
			}
			current, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, update, nil)
			if err != nil || current.Status != status {
				t.Fatalf("transition to %s: %+v %v", status, current, err)
			}
			cleared, err := s.SetTaskRequester(ctx, "tasks", task.ID, nil, nil)
			if err != nil || len(cleared.Requester) != 0 || cleared.Status != status {
				t.Fatalf("clear requester after %s: %+v %v", status, cleared, err)
			}
		})
	}
}

func TestTaskRequesterRejectsStaleRegistrationWithoutMutation(t *testing.T) {
	for _, change := range []string{"expired", "generation changed", "OS mismatch", "canonical remote rebound", "project archived"} {
		t.Run(change, func(t *testing.T) {
			s, ctx, requester, _, _ := requesterContractSetup(t)
			seenAt := time.Now()
			if change == "expired" {
				seenAt = time.Now().Add(-time.Duration(targetRegistrationLease)*time.Millisecond - time.Second)
			}
			registered := registerRequesterContractTarget(t, s, requester, "11111111-1111-4111-8111-111111111111", seenAt)
			labels := []string(nil)
			if change == "OS mismatch" {
				labels = []string{"os:windows"}
			}
			task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Reject stale requester " + change, Reporter: "fixture", Labels: labels}, nil)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "generation changed":
				if _, err := s.db.ExecContext(ctx, "UPDATE task_target_presence SET generation='22222222-2222-4222-8222-222222222222' WHERE project_ident='tasks' AND session_id=?", requester.SessionID); err != nil {
					t.Fatal(err)
				}
			case "canonical remote rebound":
				if _, err := s.db.ExecContext(ctx, "UPDATE projects SET canonical_remote='github.com/example/rebound' WHERE ident='tasks'"); err != nil {
					t.Fatal(err)
				}
			case "project archived":
				if _, err := s.SetProjectArchived(ctx, "tasks", true); err != nil {
					t.Fatal(err)
				}
			}
			before := requesterContractCounts(t, s)
			_, err = s.SetTaskRequester(ctx, "tasks", task.ID, &registered.Target, nil)
			if err == nil {
				t.Fatal("stale or incompatible requester registration was accepted")
			}
			if after := requesterContractCounts(t, s); after != before {
				t.Fatalf("rejected requester write left effects: before=%v after=%v", before, after)
			}
			current, readErr := s.Task(ctx, "tasks", task.ID)
			if readErr != nil || current.Task.Requester != nil || current.Task.Status != task.Status || current.Task.UpdatedAt != task.UpdatedAt {
				t.Fatalf("rejected requester write changed task: %+v readErr=%v setterErr=%v", current, readErr, err)
			}
		})
	}
}

func TestTaskRequesterRejectedStaleGenerationPreservesExistingSubscriptionAndAudit(t *testing.T) {
	s, ctx, requester, _, _ := requesterContractSetup(t)
	requesterTarget := registerRequesterContractTarget(t, s, requester, "11111111-1111-4111-8111-111111111111", time.Now())
	task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Keep current requester", Reporter: "fixture", Requester: &requesterTarget.Target}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE task_target_presence SET generation='33333333-3333-4333-8333-333333333333' WHERE project_ident='tasks' AND session_id=?", requester.SessionID); err != nil {
		t.Fatal(err)
	}
	beforeCounts := requesterContractCounts(t, s)
	before, err := s.Task(ctx, "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTaskRequester(ctx, "tasks", task.ID, &requesterTarget.Target, nil); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("stale requester generation error=%v, want conflict", err)
	}
	afterCounts := requesterContractCounts(t, s)
	after, err := s.Task(ctx, "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterCounts != beforeCounts || after.Task.UpdatedAt != before.Task.UpdatedAt || after.Task.Status != before.Task.Status || after.Task.OwnerOrigin != nil || !jsonEqualRequester(after.Task.Requester, requesterTarget.Target) {
		t.Fatalf("rejected stale generation changed subscription, task, or audit: counts %v->%v before=%+v after=%+v", beforeCounts, afterCounts, before.Task, after.Task)
	}
}

func TestTaskRequesterRegistrationInAnotherProjectCannotCreateOrDelegate(t *testing.T) {
	s, ctx, requester, _, _ := requesterContractSetup(t)
	if err := s.InsertProject(ctx, fixtureProject("requester-only")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET canonical_remote='github.com/example/requester-only' WHERE ident='requester-only'"); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, fixtureProject("target")); err != nil {
		t.Fatal(err)
	}
	remote := "github.com/example/requester-only"
	registered, err := s.RegisterTarget(ctx, "requester-only", TargetRegistration{
		Origin: requester, Generation: "11111111-1111-4111-8111-111111111111", CanonicalRemote: &remote,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Delegation parent", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := delegationContractCounts(t, s, ctx)
	if _, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Wrong-project requester", Reporter: "fixture", Requester: &registered.Target}, nil); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("requester registered only in another project accepted for task creation: %v", err)
	}
	if after := delegationContractCounts(t, s, ctx); after != before {
		t.Fatalf("wrong-project requester task creation left effects: before=%+v after=%+v", before, after)
	}
	input := delegationContractInput(parent.ID)
	input.Requester = &registered.Target
	if _, err := s.DelegateTask(ctx, input, nil); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("requester registered only in another project accepted for delegation: %v", err)
	}
	if after := delegationContractCounts(t, s, ctx); after != before {
		t.Fatalf("wrong-project requester delegation left effects: before=%+v after=%+v", before, after)
	}
}

func TestTaskRequesterStaleConversationFenceRollsBackAuthorizedPriorRequesterWrite(t *testing.T) {
	s, ctx, requester, _, _ := requesterContractSetup(t)
	requesterTarget := registerRequesterContractTarget(t, s, requester, "11111111-1111-4111-8111-111111111111", time.Now())
	task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Fenced requester", Reporter: "fixture", Requester: &requesterTarget.Target}, &requester)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := s.CreateConversation(ctx, "tasks", task.ID, &requester)
	if err != nil {
		t.Fatal(err)
	}
	replacement := requester
	replacement.InstanceID = "77777777-7777-4777-8777-777777777777"
	if _, err := s.ResumeConversation(ctx, "tasks", conversation.ID, requester.InstanceID, &replacement); err != nil {
		t.Fatal(err)
	}
	before := requesterContractCounts(t, s)
	current, err := s.Task(ctx, "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTaskRequester(WithConversationFence(ctx, "tasks", conversation.ID), "tasks", task.ID, nil, &requester); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("stale conversation owner changed requester: %v", err)
	}
	if after := requesterContractCounts(t, s); after != before {
		t.Fatalf("stale fenced write left effects: before=%v after=%v", before, after)
	}
	afterTask, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || afterTask.Task.UpdatedAt != current.Task.UpdatedAt || !jsonEqualRequester(afterTask.Task.Requester, requesterTarget.Target) || afterTask.Task.OwnerOrigin == nil || *afterTask.Task.OwnerOrigin != replacement {
		t.Fatalf("stale fenced write changed requester/task owner: %+v %v", afterTask, err)
	}
}

func TestRequesterResultRequiresExactExplicitSubscriptionAndNeverBroadcasts(t *testing.T) {
	requester := notifierTestAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	requester.Generation = stringPointer("11111111-1111-4111-8111-111111111111")
	requester.AgentID = stringPointer("shared-display-name")
	owner := notifierTestAgent("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	task := json.RawMessage(`{"id":"result-task","project_ident":"tasks","status":"done","labels":[],"owner_origin":null,"owner_agent_id":null,"requester":{"origin":{"session_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","instance_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","provider":"codex","os":"linux"},"generation":"11111111-1111-4111-8111-111111111111"}}`)
	if !routerNotifierRecipient("task_completed", task, json.RawMessage(`{"kind":"user"}`), requester) {
		t.Fatal("explicit requester must receive an unowned result")
	}
	peer := requester
	peer.SessionID, peer.InstanceID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", "ffffffff-ffff-4fff-8fff-ffffffffffff"
	if routerNotifierRecipient("task_completed", task, json.RawMessage(`null`), peer) {
		t.Fatal("unowned requester result was broadcast to a same-name peer")
	}
	replacement := requester
	replacement.Generation = stringPointer("22222222-2222-4222-8222-222222222222")
	if routerNotifierRecipient("task_completed", task, json.RawMessage(`null`), replacement) {
		t.Fatal("replacement generation inherited an old requester subscription")
	}
	noRequester := json.RawMessage(`{"id":"result-task","status":"done","labels":[],"reporter":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","owner_origin":{"session_id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","instance_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd"},"requester":null}`)
	if routerNotifierRecipient("task_completed", noRequester, json.RawMessage(`{"session_id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","instance_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd"}`), requester) {
		t.Fatal("reporter identity became an implicit requester on another agent's result event")
	}
	if routerNotifierRecipient("task_completed", task, json.RawMessage(`null`), owner) {
		t.Fatal("executor received the result through the requester path for unowned task")
	}
}

func TestRequesterResultIsRevalidatedBeforeNativePromptDelivery(t *testing.T) {
	ctx := context.Background()
	s, _, requester, executor, _ := requesterContractSetup(t)
	const generation = "11111111-1111-4111-8111-111111111111"
	requesterTarget := registerRequesterContractTarget(t, s, requester, generation, time.Now())
	requesterWork := makeTask(t, s, &requester)
	if _, err := s.CreateConversation(ctx, "tasks", requesterWork.ID, &requester); err != nil {
		t.Fatal(err)
	}
	completed, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Requested result", Reporter: "fixture", Requester: &requesterTarget.Target}, nil)
	if err != nil {
		t.Fatal(err)
	}
	completed, err = s.TransitionTask(ctx, "tasks", completed.ID, "in_progress", &executor)
	if err != nil {
		t.Fatal(err)
	}
	completed, err = s.UpdateTaskWorkflow(ctx, "tasks", completed.ID, TaskWorkflowUpdate{
		Status: "done", Changeset: json.RawMessage(`null`),
	}, &executor)
	if err != nil {
		t.Fatal(err)
	}
	taskJSON, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	ownerJSON, err := json.Marshal(executor)
	if err != nil {
		t.Fatal(err)
	}
	reportNativeDeliveryAgentGeneration(t, s, &requester, 1, true, generation)
	cause := notifierCause(t, s, "task.completed", notifierPayload(string(taskJSON), string(ownerJSON)))
	if err := s.ReconcileRouterNotifiers(ctx, notifierRegistry(t), cause); err != nil {
		t.Fatal(err)
	}
	var notifyUID string
	if err := s.db.QueryRowContext(ctx, `SELECT uid FROM router_events WHERE id='agent.notify' AND json_extract(payload,'$.target')=? ORDER BY seq DESC LIMIT 1`, requester.SessionID).Scan(&notifyUID); err != nil {
		t.Fatal(err)
	}
	notice, err := readRouterEventByUID(ctx, s.db, notifyUID)
	if err != nil {
		t.Fatal(err)
	}
	registry := nativeDeliveryRegistry(t)
	if pending, err := s.ReconcileNativeDelivery(ctx, registry, notice); err != nil || !pending {
		t.Fatalf("drafted requester result should freeze and wait: pending=%t err=%v", pending, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands", int64(0))
	if _, err := s.SetTaskRequester(ctx, "tasks", completed.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	reportNativeDeliveryAgentGeneration(t, s, &requester, 2, false, generation)
	if pending, err := s.ReconcileNativeDelivery(ctx, registry, notice); err != nil || pending {
		t.Fatalf("removed requester should settle without prompt: pending=%t err=%v", pending, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands WHERE kind='prompt'", int64(0))
	assertEqual(t, s.db, "SELECT status FROM amux_native_delivery_receipts WHERE notify_uid='"+notice.UID+"'", "skipped")
	assertEqual(t, s.db, "SELECT COUNT(*) FROM router_events WHERE id='agent.notify_result' AND correlation='"+notice.UID+"'", int64(1))
}

func jsonEqualRequester(raw json.RawMessage, want RegisteredTargetIdentity) bool {
	var actual RegisteredTargetIdentity
	return json.Unmarshal(raw, &actual) == nil && actual == want
}
