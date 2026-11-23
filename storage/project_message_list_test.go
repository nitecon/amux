package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestListProjectMessageThreadsMatchesRustStatesKindsCountsAndPages(t *testing.T) {
	ctx := context.Background()
	s := projectMessagesTestStore(t, "proj", "other")
	openID := insertThreadListMessage(t, s, "proj", "user", strings.Repeat("a", 199)+"✨tail", 100, nil, stringPtr("reader"), nil)
	ackID := insertThreadListMessage(t, s, "proj", "user", "acknowledged", 200, nil, nil, nil)
	answeredID := insertThreadListMessage(t, s, "proj", "user", "answered", 400, nil, nil, nil)
	replyID := insertThreadListMessage(t, s, "proj", "agent", "reply", 301, nil, nil, &answeredID)
	alertID := insertThreadListMessage(t, s, "proj", "user", "alert", 500, stringPtr("webhook"), nil, nil)
	resolvedID := insertThreadListMessage(t, s, "proj", "user", "resolved", 600, nil, nil, nil)
	agentID := insertThreadListMessage(t, s, "proj", "agent", "agent", 700, nil, nil, nil)
	systemID := insertThreadListMessage(t, s, "proj", "system", "system", 800, nil, nil, nil)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO agent_confirmations(agent_id,project_ident,message_id,confirmed_at)
		VALUES('reader','proj',?,250)`, ackID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE messages SET resolved_at=900,resolved_by='reviewer' WHERE id=?`, resolvedID); err != nil {
		t.Fatal(err)
	}
	// A corrupt foreign-project child must not change this project's state or counts.
	insertThreadListMessage(t, s, "other", "agent", "foreign reply", 950, nil, nil, &openID)

	all, err := s.ListProjectMessageThreads(ctx, " PROJ ", ProjectMessageThreadFilter{State: "all", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []int64{systemID, agentID, resolvedID, alertID, answeredID, ackID, openID}
	if len(all.Threads) != len(wantOrder) {
		t.Fatalf("all threads=%+v, want %d roots", all.Threads, len(wantOrder))
	}
	for i, id := range wantOrder {
		if all.Threads[i].ID != id {
			t.Fatalf("thread order IDs=%v, want %v", threadSummaryIDs(all.Threads), wantOrder)
		}
	}
	if all.Threads[4].State != "answered" || all.Threads[4].LastActivityAt != 301 || all.Threads[4].ReplyCount != 1 || all.Threads[4].Preview != "answered" {
		t.Fatalf("answered summary=%+v", all.Threads[4])
	}
	if all.Threads[5].State != "acknowledged" || all.Threads[5].AckCount != 1 {
		t.Fatalf("acknowledged summary=%+v", all.Threads[5])
	}
	if all.Threads[6].State != "open" || all.Threads[6].ReplyCount != 0 {
		t.Fatalf("foreign reply changed project-scoped root: %+v", all.Threads[6])
	}
	if all.Threads[0].AuthorKind != "system" || all.Threads[1].AuthorKind != "agent" || all.Threads[3].AuthorKind != "webhook" {
		t.Fatalf("effective author kinds: %+v", all.Threads)
	}
	if all.Threads[6].Preview != strings.Repeat("a", 199)+"✨" || len([]rune(all.Threads[6].Preview)) != 200 {
		t.Fatalf("SQLite character preview length=%d", len([]rune(all.Threads[6].Preview)))
	}
	wantCounts := ProjectMessageInboxCounts{HumanOpen: 3, AgentOpen: 1, AlertsOpen: 1, SystemOpen: 1, Resolved: 1}
	if all.Counts != wantCounts {
		t.Fatalf("counts=%+v, want %+v", all.Counts, wantCounts)
	}

	page, err := s.ListProjectMessageThreads(ctx, "proj", ProjectMessageThreadFilter{State: "all", Limit: 2, Offset: 1})
	if err != nil || len(page.Threads) != 2 || page.Threads[0].ID != agentID || page.Threads[1].ID != resolvedID {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if page.Counts != wantCounts {
		t.Fatalf("page-filtered counts=%+v, want project-wide %+v", page.Counts, wantCounts)
	}
	wrappedLimit, err := s.ListProjectMessageThreads(ctx, "proj", ProjectMessageThreadFilter{State: "all", Limit: ^uint64(0)})
	if err != nil || len(wrappedLimit.Threads) != len(wantOrder) {
		t.Fatalf("source-wrapped limit threads=%d err=%v", len(wrappedLimit.Threads), err)
	}
	wrappedOffset, err := s.ListProjectMessageThreads(ctx, "proj", ProjectMessageThreadFilter{State: "all", Limit: 100, Offset: ^uint64(0)})
	if err != nil || len(wrappedOffset.Threads) != len(wantOrder) || wrappedOffset.Threads[0].ID != systemID {
		t.Fatalf("source-wrapped offset=%+v err=%v", wrappedOffset, err)
	}
	if defaultLimit, err := s.ListProjectMessageThreads(ctx, "proj", ProjectMessageThreadFilter{State: "all"}); err != nil || len(defaultLimit.Threads) != len(wantOrder) {
		t.Fatalf("zero-limit source default threads=%d err=%v", len(defaultLimit.Threads), err)
	}

	unresolved, err := s.ListProjectMessageThreads(ctx, "proj", ProjectMessageThreadFilter{Kinds: []string{"human"}, Limit: 100})
	if err != nil || len(unresolved.Threads) != 3 || unresolved.Threads[0].ID != answeredID {
		t.Fatalf("unresolved human filter=%+v err=%v", unresolved, err)
	}
	allHumans, err := s.ListProjectMessageThreads(ctx, "proj", ProjectMessageThreadFilter{State: "all", Kinds: []string{"hum'an"}, Limit: 100})
	if err != nil || len(allHumans.Threads) != 4 {
		t.Fatalf("apostrophe-compatible author kind filter=%+v err=%v", allHumans, err)
	}
	byAgent, err := s.ListProjectMessageThreads(ctx, "proj", ProjectMessageThreadFilter{State: "all", AgentID: stringPtr("reader"), Limit: 100})
	if err != nil || len(byAgent.Threads) != 1 || byAgent.Threads[0].ID != openID {
		t.Fatalf("agent filter=%+v err=%v", byAgent, err)
	}
	answered, err := s.ListProjectMessageThreads(ctx, "proj", ProjectMessageThreadFilter{State: "answered", Limit: 100})
	if err != nil || len(answered.Threads) != 1 || answered.Threads[0].ID != answeredID {
		t.Fatalf("answered state filter=%+v err=%v", answered, err)
	}
	unknownState, err := s.ListProjectMessageThreads(ctx, "proj", ProjectMessageThreadFilter{State: "typo", Limit: 100})
	if err != nil || len(unknownState.Threads) != 6 {
		t.Fatalf("unknown state did not retain source unresolved default: threads=%d err=%v", len(unknownState.Threads), err)
	}
	emptyKind, err := s.ListProjectMessageThreads(ctx, "proj", ProjectMessageThreadFilter{State: "all", Kinds: []string{"missing"}, Limit: 100})
	if err != nil || emptyKind.Threads == nil || len(emptyKind.Threads) != 0 || emptyKind.Counts != wantCounts {
		t.Fatalf("empty filtered response=%+v err=%v", emptyKind, err)
	}
	if _, err := s.ListProjectMessageThreads(ctx, "missing", ProjectMessageThreadFilter{}); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing project error=%v, want ErrTaskNotFound", err)
	}
	for _, thread := range all.Threads {
		if thread.ID == replyID {
			t.Fatalf("reply %d appeared as a root thread", replyID)
		}
	}
	thread, err := s.GetProjectMessageThread(ctx, "proj", answeredID)
	if err != nil || thread.Root.ID != answeredID || len(thread.Replies) != 1 || thread.State != "answered" {
		t.Fatalf("answered thread view=%+v err=%v", thread, err)
	}
	resolved, err := s.ResolveProjectMessagesBefore(ctx, "proj", 501, []string{"bot", "webhook"}, "user")
	if err != nil || resolved != 1 {
		t.Fatalf("bulk alert resolution count=%d err=%v, want 1", resolved, err)
	}
	afterResolve, err := s.ListProjectMessageThreads(ctx, "proj", ProjectMessageThreadFilter{State: "all", Limit: 100})
	if err != nil || afterResolve.Counts.AlertsOpen != 0 {
		t.Fatalf("alerts after source bulk resolution=%+v err=%v", afterResolve, err)
	}
}

func TestResolveProjectMessagesBeforeUsesStrictCutoffRootScopeAndKinds(t *testing.T) {
	ctx := context.Background()
	s := projectMessagesTestStore(t, "proj")
	oldAlert := insertThreadListMessage(t, s, "proj", "user", "old alert", 10, stringPtr("webhook"), nil, nil)
	boundaryAlert := insertThreadListMessage(t, s, "proj", "user", "boundary alert", 20, stringPtr("webhook"), nil, nil)
	otherKind := insertThreadListMessage(t, s, "proj", "agent", "old agent root", 9, nil, nil, nil)
	child := insertThreadListMessage(t, s, "proj", "agent", "old alert reply", 8, stringPtr("webhook"), nil, &oldAlert)

	resolved, err := s.ResolveProjectMessagesBefore(ctx, "proj", 20, []string{"bot", "webhook"}, "user")
	if err != nil || resolved != 1 {
		t.Fatalf("strict cutoff bulk resolution count=%d err=%v, want 1", resolved, err)
	}
	assertProjectMessageResolution(t, s, oldAlert, true, "user")
	assertProjectMessageResolution(t, s, boundaryAlert, false, "")
	assertProjectMessageResolution(t, s, otherKind, false, "")
	assertProjectMessageResolution(t, s, child, false, "")

	resolved, err = s.ResolveProjectMessagesBefore(ctx, "proj", 21, nil, "all-kinds")
	if err != nil || resolved != 2 {
		t.Fatalf("empty-kind all-root resolution count=%d err=%v, want boundary+agent roots", resolved, err)
	}
	assertProjectMessageResolution(t, s, boundaryAlert, true, "all-kinds")
	assertProjectMessageResolution(t, s, otherKind, true, "all-kinds")
	assertProjectMessageResolution(t, s, child, false, "")
}

func insertThreadListMessage(t *testing.T, s *Store, project, source, content string, sentAt int64, authorKind, agentID *string, parentID *int64) int64 {
	t.Helper()
	var parent, kind, agent any
	if parentID != nil {
		parent = *parentID
	}
	if authorKind != nil {
		kind = *authorKind
	}
	if agentID != nil {
		agent = *agentID
	}
	result, err := s.db.ExecContext(context.Background(), `INSERT INTO messages(
		project_ident,source,content,sent_at,parent_message_id,author_kind,agent_id,message_type)
		VALUES(?,?,?,?,?,?,?,'message')`, project, source, content, sentAt, parent, kind, agent)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func threadSummaryIDs(threads []ProjectMessageThreadSummary) []int64 {
	ids := make([]int64, len(threads))
	for i, thread := range threads {
		ids[i] = thread.ID
	}
	return ids
}

func assertProjectMessageResolution(t *testing.T, s *Store, id int64, wantResolved bool, wantBy string) {
	t.Helper()
	var resolvedAt sql.NullInt64
	var resolvedBy sql.NullString
	if err := s.db.QueryRowContext(context.Background(), "SELECT resolved_at,resolved_by FROM messages WHERE id=?", id).Scan(&resolvedAt, &resolvedBy); err != nil {
		t.Fatal(err)
	}
	if resolvedAt.Valid != wantResolved || resolvedBy.Valid != wantResolved || resolvedBy.String != wantBy {
		t.Fatalf("message %d resolution=(%v,%q), want resolved=%t by %q", id, resolvedAt, resolvedBy.String, wantResolved, wantBy)
	}
}
