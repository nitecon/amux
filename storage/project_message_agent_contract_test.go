package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/nitecon/amux/identity/agent"
)

func TestAgentProjectMessageReceiptSurvivesReopenAndReceiptFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "messages.sqlite")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if store != nil {
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	if err := store.InsertProject(ctx, Project{Ident: "native", ChannelName: "internal", RoomID: "native", CreatedAt: 1, Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	origin := agent.Origin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", OS: "linux"}
	eventAt := int64(1234)
	input := AgentProjectMessageInput{Content: "durable native message", MessageType: "message", Subject: ptr("subject"), Hostname: ptr("host"), EventAt: &eventAt}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_internal_receipt BEFORE UPDATE OF external_message_id ON messages
		BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if message, err := store.CreateAgentProjectMessage(ctx, "native", origin, input); err == nil || message != nil {
		t.Fatalf("receipt failure returned message=%+v err=%v", message, err)
	}
	for _, check := range []struct {
		table string
		query string
		args  []any
	}{
		{table: "messages", query: "SELECT COUNT(*) FROM messages WHERE project_ident='native'"},
		{table: "agents", query: "SELECT COUNT(*) FROM agents WHERE project_ident='native' AND agent_id=?", args: []any{origin.SessionID}},
		{table: "agent_confirmations", query: "SELECT COUNT(*) FROM agent_confirmations WHERE project_ident='native' AND agent_id=?", args: []any{origin.SessionID}},
	} {
		var count int
		if err := store.db.QueryRowContext(ctx, check.query, check.args...).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed receipt left %s rows=%d err=%v", check.table, count, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER fail_internal_receipt"); err != nil {
		t.Fatal(err)
	}
	message, err := store.CreateAgentProjectMessage(ctx, "native", origin, input)
	if err != nil {
		t.Fatal(err)
	}
	wantReceipt := fmt.Sprintf("internal:%d", message.ID)
	if message.ExternalMessageID == nil || *message.ExternalMessageID != wantReceipt || message.Source != "agent" || message.AgentID == nil || *message.AgentID != origin.SessionID || message.Content != input.Content || message.MessageType != input.MessageType || message.Subject == nil || *message.Subject != "subject" || message.Hostname == nil || *message.Hostname != "host" || message.EventAt == nil || *message.EventAt != 1234 || message.ConfirmedAt == nil {
		t.Fatalf("stored agent message=%+v, want internal receipt, sender confirmation and metadata", message)
	}
	var registrations, confirmations int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE project_ident='native' AND agent_id=?`, origin.SessionID).Scan(&registrations); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_confirmations WHERE project_ident='native' AND agent_id=? AND message_id=?`, origin.SessionID, message.ID).Scan(&confirmations); err != nil {
		t.Fatal(err)
	}
	if registrations != 1 || confirmations != 1 {
		t.Fatalf("sender registration=%d confirmation=%d, want one each", registrations, confirmations)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = nil
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := store.GetProjectMessageThread(ctx, "native", message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if thread.Root.ExternalMessageID == nil || *thread.Root.ExternalMessageID != wantReceipt || thread.Root.Content != input.Content || thread.Root.Subject == nil || *thread.Root.Subject != "subject" || thread.Root.Hostname == nil || *thread.Root.Hostname != "host" || thread.Root.EventAt == nil || *thread.Root.EventAt != 1234 || len(thread.Confirmations) != 1 || thread.Confirmations[0].AgentID != origin.SessionID {
		t.Fatalf("reopened native message=%+v, want persisted receipt and sender confirmation", thread)
	}
}
