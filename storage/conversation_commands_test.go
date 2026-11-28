package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/identity/agent"
)

func commandConversation(t *testing.T) (*Store, *Conversation, *agent.Origin) {
	t.Helper()
	s := taskStore(t)
	task := makeTask(t, s, nil)
	actor := taskActor()
	c, err := s.CreateConversation(context.Background(), "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindConversationSession(context.Background(), "tasks", c.ID, "native", actor); err != nil {
		t.Fatal(err)
	}
	return s, c, actor
}

func TestPendingTaskConversationsUsesPendingPromptIndex(t *testing.T) {
	ctx := context.Background()
	s, existing, actor := commandConversation(t)
	c, err := s.CreateConversationWithPrincipal(ctx, "tasks", existing.TaskID, actor, nil, "api-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindConversationSessionWithPrincipal(ctx, "tasks", c.ID, "native", actor, "api-key"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+pendingTaskConversationsQuery, "tasks", actor.SessionID, actor.Provider, actor.OS, "api-key")
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	details := strings.Join(plan, "\n")
	if !strings.Contains(details, "USING INDEX amux_pending_conversation_prompts") ||
		!strings.Contains(details, "SEARCH c USING INDEX sqlite_autoindex_amux_conversations_1 (id=?)") {
		t.Fatalf("discovery must scan pending prompts and look up conversations by ID:\n%s", details)
	}

	check := func(project string, owner agent.Origin, want bool) {
		t.Helper()
		ids, err := s.PendingTaskConversations(ctx, project, owner, "api-key")
		if err != nil {
			t.Fatal(err)
		}
		if want && (len(ids) != 1 || ids[0] != c.ID) || !want && len(ids) != 0 {
			t.Fatalf("pending conversations: %v, want match %v", ids, want)
		}
	}
	check("tasks", *actor, false)
	first, second := uuid.NewString(), uuid.NewString()
	for _, id := range []string{first, second} {
		if _, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, id, conversation.Command{Kind: "prompt", Text: "Follow up"}); err != nil {
			t.Fatal(err)
		}
	}
	check("tasks", *actor, true) // Multiple pending prompts yield one thread.
	check("other-project", *actor, false)
	for _, field := range []string{"session", "provider", "os"} {
		other := *actor
		switch field {
		case "session":
			other.SessionID = uuid.NewString()
		case "provider":
			other.Provider = "other-provider"
		case "os":
			other.OS = "other-os"
		}
		check("tasks", other, false)
	}
	for _, id := range []string{first, second} {
		if _, err := s.ClaimConversationCommand(ctx, "tasks", c.ID, id, actor); err != nil {
			t.Fatal(err)
		}
		check("tasks", *actor, true) // Claimed prompts remain discoverable.
		if _, err := s.FinishConversationCommand(ctx, "tasks", c.ID, id, "done", actor); err != nil {
			t.Fatal(err)
		}
	}
	check("tasks", *actor, false) // Terminal prompts leave the partial index.
}

func TestConversationCommandsDurableClaimAndIdempotency(t *testing.T) {
	ctx := context.Background()
	s, c, actor := commandConversation(t)
	id := uuid.NewString()
	input := conversation.Command{Kind: "prompt", Text: "Inspect the task"}
	command, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, id, input)
	if err != nil || command.State != "queued" || command.Sequence != 1 {
		t.Fatalf("enqueue %+v %v", command, err)
	}
	retry, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, id, input)
	if err != nil || retry.ID != id || retry.Sequence != 1 {
		t.Fatalf("retry %+v %v", retry, err)
	}
	input.Text = "different"
	if _, err = s.EnqueueConversationCommand(ctx, "tasks", c.ID, id, input); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	claimed, err := s.ClaimConversationCommand(ctx, "tasks", c.ID, id, actor)
	if err != nil || claimed.State != "claimed" {
		t.Fatalf("claim %+v %v", claimed, err)
	}
	claimed, err = s.ClaimConversationCommand(ctx, "tasks", c.ID, id, actor)
	if err != nil || claimed.State != "claimed" {
		t.Fatalf("claim retry %+v %v", claimed, err)
	}
	if _, err = s.FinishConversationCommand(ctx, "tasks", c.ID, id, "done", actor); err != nil {
		t.Fatal(err)
	}
	terminal, err := s.FinishConversationCommand(ctx, "tasks", c.ID, id, "done", actor)
	if err != nil || terminal.State != "done" {
		t.Fatalf("finish retry %+v %v", terminal, err)
	}
	if _, err = s.FinishConversationCommand(ctx, "tasks", c.ID, id, "rejected", actor); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("terminal outcome changed: %v", err)
	}
	var dbSequence int
	var dbName, dbPath string
	if err := s.db.QueryRowContext(ctx, "PRAGMA database_list").Scan(&dbSequence, &dbName, &dbPath); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	list, err := reopened.ConversationCommands(ctx, "tasks", c.ID, 0, 100)
	if err != nil || len(list) != 1 || list[0].State != "done" {
		t.Fatalf("durable result %+v %v", list, err)
	}
}
func TestConversationPermissionRequiresCurrentOfferedEvent(t *testing.T) {
	ctx := context.Background()
	s, c, actor := commandConversation(t)
	permissionID := uuid.NewString()
	event := conversation.Event{Kind: "permission", SessionID: "native", RequestID: `"rpc-1"`, Options: []conversation.PermissionOption{{ID: "reject", Name: "Reject", Kind: "reject_once"}, {ID: "allow", Name: "Allow", Kind: "allow_once"}}}
	if _, err := s.AppendConversationEvent(ctx, "tasks", c.ID, permissionID, event, actor); err != nil {
		t.Fatal(err)
	}
	input := conversation.Command{Kind: "permission", PermissionEventID: permissionID, OptionID: "invented"}
	if _, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), input); !errors.Is(err, ErrConversationInput) {
		t.Fatalf("unoffered choice accepted: %v", err)
	}
	input.OptionID = "reject"
	command, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), input)
	if err != nil || command.RequestID != event.RequestID {
		t.Fatalf("permission %+v %v", command, err)
	}
	input.OptionID = "allow"
	if _, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), input); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("second permission choice accepted: %v", err)
	}
	// A later turn completion makes any unanswered permission from that turn stale.
	anotherID := uuid.NewString()
	event.RequestID = `"rpc-2"`
	if _, err = s.AppendConversationEvent(ctx, "tasks", c.ID, anotherID, event, actor); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendConversationEvent(ctx, "tasks", c.ID, uuid.NewString(), conversation.Event{Kind: "turn_completed", SessionID: "native", Status: "cancelled"}, actor); err != nil {
		t.Fatal(err)
	}
	input.PermissionEventID = anotherID
	if _, err = s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), input); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("ended turn permission accepted: %v", err)
	}
}
func TestConversationResumeInterruptsClaimedWorkWithoutReexecution(t *testing.T) {
	ctx := context.Background()
	s, c, actor := commandConversation(t)
	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	for _, id := range ids[:2] {
		if _, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, id, conversation.Command{Kind: "prompt", Text: "Inspect"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ClaimConversationCommand(ctx, "tasks", c.ID, ids[0], actor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, ids[2], conversation.Command{Kind: "cancel", PromptCommandID: ids[0]}); err != nil {
		t.Fatal(err)
	}
	successor := *actor
	successor.InstanceID = uuid.NewString()
	if _, err := s.ResumeConversation(ctx, "tasks", c.ID, actor.InstanceID, &successor); err != nil {
		t.Fatal(err)
	}
	records, err := s.ConversationCommands(ctx, "tasks", c.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if records[0].State != "interrupted" || records[1].State != "queued" || records[1].TargetInstanceID != successor.InstanceID || records[2].State != "interrupted" {
		t.Fatalf("resume records %+v", records)
	}
	if _, err = s.ClaimConversationCommand(ctx, "tasks", c.ID, ids[0], &successor); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("uncertain prompt replayed: %v", err)
	}
	if _, err = s.ClaimConversationCommand(ctx, "tasks", c.ID, ids[1], actor); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("stale worker claimed: %v", err)
	}
	if _, err = s.ClaimConversationCommand(ctx, "tasks", c.ID, ids[1], &successor); err != nil {
		t.Fatal(err)
	}
}

func TestQueuedPermissionExpiresBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	s, c, actor := commandConversation(t)
	permissionID := uuid.NewString()
	event := conversation.Event{Kind: "permission", SessionID: "native", RequestID: `"reused-rpc"`, Options: []conversation.PermissionOption{{ID: "allow", Name: "Allow", Kind: "allow_once"}}}
	if _, err := s.AppendConversationEvent(ctx, "tasks", c.ID, permissionID, event, actor); err != nil {
		t.Fatal(err)
	}
	command, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), conversation.Command{Kind: "permission", PermissionEventID: permissionID, OptionID: "allow"})
	if err != nil {
		t.Fatal(err)
	}
	// Even within one turn the agent may reuse an RPC ID after resolving an old request.
	if _, err = s.AppendConversationEvent(ctx, "tasks", c.ID, uuid.NewString(), event, actor); err != nil {
		t.Fatal(err)
	}
	result, err := s.ClaimConversationCommand(ctx, "tasks", c.ID, command.ID, actor)
	if err != nil || result.State != "rejected" {
		t.Fatalf("stale permission dispatch %+v %v", result, err)
	}
}
func TestCancellationTargetsOnlyItsPrompt(t *testing.T) {
	ctx := context.Background()
	s, c, actor := commandConversation(t)
	prompt, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: "Work"})
	if err != nil {
		t.Fatal(err)
	}
	input := conversation.Command{Kind: "cancel", PromptCommandID: prompt.ID}
	if _, err = s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), input); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("queued prompt considered active: %v", err)
	}
	if _, err = s.ClaimConversationCommand(ctx, "tasks", c.ID, prompt.ID, actor); err != nil {
		t.Fatal(err)
	}
	cancel, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishConversationCommand(ctx, "tasks", c.ID, prompt.ID, "done", actor); err != nil {
		t.Fatal(err)
	}
	result, err := s.ClaimConversationCommand(ctx, "tasks", c.ID, cancel.ID, actor)
	if err != nil || result.State != "rejected" {
		t.Fatalf("cancel after target completed %+v %v", result, err)
	}
}

func TestConversationPromptClaimsAreSerialAndFIFO(t *testing.T) {
	ctx := context.Background()
	s, c, actor := commandConversation(t)
	ids := []string{uuid.NewString(), uuid.NewString()}
	for _, id := range ids {
		if _, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, id, conversation.Command{Kind: "prompt", Text: "Work"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ClaimConversationCommand(ctx, "tasks", c.ID, ids[1], actor); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("later prompt bypassed FIFO: %v", err)
	}
	if _, err := s.ClaimConversationCommand(ctx, "tasks", c.ID, ids[0], actor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimConversationCommand(ctx, "tasks", c.ID, ids[1], actor); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("concurrent prompt accepted: %v", err)
	}
	if _, err := s.FinishConversationCommand(ctx, "tasks", c.ID, ids[0], "done", actor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimConversationCommand(ctx, "tasks", c.ID, ids[1], actor); err != nil {
		t.Fatal(err)
	}
}

func TestConversationCommandChangesReplayStateUpdates(t *testing.T) {
	ctx := context.Background()
	s, c, actor := commandConversation(t)
	command, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: "Inspect"})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := s.ConversationCommandChanges(ctx, "tasks", c.ID, 0, 100)
	if err != nil || len(changes) != 1 || changes[0].Revision <= 0 || changes[0].State != "queued" {
		t.Fatalf("initial changes %+v %v", changes, err)
	}
	after := changes[0].Revision
	if _, err = s.ClaimConversationCommand(ctx, "tasks", c.ID, command.ID, actor); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishConversationCommand(ctx, "tasks", c.ID, command.ID, "done", actor); err != nil {
		t.Fatal(err)
	}
	changes, err = s.ConversationCommandChanges(ctx, "tasks", c.ID, after, 100)
	if err != nil || len(changes) != 2 || changes[0].State != "done" || changes[1].Revision <= changes[0].Revision {
		t.Fatalf("state changes %+v %v", changes, err)
	}
	// Change records are notifications of current committed state, not intent to execute again.
	for _, change := range changes {
		if change.ID != command.ID || change.State != "done" {
			t.Fatalf("changed intent %+v", change)
		}
	}
}

func TestConversationCommandsBoundQueuedIntent(t *testing.T) {
	ctx := context.Background()
	s, c, actor := commandConversation(t)
	var firstID string
	for i := 0; i < 128; i++ {
		id := uuid.NewString()
		if i == 0 {
			firstID = id
		}
		if _, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, id, conversation.Command{Kind: "prompt", Text: "Work"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: "overflow"}); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("unbounded queue: %v", err)
	}
	if _, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: strings.Repeat("\x01", 1<<19)}); !errors.Is(err, ErrConversationInput) {
		t.Fatalf("unbounded encoded prompt: %v", err)
	}
	if _, err := s.ClaimConversationCommand(ctx, "tasks", c.ID, firstID, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), conversation.Command{Kind: "cancel", PromptCommandID: firstID}); err != nil {
		t.Fatalf("prompt backlog blocked cancellation: %v", err)
	}
}

func TestConversationPermissionFromPriorInstanceCannotBeReused(t *testing.T) {
	ctx := context.Background()
	s, c, actor := commandConversation(t)
	id := uuid.NewString()
	if _, err := s.AppendConversationEvent(ctx, "tasks", c.ID, id, conversation.Event{Kind: "permission", SessionID: "native", RequestID: `"old-rpc"`, Options: []conversation.PermissionOption{{ID: "allow"}}}, actor); err != nil {
		t.Fatal(err)
	}
	successor := *actor
	successor.InstanceID = uuid.NewString()
	if _, err := s.ResumeConversation(ctx, "tasks", c.ID, actor.InstanceID, &successor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), conversation.Command{Kind: "permission", PermissionEventID: id, OptionID: "allow"}); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("old-instance permission accepted: %v", err)
	}
}

func TestConversationCommandReplayBoundsEncodedPayload(t *testing.T) {
	ctx := context.Background()
	s, c, _ := commandConversation(t)
	// Control characters expand in JSON, so raw text length is not a wire bound.
	for i := 0; i < 8; i++ {
		if _, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: strings.Repeat("\x01", 128<<10)}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.ConversationCommands(ctx, "tasks", c.ID, 0, 1000)
	if err != nil || len(page) == 0 || len(page) == 8 {
		t.Fatalf("unbounded page length %d: %v", len(page), err)
	}
	raw, err := json.Marshal(page)
	if err != nil || len(raw) > (4<<20)+1024 {
		t.Fatalf("encoded page size %d: %v", len(raw), err)
	}
	rest, err := s.ConversationCommands(ctx, "tasks", c.ID, page[len(page)-1].Sequence, 1000)
	if err != nil || len(rest)+len(page) != 8 {
		t.Fatalf("paged intent lost: %d + %d: %v", len(page), len(rest), err)
	}
}
