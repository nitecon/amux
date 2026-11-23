package storage

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestGetProjectMessageThreadOrdersRepliesAndConfirmationsAndDerivesState(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	if err := s.InsertProject(ctx, fixtureProject("other")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	rootID := insertThreadContractMessage(t, s, "tasks", "user", "question", nil, now)
	replyOne := insertThreadContractMessage(t, s, "tasks", "agent", "first reply", &rootID, now+1)
	replyTwo := insertThreadContractMessage(t, s, "tasks", "agent", "second reply", &rootID, now+2)
	if confirmed, err := s.ConfirmProjectMessage(ctx, "tasks", "agent-a", rootID); err != nil || !confirmed {
		t.Fatalf("first confirmation=%t err=%v", confirmed, err)
	}
	if confirmed, err := s.ConfirmProjectMessage(ctx, "tasks", "agent-b", rootID); err != nil || !confirmed {
		t.Fatalf("second confirmation=%t err=%v", confirmed, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE agent_confirmations SET confirmed_at=CASE agent_id WHEN 'agent-a' THEN 30 ELSE 20 END
		WHERE project_ident='tasks' AND message_id=?`, rootID); err != nil {
		t.Fatal(err)
	}
	thread, err := s.GetProjectMessageThread(ctx, "TASKS", replyTwo)
	if err != nil {
		t.Fatal(err)
	}
	if thread.Root.ID != rootID || thread.State != "answered" {
		t.Fatalf("reply lookup root=%+v state=%q", thread.Root, thread.State)
	}
	if got := []int64{thread.Replies[0].ID, thread.Replies[1].ID}; !reflect.DeepEqual(got, []int64{replyOne, replyTwo}) {
		t.Fatalf("reply order=%v, want IDs [%d %d]", got, replyOne, replyTwo)
	}
	if !reflect.DeepEqual(thread.Confirmations, []ProjectMessageConfirmation{{AgentID: "agent-b", ConfirmedAt: 20}, {AgentID: "agent-a", ConfirmedAt: 30}}) {
		t.Fatalf("confirmation order=%+v", thread.Confirmations)
	}
	wire, err := json.Marshal(thread)
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]json.RawMessage
	if err := json.Unmarshal(wire, &encoded); err != nil {
		t.Fatal(err)
	}
	var tuples [][]json.RawMessage
	if err := json.Unmarshal(encoded["confirmations"], &tuples); err != nil || len(tuples) != 2 || len(tuples[0]) != 2 {
		t.Fatalf("confirmation wire value=%s err=%v, want tuple arrays", encoded["confirmations"], err)
	}
	var firstAgent string
	var firstAt int64
	if err := json.Unmarshal(tuples[0][0], &firstAgent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(tuples[0][1], &firstAt); err != nil || firstAgent != "agent-b" || firstAt != 20 {
		t.Fatalf("first confirmation tuple=[%q %d] err=%v", firstAgent, firstAt, err)
	}

	if _, err := s.db.ExecContext(ctx, "UPDATE messages SET resolved_at=?,resolved_by='reviewer' WHERE id=?", now+3, rootID); err != nil {
		t.Fatal(err)
	}
	thread, err = s.GetProjectMessageThread(ctx, "tasks", rootID)
	if err != nil || thread.State != "resolved" {
		t.Fatalf("resolved state=%+v err=%v", thread, err)
	}

	acknowledgedID := insertThreadContractMessage(t, s, "tasks", "user", "acknowledged", nil, now+4)
	if confirmed, err := s.ConfirmProjectMessage(ctx, "tasks", "agent-a", acknowledgedID); err != nil || !confirmed {
		t.Fatalf("acknowledged confirmation=%t err=%v", confirmed, err)
	}
	thread, err = s.GetProjectMessageThread(ctx, "tasks", acknowledgedID)
	if err != nil || thread.State != "acknowledged" {
		t.Fatalf("acknowledged state=%+v err=%v", thread, err)
	}

	openID := insertThreadContractMessage(t, s, "tasks", "user", "foreign acknowledgement only", nil, now+5)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO agent_confirmations(agent_id,project_ident,message_id,confirmed_at)
		VALUES('foreign-project-agent','other',?,40)`, openID); err != nil {
		t.Fatal(err)
	}
	thread, err = s.GetProjectMessageThread(ctx, "tasks", openID)
	if err != nil || thread.State != "open" || len(thread.Confirmations) != 0 {
		t.Fatalf("cross-project acknowledgement affected thread=%+v err=%v", thread, err)
	}
	if thread.Replies == nil || thread.Confirmations == nil {
		t.Fatalf("empty lists must encode as arrays: %+v", thread)
	}
	var decoded ProjectMessageConfirmation
	if err := json.Unmarshal([]byte(`["agent-c",50]`), &decoded); err != nil || decoded != (ProjectMessageConfirmation{AgentID: "agent-c", ConfirmedAt: 50}) {
		t.Fatalf("decode tuple=%+v err=%v", decoded, err)
	}
	for _, raw := range []string{`["agent"]`, `["agent",1,2]`, `[null,1]`, `["agent",null]`, `["agent",1.5]`} {
		if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
			t.Errorf("accepted invalid confirmation tuple %s", raw)
		}
	}
}

func TestGetProjectMessageThreadUsesOneHopParentAndProjectScopedFallback(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	if err := s.InsertProject(ctx, fixtureProject("other")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	rootID := insertThreadContractMessage(t, s, "tasks", "user", "root", nil, now)
	replyID := insertThreadContractMessage(t, s, "tasks", "agent", "reply", &rootID, now+1)
	thread, err := s.GetProjectMessageThread(ctx, "tasks", replyID)
	if err != nil || thread.Root.ID != rootID || len(thread.Replies) != 1 || thread.Replies[0].ID != replyID || thread.State != "answered" {
		t.Fatalf("one-hop reply lookup=%+v err=%v", thread, err)
	}

	foreignRootID := insertThreadContractMessage(t, s, "other", "user", "other project's root", nil, now+2)
	fallbackID := insertThreadContractMessage(t, s, "tasks", "agent", "reply with foreign parent", &foreignRootID, now+3)
	fallbackChildID := insertThreadContractMessage(t, s, "tasks", "agent", "same-project child", &fallbackID, now+4)
	insertThreadContractMessage(t, s, "other", "agent", "foreign child", &fallbackID, now+5)
	thread, err = s.GetProjectMessageThread(ctx, "tasks", fallbackID)
	if err != nil || thread.Root.ID != fallbackID || thread.Root.ParentMessageID == nil || *thread.Root.ParentMessageID != foreignRootID ||
		len(thread.Replies) != 1 || thread.Replies[0].ID != fallbackChildID || thread.State != "answered" {
		t.Fatalf("missing-parent fallback or project-scoped replies=%+v err=%v", thread, err)
	}
	if thread, err = s.GetProjectMessageThread(ctx, "tasks", foreignRootID); err != ErrProjectMessageNotFound || thread != nil {
		t.Fatalf("foreign root lookup thread=%+v err=%v, want project-scoped not found", thread, err)
	}
	if thread, err = s.GetProjectMessageThread(ctx, "tasks", 9999999); err != ErrProjectMessageNotFound || thread != nil {
		t.Fatalf("missing message lookup thread=%+v err=%v", thread, err)
	}
}

func insertThreadContractMessage(t *testing.T, s *Store, project, source, content string, parentID *int64, sentAt int64) int64 {
	t.Helper()
	var parent any
	if parentID != nil {
		parent = *parentID
	}
	result, err := s.db.ExecContext(context.Background(), `INSERT INTO messages(
		project_ident,source,content,sent_at,parent_message_id,message_type,deliver_to_agents)
		VALUES(?,?,?,?,?,'message',0)`, project, source, content, sentAt, parent)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
