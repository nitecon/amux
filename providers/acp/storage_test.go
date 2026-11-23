package acp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/storage"
)

func TestACPEventsPersistAgainstTheirTaskBeforeDelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "chat.db")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.InsertProject(ctx, storage.Project{Ident: "chat", ChannelName: "chat", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "chat", Title: "Inspect code", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := &storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "antigravity", OS: "linux"}
	bound, err := store.CreateConversation(ctx, "chat", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan conversation.Event, 16)
	session, err := Start(ctx, Config{
		Command: []string{os.Args[0], "-test.run=^TestACPProcessFixture$"}, Directory: t.TempDir(),
		Env: append(os.Environ(), "AMUX_ACP_FIXTURE=1"),
	}, func(ctx context.Context, event conversation.Event) error {
		if event.Kind == "session_ready" {
			if _, err := store.BindConversationSession(ctx, "chat", bound.ID, event.SessionID, actor); err != nil {
				return err
			}
		}
		if _, err := store.AppendConversationEvent(ctx, "chat", bound.ID, uuid.NewString(), event, actor); err != nil {
			return err
		}
		select {
		case events <- event:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	done := make(chan error, 1)
	go func() { _, err := session.Prompt(ctx, "Inspect code"); done <- err }()
	for {
		select {
		case event := <-events:
			if event.Kind == "permission" {
				// The UI/worker receives a permission only after it is replayable.
				persisted, err := store.ConversationEvents(ctx, "chat", bound.ID, 0, 100)
				if err != nil || len(persisted) != 4 || persisted[3].Event.Kind != "permission" {
					t.Fatalf("permission not committed before delivery: %d %v", len(persisted), err)
				}
				if err = session.ResolvePermission(ctx, event.RequestID, "reject-once"); err != nil {
					t.Fatal(err)
				}
			}
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := storage.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			record, err := reopened.Conversation(ctx, "chat", bound.ID)
			if err != nil || record.TaskID != task.ID || record.NativeSessionID != session.ID() {
				t.Fatalf("durable binding %+v %v", record, err)
			}
			persisted, err := reopened.ConversationEvents(ctx, "chat", bound.ID, 0, 100)
			if err != nil || len(persisted) != 5 || persisted[4].Event.Kind != "turn_completed" {
				t.Fatalf("durable transcript %d %v", len(persisted), err)
			}
			return
		case <-ctx.Done():
			t.Fatal("ACP durable conversation timed out")
		}
	}
}
