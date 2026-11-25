package desktop

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/storage"
)

func TestTaskArchiveSurvivesReplyToSameAgent(t *testing.T) {
	root := t.TempDir()
	owner := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "macos"}
	info := storage.Conversation{ID: uuid.NewString(), ProjectIdent: "demo", TaskID: uuid.NewString(), Provider: owner.Provider, Owner: owner}
	run := storage.WorkerRun{Owner: owner, Directory: root}
	archive, err := openTaskConversationArchive(root, info, run)
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.AppendEvent(context.Background(), conversation.Event{Kind: "user_message", Text: "Original task run"}); err != nil {
		t.Fatal(err)
	}
	info.ID = uuid.NewString()
	run.Owner.InstanceID = uuid.NewString()
	info.Owner = run.Owner
	resumed, err := openTaskConversationArchive(root, info, run)
	if err != nil || resumed.EventCount() != 1 {
		t.Fatalf("resumed task did not retain its agent transcript: %+v, %v", resumed, err)
	}
}
