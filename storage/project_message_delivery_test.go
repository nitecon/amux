package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/router"
)

func projectInboxTick(now time.Time) router.Event {
	return router.Event{Seq: 1, UID: uuid.NewString(), ID: "clock.tick", Version: 1, EmittedAt: now.UnixMilli()}
}

func reportProjectInboxNative(t *testing.T, s *Store, project string, origin AgentOrigin, source string, sequence int64, draft bool, now time.Time) {
	t.Helper()
	_, err := s.ReportPresenceWithPrincipal(context.Background(), PresenceReport{Source: source, Sequence: sequence,
		Agents: []PresenceAgent{{ProjectIdent: project, Origin: origin, Busy: true, Draft: draft}}}, now, "api-key")
	if err != nil {
		t.Fatal(err)
	}
}

func createInboxConversation(t *testing.T, s *Store, project string, origin *AgentOrigin) *Conversation {
	t.Helper()
	task, err := s.InsertTask(context.Background(), TaskInsert{Project: project, Title: "Inbox receiver", Reporter: "fixture"}, origin)
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateConversation(context.Background(), project, task.ID, origin)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func TestReconcileProjectInboxQueuesDelegatedTargetCommentAsSourcePromptAndReplaySafe(t *testing.T) {
	ctx := context.Background()
	s, _, actor, delegated := delegationUpdatesFixture(t)
	conv := createInboxConversation(t, s, delegated.SourceTask.ProjectIdent, actor)
	initialID := uuid.NewString()
	if _, err := s.EnqueueConversationCommand(ctx, conv.ProjectIdent, conv.ID, initialID, conversation.Command{Kind: "prompt", Text: "initial provider prompt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE amux_conversation_commands SET state='claimed' WHERE conversation_id=? AND id=?`, conv.ID, initialID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskComment(ctx, delegated.TargetTask.ProjectIdent, delegated.TargetTask.ID, "target-agent", "agent", "progress is ready", nil, nil); err != nil {
		t.Fatal(err)
	}
	var messageID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM messages WHERE project_ident=? AND subject='Delegated task comment added' AND content LIKE '%progress is ready%'`, delegated.SourceTask.ProjectIdent).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	reportProjectInboxNative(t, s, delegated.SourceTask.ProjectIdent, *actor, "native:project-inbox", 1, false, now)
	tick := projectInboxTick(now)
	for i := 0; i < 2; i++ {
		if err := s.ReconcileProjectInbox(ctx, tick); err != nil {
			t.Fatal(err)
		}
	}
	commands, err := s.ConversationCommands(ctx, conv.ProjectIdent, conv.ID, 0, 10)
	if err != nil || len(commands) != 2 {
		t.Fatalf("commands=%+v err=%v, want initial plus one inbox prompt", commands, err)
	}
	if commands[0].Sequence != 1 || commands[1].Sequence != 2 || !strings.Contains(commands[1].Command.Text, "[amux project message]") ||
		!strings.Contains(commands[1].Command.Text, fmt.Sprintf("message_id: %d", messageID)) || !strings.Contains(commands[1].Command.Text, "Delegated task comment added") ||
		!strings.Contains(commands[1].Command.Text, "target-agent: progress is ready") {
		t.Fatalf("source inbox prompt was not queued after inflight work: %+v", commands)
	}
	unread, err := s.GetUnconfirmedProjectMessages(ctx, delegated.SourceTask.ProjectIdent, actor.SessionID)
	if err != nil || len(unread) != 0 {
		t.Fatalf("source session inbox remains unread: %+v err=%v", unread, err)
	}
	var acks int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_confirmations WHERE project_ident=? AND agent_id=? AND message_id=?", delegated.SourceTask.ProjectIdent, actor.SessionID, messageID).Scan(&acks); err != nil || acks != 1 {
		t.Fatalf("source project message receipt count=%d err=%v", acks, err)
	}
}

func TestReconcileProjectInboxDefersDraftAndRequiresAuthenticatedCurrentSource(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	actor := taskActor()
	conversation := createInboxConversation(t, s, "tasks", actor)
	messageID := insertProjectMessageFixture(t, s, "tasks", "user", "needs attention", false)
	now := time.Now().UTC()
	reportProjectInboxNative(t, s, "tasks", *actor, "native:inbox-deferral", 1, true, now)
	tick := projectInboxTick(now)
	if err := s.ReconcileProjectInbox(ctx, tick); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_confirmations WHERE message_id=?", messageID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("draft inbox message was acknowledged: %d", count)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_conversation_commands WHERE conversation_id=?", conversation.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("draft session received command count=%d err=%v", count, err)
	}
	// An in-process report may refresh presence but cannot borrow the earlier
	// network principal's sequence proof.
	if accepted, err := s.ReportPresence(ctx, PresenceReport{Source: "native:inbox-deferral", Sequence: 2, Agents: []PresenceAgent{{ProjectIdent: "tasks", Origin: *actor, Busy: true}}}, now.Add(time.Second)); err != nil || !accepted {
		t.Fatalf("trusted report accepted=%t err=%v", accepted, err)
	}
	if err := s.ReconcileProjectInbox(ctx, projectInboxTick(now.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_confirmations WHERE message_id=?", messageID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("untrusted sequence was acknowledged count=%d err=%v", count, err)
	}
	// A fresh authenticated sequence removes the draft and restores delivery.
	reportProjectInboxNative(t, s, "tasks", *actor, "native:inbox-deferral", 3, false, now.Add(2*time.Second))
	if err := s.ReconcileProjectInbox(ctx, projectInboxTick(now.Add(2*time.Second))); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_confirmations WHERE message_id=?", messageID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("authenticated non-draft sequence did not confirm count=%d err=%v", count, err)
	}
}

func TestReconcileProjectInboxRollsBackCommandWhenPerAgentAckFails(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	actor := taskActor()
	conversation := createInboxConversation(t, s, "tasks", actor)
	messageID := insertProjectMessageFixture(t, s, "tasks", "user", "atomic delivery", false)
	now := time.Now().UTC()
	reportProjectInboxNative(t, s, "tasks", *actor, "native:inbox-rollback", 1, false, now)
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_project_inbox_ack BEFORE INSERT ON agent_confirmations
		BEGIN SELECT RAISE(ABORT,'forced ack failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileProjectInbox(ctx, projectInboxTick(now)); err == nil {
		t.Fatal("expected per-agent ack failure")
	}
	var commands, acks int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_conversation_commands WHERE conversation_id=?", conversation.ID).Scan(&commands); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_confirmations WHERE message_id=?", messageID).Scan(&acks); err != nil {
		t.Fatal(err)
	}
	if commands != 0 || acks != 0 {
		t.Fatalf("failed atomic delivery left commands=%d acknowledgements=%d", commands, acks)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_project_inbox_ack"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileProjectInbox(ctx, projectInboxTick(now)); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_conversation_commands WHERE conversation_id=?", conversation.ID).Scan(&commands); err != nil || commands != 1 {
		t.Fatalf("retry did not deliver once commands=%d err=%v", commands, err)
	}
}

func TestProjectInboxRejectsAmbiguousLiveSessionIncludingDraftInstance(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	actor := taskActor()
	conversation := createInboxConversation(t, s, "tasks", actor)
	messageID := insertProjectMessageFixture(t, s, "tasks", "user", "ambiguous session", false)
	now := time.Now().UTC()
	reportProjectInboxNative(t, s, "tasks", *actor, "native:inbox-ambiguous", 1, false, now)
	other := *actor
	other.InstanceID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	_, err := s.db.ExecContext(ctx, `INSERT INTO amux_presence_agents(project_ident,instance_id,session_id,provider,os,source,sequence,seen_at,last_activity_at,busy,draft,auto_assign,review_context_clear,state,hostname,goal,generation,agent_id,report_kind,event_agent_json,identity_seq,activity_seq,binding_required,binding_remote)
		VALUES('tasks',?,?,?,?,'native:second-instance',1,?,NULL,1,1,0,0,'busy',NULL,NULL,NULL,NULL,'native',NULL,0,0,0,NULL)`, other.InstanceID, other.SessionID, other.Provider, other.OS, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	recipient := projectInboxRecipient{project: "tasks", instance: actor.InstanceID, session: actor.SessionID, provider: actor.Provider, os: actor.OS, source: "native:inbox-ambiguous", sequence: 1}
	if delivered, err := s.deliverProjectInboxRecipient(ctx, recipient, now.UnixMilli(), 32); err != nil || delivered {
		t.Fatalf("ambiguous identity delivered=%t err=%v", delivered, err)
	}
	var commands, acks int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_conversation_commands WHERE conversation_id=?", conversation.ID).Scan(&commands); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_confirmations WHERE message_id=?", messageID).Scan(&acks); err != nil || commands != 0 || acks != 0 {
		t.Fatalf("ambiguous session effects commands=%d acks=%d err=%v", commands, acks, err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM amux_presence_agents WHERE project_ident='tasks' AND instance_id=?", other.InstanceID); err != nil {
		t.Fatal(err)
	}
	createInboxConversation(t, s, "tasks", actor)
	if err := s.ReconcileProjectInbox(ctx, projectInboxTick(now)); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_confirmations WHERE message_id=?", messageID).Scan(&acks); err != nil || acks != 0 {
		t.Fatalf("ambiguous exact-owner conversations were acknowledged count=%d err=%v", acks, err)
	}
}

func TestProjectInboxPromptIsBoundedAndPreservesStoredMessage(t *testing.T) {
	message := ProjectMessage{ID: 77, ProjectIdent: "tasks", Source: "user", MessageType: "comment", SentAt: 1, Content: strings.Repeat("💡", 20_000)}
	text := renderProjectInboxPrompt(message)
	if len(text) > projectInboxPromptLimit || !utf8.ValidString(text) || !strings.Contains(text, "call project_message_thread with id 77") {
		t.Fatalf("bounded prompt bytes=%d valid=%t cue=%t", len(text), utf8.ValidString(text), strings.Contains(text, "call project_message_thread with id 77"))
	}
	if message.Content != strings.Repeat("💡", 20_000) {
		t.Fatal("rendering mutated the stored message value")
	}
}

func TestProjectInboxBacklogDrainsAcrossTicksAndFullRecipientDoesNotStarvePeer(t *testing.T) {
	t.Run("drains after queue capacity frees", func(t *testing.T) {
		ctx := context.Background()
		s := taskStore(t)
		actor := taskActor()
		conv := createInboxConversation(t, s, "tasks", actor)
		for i := 0; i < 130; i++ {
			insertProjectMessageFixture(t, s, "tasks", "user", fmt.Sprintf("backlog message %d", i), false)
		}
		now := time.Now().UTC()
		reportProjectInboxNative(t, s, "tasks", *actor, "native:inbox-backlog", 1, false, now)
		for i := 0; i < 5; i++ {
			tick := projectInboxTick(now.Add(time.Duration(i) * time.Second))
			tick.Seq = int64(i + 1)
			if err := s.ReconcileProjectInbox(ctx, tick); err != nil {
				t.Fatal(err)
			}
		}
		var queued, acknowledgements int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_conversation_commands WHERE conversation_id=? AND state IN('queued','claimed') AND kind='prompt'", conv.ID).Scan(&queued); err != nil || queued != 128 {
			t.Fatalf("full backlog queue=%d err=%v", queued, err)
		}
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_confirmations WHERE project_ident='tasks' AND agent_id=?", actor.SessionID).Scan(&acknowledgements); err != nil || acknowledgements != 128 {
			t.Fatalf("full backlog acknowledgements=%d err=%v", acknowledgements, err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE amux_conversation_commands SET state='completed' WHERE conversation_id=? AND sequence<=4`, conv.ID); err != nil {
			t.Fatal(err)
		}
		tick := projectInboxTick(now.Add(6 * time.Second))
		tick.Seq = 6
		if err := s.ReconcileProjectInbox(ctx, tick); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_confirmations WHERE project_ident='tasks' AND agent_id=?", actor.SessionID).Scan(&acknowledgements); err != nil || acknowledgements != 130 {
			t.Fatalf("freed queue slots did not drain remaining messages: acknowledgements=%d err=%v", acknowledgements, err)
		}
	})

	t.Run("full recipient does not block another session", func(t *testing.T) {
		ctx := context.Background()
		s := taskStore(t)
		first, second := taskActor(), *taskActor()
		firstConv := createInboxConversation(t, s, "tasks", first)
		second.SessionID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
		second.InstanceID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
		secondConv := createInboxConversation(t, s, "tasks", &second)
		for i := 0; i < 128; i++ {
			if _, err := s.EnqueueConversationCommand(ctx, "tasks", firstConv.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: fmt.Sprintf("occupied %d", i)}); err != nil {
				t.Fatal(err)
			}
		}
		messageID := insertProjectMessageFixture(t, s, "tasks", "user", "available for peer", false)
		now := time.Now().UTC()
		reportProjectInboxNative(t, s, "tasks", *first, "native:inbox-full-a", 1, false, now)
		reportProjectInboxNative(t, s, "tasks", second, "native:inbox-full-b", 1, false, now)
		if err := s.ReconcileProjectInbox(ctx, projectInboxTick(now)); err != nil {
			t.Fatal(err)
		}
		var firstAcks, secondAcks, secondCommands int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_confirmations WHERE message_id=? AND agent_id=?", messageID, first.SessionID).Scan(&firstAcks); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_confirmations WHERE message_id=? AND agent_id=?", messageID, second.SessionID).Scan(&secondAcks); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_conversation_commands WHERE conversation_id=?", secondConv.ID).Scan(&secondCommands); err != nil || firstAcks != 0 || secondAcks != 1 || secondCommands != 1 {
			t.Fatalf("recipient isolation firstAcks=%d secondAcks=%d secondCommands=%d err=%v", firstAcks, secondAcks, secondCommands, err)
		}
	})
}
