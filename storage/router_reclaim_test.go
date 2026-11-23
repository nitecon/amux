package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/router"
)

func reclaimClock(at int64) router.Event {
	return router.Event{UID: uuid.NewString(), ID: "clock.tick", Version: 1, EmittedAt: at}
}

func assertReclaimValue(t *testing.T, s *Store, query string, want any, args ...any) {
	t.Helper()
	var got any
	if err := s.db.QueryRowContext(context.Background(), query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("query %q = %#v, want %#v", query, got, want)
	}
}

func reclaimTask(t *testing.T, s *Store, status string, updatedAt int64, ownerAgent, ownerJSON *string) *Task {
	t.Helper()
	task := makeTask(t, s, nil)
	if _, err := s.db.ExecContext(context.Background(), `UPDATE tasks SET status=?,updated_at=?,owner_agent_id=?,owner_origin_json=?,started_at=? WHERE id=?`, status, updatedAt, ownerAgent, ownerJSON, updatedAt-1, task.ID); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestReconcileRouterReclaimLegacyEligibilityAndBusySessions(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	now := int64(1_800_600_000_000)
	cutoff := now - reclaimAfterMS
	busyID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	partialBusy, partialIdle := `{"session_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`, `{"session_id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc"}`
	busyTask := reclaimTask(t, s, "in_progress", cutoff, &busyID, nil)
	partialBusyTask := reclaimTask(t, s, "todo", cutoff, nil, &partialBusy)
	partialIdleTask := reclaimTask(t, s, "todo", cutoff, nil, &partialIdle)
	unownedTask := reclaimTask(t, s, "in_progress", cutoff, nil, nil)
	freshTask := reclaimTask(t, s, "in_progress", cutoff+1, nil, nil)
	unclaimedTask := reclaimTask(t, s, "todo", cutoff, nil, nil)

	// Legacy busy-session claims intentionally protect across projects.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('other','discord','',0)`); err != nil {
		t.Fatal(err)
	}
	busyOrigin := assignmentOrigin("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "codex", "linux")
	busyOrigin.SessionID = busyID
	busyAgent := idleAssignmentAgent("other", busyOrigin, time.UnixMilli(now))
	busyAgent.Busy = true
	reportAssignmentPresence(t, s, time.UnixMilli(now), "reclaim-presence", 1, busyAgent)

	got, err := s.ReconcileRouterReclaim(ctx, reclaimClock(now))
	if err != nil || got != 3 {
		t.Fatalf("reclaimed=%d err=%v want 3", got, err)
	}
	for task, want := range map[*Task]string{
		busyTask: "todo", partialBusyTask: "todo", partialIdleTask: "todo",
		unownedTask: "todo", freshTask: "in_progress", unclaimedTask: "todo",
	} {
		detail, err := s.Task(ctx, "tasks", task.ID)
		if err != nil || detail.Task.Status != want {
			t.Fatalf("task %s status=%v err=%v want %s", task.ID, detail, err, want)
		}
	}
	assertReclaimValue(t, s, "SELECT COUNT(*) FROM task_comments WHERE task_id IN (?,?,?)", int64(3), busyTask.ID, partialIdleTask.ID, unownedTask.ID)
}

func TestReconcileRouterReclaimRetainsNativeAndMalformedOwnership(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	now := int64(1_800_610_000_000)
	cutoff := now - reclaimAfterMS
	nativeOwner := `{"session_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","instance_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","provider":"codex","os":"linux"}`
	ownerTask := reclaimTask(t, s, "in_progress", cutoff, nil, &nativeOwner)
	busyOrigin := assignmentOrigin("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "codex", "linux")
	busyOwnerJSON, _ := json.Marshal(busyOrigin)
	busyOwner := string(busyOwnerJSON)
	busyTask := reclaimTask(t, s, "in_progress", cutoff, nil, &busyOwner)
	goneOrigin := assignmentOrigin("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", "ffffffff-ffff-4fff-8fff-ffffffffffff", "codex", "linux")
	goneOwnerJSON, _ := json.Marshal(goneOrigin)
	goneOwner := string(goneOwnerJSON)
	goneTask := reclaimTask(t, s, "in_progress", cutoff, nil, &goneOwner)
	partial := `{"session_id":"legacy-conversation"}`
	conversationTask := reclaimTask(t, s, "in_progress", cutoff, nil, &partial)
	malformed := `{"session_id":42}`
	malformedTask := reclaimTask(t, s, "in_progress", cutoff, nil, &malformed)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO amux_conversations(id,project_ident,task_id,provider,native_session_id,owner_json,created_at) VALUES('native-conversation','tasks',?,'codex','native-session','{}',?)`, conversationTask.ID, now-reclaimAfterMS*2); err != nil {
		t.Fatal(err)
	}
	busyAgent := idleAssignmentAgent("tasks", busyOrigin, time.UnixMilli(now))
	busyAgent.Busy = true
	goneAgent := idleAssignmentAgent("tasks", goneOrigin, time.UnixMilli(now))
	goneAgent.Busy = true
	reportAssignmentPresence(t, s, time.UnixMilli(now), "native-owner-presence", 1, busyAgent, goneAgent)
	if _, err := s.db.ExecContext(ctx, `UPDATE amux_presence_agents SET state='gone' WHERE session_id=?`, goneOrigin.SessionID); err != nil {
		t.Fatal(err)
	}

	// Valid native ownership survives when presence is absent or busy. The
	// conversation evidence also retains a task with only legacy source JSON.
	if _, err := s.ReconcileRouterReclaim(ctx, reclaimClock(now)); err != nil {
		t.Fatal(err)
	}
	for _, task := range []*Task{ownerTask, busyTask, goneTask, conversationTask, malformedTask} {
		assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", "in_progress", task.ID)
	}
	assertReclaimValue(t, s, "SELECT COUNT(*) FROM task_comments WHERE task_id IN (?,?,?,?,?)", int64(0), ownerTask.ID, busyTask.ID, goneTask.ID, conversationTask.ID, malformedTask.ID)
}

func TestReconcileRouterReclaimDispatchReplayAndCausalTimestamp(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	now := int64(1_800_620_000_000)
	cutoff := now - reclaimAfterMS
	task := reclaimTask(t, s, "in_progress", cutoff, nil, nil)
	dispatched := reclaimTask(t, s, "in_progress", cutoff, nil, nil)
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE task_dispatches(task_id TEXT PRIMARY KEY); INSERT INTO task_dispatches VALUES(?)`, dispatched.ID); err != nil {
		t.Fatal(err)
	}
	first, err := s.ReconcileRouterReclaim(ctx, reclaimClock(now))
	if err != nil || first != 1 {
		t.Fatalf("first reclaim=%d err=%v", first, err)
	}
	if second, err := s.ReconcileRouterReclaim(ctx, reclaimClock(now)); err != nil || second != 0 {
		t.Fatalf("replay reclaim=%d err=%v", second, err)
	}
	assertReclaimValue(t, s, "SELECT updated_at FROM tasks WHERE id=?", now, task.ID)
	assertReclaimValue(t, s, "SELECT started_at FROM tasks WHERE id=?", nil, task.ID)
	assertReclaimValue(t, s, "SELECT author FROM task_comments WHERE task_id=?", "system", task.ID)
	assertReclaimValue(t, s, "SELECT author_type FROM task_comments WHERE task_id=?", "system", task.ID)
	assertReclaimValue(t, s, "SELECT content FROM task_comments WHERE task_id=?", reclaimComment, task.ID)
	assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", "in_progress", dispatched.ID)
}

func TestReconcileRouterReclaimRollsBackBatchOnCommentFailure(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	now := int64(1_800_630_000_000)
	first := reclaimTask(t, s, "in_progress", now-reclaimAfterMS, nil, nil)
	second := reclaimTask(t, s, "in_progress", now-reclaimAfterMS, nil, nil)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER fail_reclaim_comment BEFORE INSERT ON task_comments WHEN NEW.task_id='%s' BEGIN SELECT RAISE(ABORT,'injected reclaim comment failure'); END`, second.ID)); err != nil {
		t.Fatal(err)
	}
	if count, err := s.ReconcileRouterReclaim(ctx, reclaimClock(now)); err == nil || !strings.Contains(err.Error(), "injected reclaim comment failure") || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	for _, task := range []*Task{first, second} {
		assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", "in_progress", task.ID)
		assertReclaimValue(t, s, "SELECT COUNT(*) FROM task_comments WHERE task_id=?", int64(0), task.ID)
	}
}

func TestReconcileRouterReclaimDisabledIsBenign(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	task := reclaimTask(t, s, "in_progress", 0, nil, nil)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('plugin.reclaim.enabled','false')"); err != nil {
		t.Fatal(err)
	}
	count, err := s.ReconcileRouterReclaim(ctx, reclaimClock(reclaimAfterMS))
	if err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	assertReclaimValue(t, s, "SELECT status FROM tasks WHERE id=?", "in_progress", task.ID)
}

func TestReconcileRouterReclaimEventGuards(t *testing.T) {
	s := taskStore(t)
	for _, event := range []router.Event{{ID: "clock.tick", Version: 2, UID: uuid.NewString()}, {ID: "clock.tick", Version: 1, UID: "bad"}, {ID: "other", Version: 1, UID: uuid.NewString()}} {
		if _, err := s.ReconcileRouterReclaim(context.Background(), event); err == nil {
			t.Fatalf("accepted invalid event %+v", event)
		}
	}
}

func TestReclaimOwnerSessionFailsClosedOnMalformedJSONValues(t *testing.T) {
	for _, raw := range []string{`{"session_id":`, `null`, `{"session_id":null}`, `{"session_id":42}`} {
		if session, protected := reclaimOwnerSession(raw); !protected || session != "" {
			t.Fatalf("raw owner %q classified as legacy session %q, protected=%v", raw, session, protected)
		}
	}
	if session, protected := reclaimOwnerSession(`{"session_id":"legacy-session"}`); protected || session != "legacy-session" {
		t.Fatalf("partial legacy source = session %q protected=%v", session, protected)
	}
}
