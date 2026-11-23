package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
)

func TestProjectInboxAcknowledgementSurvivesReopenResumeAndCancel(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	actor := taskActor()
	conv := createInboxConversation(t, s, "tasks", actor)
	messageID := insertProjectMessageFixture(t, s, "tasks", "user", "resume this inbox prompt", false)
	now := time.Now().UTC()
	source := "native:inbox-resume"
	reportProjectInboxNative(t, s, "tasks", *actor, source, 1, false, now)
	if err := s.ReconcileProjectInbox(ctx, projectInboxTick(now)); err != nil {
		t.Fatal(err)
	}
	var databasePath string
	if err := s.db.QueryRowContext(ctx, "PRAGMA database_list").Scan(new(int), new(string), &databasePath); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, filepath.Clean(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	next := *actor
	next.InstanceID = uuid.NewString()
	if _, err := reopened.ResumeConversation(ctx, "tasks", conv.ID, actor.InstanceID, &next); err != nil {
		t.Fatal(err)
	}
	now = time.Now().UTC()
	reportProjectInboxNative(t, reopened, "tasks", next, source, 2, false, now)
	if err := reopened.ReconcileProjectInbox(ctx, projectInboxTick(now)); err != nil {
		t.Fatal(err)
	}
	commands, err := reopened.ConversationCommands(ctx, "tasks", conv.ID, 0, 10)
	if err != nil || len(commands) != 1 {
		t.Fatalf("resume/reconcile commands=%+v err=%v, want the original inbox prompt only", commands, err)
	}
	wantCommandID := projectInboxCommandID("tasks", actor.SessionID, messageID)
	if commands[0].ID != wantCommandID || commands[0].TargetInstanceID != next.InstanceID || commands[0].State != "queued" {
		t.Fatalf("resumed inbox command=%+v, want stable ID queued for new instance %q", commands[0], next.InstanceID)
	}
	if _, err := reopened.ClaimConversationCommand(ctx, "tasks", conv.ID, commands[0].ID, &next); err != nil {
		t.Fatalf("claim resumed inbox prompt: %v", err)
	}
	if _, err := reopened.EnqueueConversationCommand(ctx, "tasks", conv.ID, uuid.NewString(), conversation.Command{
		Kind: "cancel", PromptCommandID: commands[0].ID,
	}); err != nil {
		t.Fatalf("enqueue cancel for claimed inbox prompt: %v", err)
	}
	if err := reopened.ReconcileProjectInbox(ctx, projectInboxTick(now.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	commands, err = reopened.ConversationCommands(ctx, "tasks", conv.ID, 0, 10)
	if err != nil || len(commands) != 2 || commands[0].ID != wantCommandID || commands[1].Command.Kind != "cancel" {
		t.Fatalf("cancel/reconcile commands=%+v err=%v, want one inbox prompt and its cancel", commands, err)
	}
	var acknowledgements int
	if err := reopened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_confirmations
		WHERE project_ident='tasks' AND agent_id=? AND message_id=?`, actor.SessionID, messageID).Scan(&acknowledgements); err != nil || acknowledgements != 1 {
		t.Fatalf("inbox acknowledgement after reopen/resume/cancel=%d err=%v", acknowledgements, err)
	}
}

func TestProjectInboxRecipientSnapshotRejectsClockAdvancedPastTTL(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	actor := taskActor()
	conv := createInboxConversation(t, s, "tasks", actor)
	messageID := insertProjectMessageFixture(t, s, "tasks", "user", "stale receiver", false)
	base := time.Now().UTC()
	reportProjectInboxNative(t, s, "tasks", *actor, "native:inbox-clock-advance", 1, false, base)
	recipients, err := s.projectInboxRecipients(ctx, base.UnixMilli())
	if err != nil || len(recipients) != 1 {
		t.Fatalf("initial recipient snapshot=%+v err=%v", recipients, err)
	}
	advancedClock := base.Add(time.Duration(presenceGoneAfter) * time.Millisecond).UnixMilli()
	if _, err := s.db.ExecContext(ctx, "UPDATE amux_presence_clock SET observed_at=MAX(observed_at,?) WHERE id=1", advancedClock); err != nil {
		t.Fatal(err)
	}
	if delivered, err := s.deliverProjectInboxRecipient(ctx, recipients[0], base.UnixMilli(), 32); err != nil || delivered {
		t.Fatalf("recipient expired after its snapshot: delivered=%t err=%v", delivered, err)
	}
	assertProjectInboxNoCommandOrAck(t, s, conv.ID, messageID)
}

func TestProjectInboxRecipientSnapshotRejectsProviderOSOrGenerationReplacement(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		value string
	}{
		{name: "provider", field: "provider", value: "claude"},
		{name: "OS", field: "os", value: "windows"},
		{name: "generation", field: "generation", value: "cccccccc-cccc-4ccc-8ccc-cccccccccccc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := taskStore(t)
			actor := taskActor()
			conv := createInboxConversation(t, s, "tasks", actor)
			messageID := insertProjectMessageFixture(t, s, "tasks", "user", "binding replacement", false)
			base := time.Now().UTC()
			reportProjectInboxNative(t, s, "tasks", *actor, "native:inbox-replacement", 1, false, base)
			recipients, err := s.projectInboxRecipients(ctx, base.UnixMilli())
			if err != nil || len(recipients) != 1 {
				t.Fatalf("initial recipient snapshot=%+v err=%v", recipients, err)
			}
			query := "UPDATE amux_presence_agents SET " + tc.field + "=? WHERE project_ident='tasks' AND instance_id=? AND session_id=?"
			if _, err := s.db.ExecContext(ctx, query, tc.value, actor.InstanceID, actor.SessionID); err != nil {
				t.Fatal(err)
			}
			if delivered, err := s.deliverProjectInboxRecipient(ctx, recipients[0], base.UnixMilli(), 32); err != nil || delivered {
				t.Fatalf("replaced %s recipient: delivered=%t err=%v", tc.field, delivered, err)
			}
			assertProjectInboxNoCommandOrAck(t, s, conv.ID, messageID)
		})
	}
}

func TestProjectInboxRecipientSnapshotRejectsInstanceAddedBeforeTransaction(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	actor := taskActor()
	conv := createInboxConversation(t, s, "tasks", actor)
	messageID := insertProjectMessageFixture(t, s, "tasks", "user", "new instance before delivery", false)
	now := time.Now().UTC()
	reportProjectInboxNative(t, s, "tasks", *actor, "native:inbox-instance-race", 1, false, now)
	recipients, err := s.projectInboxRecipients(ctx, now.UnixMilli())
	if err != nil || len(recipients) != 1 {
		t.Fatalf("initial recipient snapshot=%+v err=%v", recipients, err)
	}
	secondInstance := uuid.NewString()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO amux_presence_agents(project_ident,instance_id,session_id,provider,os,source,sequence,seen_at,last_activity_at,busy,draft,auto_assign,review_context_clear,state,hostname,goal,generation,agent_id,report_kind,event_agent_json,identity_seq,activity_seq,binding_required,binding_remote)
		VALUES('tasks',?,?,?,?,'native:inbox-instance-race-second',1,?,NULL,1,0,0,0,'busy',NULL,NULL,NULL,NULL,'native',NULL,0,0,0,NULL)`, secondInstance, actor.SessionID, actor.Provider, actor.OS, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if delivered, err := s.deliverProjectInboxRecipient(ctx, recipients[0], now.UnixMilli(), 32); err != nil || delivered {
		t.Fatalf("stale recipient snapshot delivered=%t err=%v", delivered, err)
	}
	assertProjectInboxNoCommandOrAck(t, s, conv.ID, messageID)
}

func assertProjectInboxNoCommandOrAck(t *testing.T, s *Store, conversationID string, messageID int64) {
	t.Helper()
	var commands, acknowledgements int
	if err := s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM amux_conversation_commands WHERE conversation_id=?", conversationID).Scan(&commands); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM agent_confirmations WHERE message_id=?", messageID).Scan(&acknowledgements); err != nil {
		t.Fatal(err)
	}
	if commands != 0 || acknowledgements != 0 {
		t.Fatalf("stale recipient effects: commands=%d acknowledgements=%d", commands, acknowledgements)
	}
}
