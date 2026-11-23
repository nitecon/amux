package storage

import (
	"context"
	"testing"
	"time"
)

func TestLegacyNestedProjectMessageThreadAndHumanReplyFollowSourceOneHop(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	now := time.Now().UnixMilli()
	rootA := insertThreadContractMessage(t, s, "tasks", "user", "A root", nil, now)
	replyB := insertThreadContractMessage(t, s, "tasks", "agent", "B replies to A", &rootA, now+1)
	if _, err := s.db.ExecContext(ctx, "UPDATE messages SET message_type='reply' WHERE project_ident='tasks' AND id=?", replyB); err != nil {
		t.Fatal(err)
	}
	replyC := insertThreadContractMessage(t, s, "tasks", "agent", "C replies to B", &replyB, now+2)
	if _, err := s.db.ExecContext(ctx, "UPDATE messages SET message_type='reply' WHERE project_ident='tasks' AND id=?", replyC); err != nil {
		t.Fatal(err)
	}

	thread, err := s.GetProjectMessageThread(ctx, "tasks", replyC)
	if err != nil || thread.Root.ID != replyB || thread.Root.ParentMessageID == nil || *thread.Root.ParentMessageID != rootA ||
		len(thread.Replies) != 1 || thread.Replies[0].ID != replyC {
		t.Fatalf("one-hop legacy thread=%+v err=%v", thread, err)
	}

	created, err := s.CreateHumanProjectMessage(ctx, "tasks", HumanProjectMessageInput{
		Content: "human reply addressed to C", ParentMessageID: &replyC,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ParentMessageID == nil || *created.ParentMessageID != replyB || created.MessageType != "reply" {
		t.Fatalf("human reply=%+v, want immediate one-hop parent B (%d)", created, replyB)
	}
	thread, err = s.GetProjectMessageThread(ctx, "tasks", created.ID)
	if err != nil || thread.Root.ID != replyB || thread.Root.ParentMessageID == nil || *thread.Root.ParentMessageID != rootA ||
		len(thread.Replies) != 2 || thread.Replies[0].ID != replyC || thread.Replies[1].ID != created.ID {
		t.Fatalf("thread after human reply=%+v err=%v", thread, err)
	}
}
