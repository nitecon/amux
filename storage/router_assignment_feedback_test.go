package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/identity/agent"
)

func migrateTestRouterAssignmentFeedback(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateRouterAssignmentFeedback(ctx, tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func addFeedbackOffer(t *testing.T, s *Store, task *Task, actor *agent.Origin, source string) string {
	t.Helper()
	ctx := context.Background()
	uid := uuid.NewString()
	origin, err := json.Marshal(actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO assignment_offers(notify_uid,task_id,project,session_id,reporter_source) VALUES(?,?,?,?,?)`, uid, task.ID, task.ProjectIdent, actor.SessionID, source); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO amux_assignment_offer_claims(notify_uid,origin_json,claim_updated_at) VALUES(?,?,?)`, uid, string(origin), task.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	return uid
}

func TestRouterAssignmentFeedbackMigrationPreservesRustOfferDDL(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	var before string
	if err := s.db.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='assignment_offers'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	migrateTestRouterAssignmentFeedback(t, s)
	var after string
	if err := s.db.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='assignment_offers'").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("Rust offer schema changed:\nbefore=%s\nafter=%s", before, after)
	}
	var fk string
	if err := s.db.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='amux_assignment_offer_claims'").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk == "" {
		t.Fatal("native offer proof table missing")
	}
}

func TestRouterAssignmentFeedbackReleasesOnlyCapturedExactOwner(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	migrateTestAssignmentAdoption(t, s)
	migrateTestRouterAssignmentFeedback(t, s)
	actor := taskActor()
	task := ownedInProgressTask(t, s, actor)
	notifyUID, correlation := uuid.NewString(), uuid.NewString()
	origin, _ := json.Marshal(actor)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO assignment_offers(notify_uid,task_id,project,session_id,reporter_source) VALUES(?,?,?,?,?)`, notifyUID, task.ID, "tasks", actor.SessionID, "agent:captured"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO amux_assignment_offer_claims(notify_uid,origin_json,claim_updated_at) VALUES(?,?,?)`, notifyUID, string(origin), task.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	result := fism.Event{UID: uuid.NewString(), ID: "agent.notify_result", Version: 1, Source: "agent:other", EmittedAt: task.UpdatedAt + 1, Correlation: &notifyUID, Payload: []byte(`{"target":"` + actor.SessionID + `","status":"skipped"}`)}
	if err := s.ReconcileRouterAssignmentFeedback(ctx, result); err != nil {
		t.Fatal(err)
	}
	current, err := readTask(ctx, s.db, "tasks", task.ID)
	if err != nil || current.Status != "in_progress" {
		t.Fatalf("wrong source released task: task=%+v err=%v", current, err)
	}
	var wrongSourceOffers int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM assignment_offers WHERE notify_uid=?", notifyUID).Scan(&wrongSourceOffers); err != nil {
		t.Fatal(err)
	}
	if wrongSourceOffers != 1 {
		t.Fatalf("wrong-source receipt consumed offer: %d", wrongSourceOffers)
	}
	result.Source, result.Correlation = "agent:captured", &correlation
	if err := s.ReconcileRouterAssignmentFeedback(ctx, result); err != nil {
		t.Fatal(err)
	}
	current, err = readTask(ctx, s.db, "tasks", task.ID)
	if err != nil || current.Status != "in_progress" {
		t.Fatalf("uncorrelated receipt released task: task=%+v err=%v", current, err)
	}
	result.Correlation = &notifyUID
	if err := s.ReconcileRouterAssignmentFeedback(ctx, result); err != nil {
		t.Fatal(err)
	}
	current, err = readTask(ctx, s.db, "tasks", task.ID)
	if err != nil || current.Status != "todo" || current.OwnerOrigin != nil {
		t.Fatalf("captured refusal did not release: task=%+v err=%v", current, err)
	}
	var offers int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM assignment_offers WHERE notify_uid=?", notifyUID).Scan(&offers); err != nil || offers != 0 {
		t.Fatalf("offer remains after release: count=%d err=%v", offers, err)
	}
	if err := s.ReconcileRouterAssignmentFeedback(ctx, result); err != nil {
		t.Fatalf("replay: %v", err)
	}
	current, err = readTask(ctx, s.db, "tasks", task.ID)
	if err != nil || current.Status != "todo" {
		t.Fatalf("replay changed task: %+v %v", current, err)
	}
}

func TestRouterAssignmentFeedbackLegacyOfferFailsClosed(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	migrateTestAssignmentAdoption(t, s)
	migrateTestRouterAssignmentFeedback(t, s)
	task := ownedInProgressTask(t, s, taskActor())
	uid := uuid.NewString()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO assignment_offers(notify_uid,task_id,project,session_id,reporter_source) VALUES(?,?,?,?,NULL)`, uid, task.ID, "tasks", taskActor().SessionID); err != nil {
		t.Fatal(err)
	}
	event := fism.Event{UID: uuid.NewString(), ID: "agent.notify_result", Version: 1, Source: "agent:captured", Correlation: &uid, Payload: []byte(`{"target":"` + taskActor().SessionID + `","status":"failed"}`)}
	if err := s.ReconcileRouterAssignmentFeedback(ctx, event); err != nil {
		t.Fatal(err)
	}
	current, err := readTask(ctx, s.db, "tasks", task.ID)
	if err != nil || current.Status != "in_progress" {
		t.Fatalf("legacy row authorized release: task=%+v err=%v", current, err)
	}
	var proof sql.NullString
	if err := s.db.QueryRowContext(ctx, "SELECT reporter_source FROM assignment_offers WHERE notify_uid=?", uid).Scan(&proof); err != nil {
		t.Fatal(err)
	}
}

func TestRouterAssignmentFeedbackRefusalFailureRollsBackForReplay(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	migrateTestAssignmentAdoption(t, s)
	migrateTestRouterAssignmentFeedback(t, s)
	actor := taskActor()
	task := ownedInProgressTask(t, s, actor)
	now := time.Now().UTC().Add(-time.Minute)
	reportAssignmentPresence(t, s, now, "agent:captured", 1, idleAssignmentAgent("tasks", *actor, now))
	uid := uuid.NewString()
	origin, _ := json.Marshal(actor)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO assignment_offers(notify_uid,task_id,project,session_id,reporter_source) VALUES(?,?,?,?,?)`, uid, task.ID, "tasks", actor.SessionID, "agent:captured"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO amux_assignment_offer_claims(notify_uid,origin_json,claim_updated_at) VALUES(?,?,?)`, uid, string(origin), task.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_assignment_refusal BEFORE INSERT ON assignment_refusals BEGIN SELECT RAISE(ABORT,'refusal write rejected'); END`); err != nil {
		t.Fatal(err)
	}
	event := fism.Event{UID: uuid.NewString(), ID: "agent.notify_result", Version: 1, Source: "agent:captured", EmittedAt: now.Add(40 * time.Second).UnixMilli(), Correlation: &uid, Payload: []byte(`{"target":"` + actor.SessionID + `","status":"failed"}`)}
	if err := s.ReconcileRouterAssignmentFeedback(ctx, event); err == nil {
		t.Fatal("refusal write failure was swallowed")
	}
	current, err := readTask(ctx, s.db, "tasks", task.ID)
	if err != nil || current.Status != "in_progress" {
		t.Fatalf("failed transaction released claim: %+v %v", current, err)
	}
	var offers int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM assignment_offers WHERE notify_uid=?", uid).Scan(&offers); err != nil || offers != 1 {
		t.Fatalf("failed transaction consumed offer: %d %v", offers, err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER reject_assignment_refusal"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRouterAssignmentFeedback(ctx, event); err != nil {
		t.Fatalf("retry: %v", err)
	}
	current, err = readTask(ctx, s.db, "tasks", task.ID)
	if err != nil || current.Status != "todo" {
		t.Fatalf("retry did not release: %+v %v", current, err)
	}
}

func TestRouterAssignmentFeedbackAdoptionFencesNativeRelease(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	migrateTestAssignmentAdoption(t, s)
	migrateTestRouterAssignmentFeedback(t, s)
	actor := taskActor()
	task := ownedInProgressTask(t, s, actor)
	uid := uuid.NewString()
	origin, _ := json.Marshal(actor)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO assignment_offers(notify_uid,task_id,project,session_id,reporter_source) VALUES(?,?,?,?,?)`, uid, task.ID, "tasks", actor.SessionID, "agent:captured"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO amux_assignment_offer_claims(notify_uid,origin_json,claim_updated_at) VALUES(?,?,?)`, uid, string(origin), task.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	adopted, err := s.AdoptWorkerAssignment(ctx, "tasks", actor)
	if err != nil || adopted == nil {
		t.Fatalf("adopt: %+v %v", adopted, err)
	}
	event := fism.Event{UID: uuid.NewString(), ID: "agent.notify_result", Version: 1, Source: "agent:captured", EmittedAt: task.UpdatedAt + 1, Correlation: &uid, Payload: []byte(`{"target":"` + actor.SessionID + `","status":"skipped"}`)}
	if err := s.ReconcileRouterAssignmentFeedback(ctx, event); err != nil {
		t.Fatal(err)
	}
	current, err := readTask(ctx, s.db, "tasks", task.ID)
	if err != nil || current.Status != "in_progress" {
		t.Fatalf("adopted task released: %+v %v", current, err)
	}
	var prompts int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_conversation_commands WHERE kind='prompt'").Scan(&prompts); err != nil || prompts != 1 {
		t.Fatalf("initial prompt count=%d err=%v", prompts, err)
	}
}

func TestRouterAssignmentFeedbackStaleCommentCannotReleaseClaim(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	migrateTestAssignmentAdoption(t, s)
	migrateTestRouterAssignmentFeedback(t, s)
	actor := taskActor()
	task := ownedInProgressTask(t, s, actor)
	uid := addFeedbackOffer(t, s, task, actor, "agent:captured")
	if _, err := s.AddTaskComment(ctx, "tasks", task.ID, "someone", "user", "updated after notification", nil, nil); err != nil {
		t.Fatal(err)
	}
	event := fism.Event{UID: uuid.NewString(), ID: "agent.notify_result", Version: 1, Source: "agent:captured", Correlation: &uid, Payload: []byte(`{"target":"` + actor.SessionID + `","status":"skipped"}`)}
	if err := s.ReconcileRouterAssignmentFeedback(ctx, event); err != nil {
		t.Fatal(err)
	}
	current, err := readTask(ctx, s.db, "tasks", task.ID)
	if err != nil || current.Status != "in_progress" {
		t.Fatalf("stale comment receipt released claim: %+v %v", current, err)
	}
}

func TestRouterAssignmentFeedbackSameMillisecondNewClaimRejectsOldOffer(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	migrateTestAssignmentAdoption(t, s)
	migrateTestRouterAssignmentFeedback(t, s)
	actor := taskActor()
	task := ownedInProgressTask(t, s, actor)
	oldUID := addFeedbackOffer(t, s, task, actor, "agent:captured")
	ownerJSON, _ := json.Marshal(actor)
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='todo',owner_agent_id=NULL,owner_origin_json=NULL,updated_at=? WHERE id=?`, task.UpdatedAt, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='in_progress',owner_origin_json=?,updated_at=? WHERE id=?`, string(ownerJSON), task.UpdatedAt, task.ID); err != nil {
		t.Fatal(err)
	}
	current, err := readTask(ctx, s.db, "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	newUID := addFeedbackOffer(t, s, current, actor, "agent:captured")
	event := fism.Event{UID: uuid.NewString(), ID: "agent.notify_result", Version: 1, Source: "agent:captured", Correlation: &oldUID, Payload: []byte(`{"target":"` + actor.SessionID + `","status":"failed"}`)}
	if err := s.ReconcileRouterAssignmentFeedback(ctx, event); err != nil {
		t.Fatal(err)
	}
	current, err = readTask(ctx, s.db, "tasks", task.ID)
	if err != nil || current.Status != "in_progress" {
		t.Fatalf("old offer released same-ms claim: %+v %v", current, err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM assignment_offers WHERE notify_uid=?", newUID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("new offer count=%d err=%v", count, err)
	}
}

func TestRouterAssignmentFeedbackLegacySchemaAddsNullableSource(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE TABLE amux_go_migrations(version INTEGER PRIMARY KEY)`,
		`CREATE TABLE tasks(id TEXT PRIMARY KEY)`,
		`CREATE TABLE task_comments(id TEXT,task_id TEXT)`,
		`CREATE TABLE assignment_offers(notify_uid TEXT PRIMARY KEY,task_id TEXT NOT NULL,project TEXT NOT NULL,session_id TEXT NOT NULL)`,
		`INSERT INTO assignment_offers VALUES('legacy','task','project','session')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateRouterAssignmentFeedback(ctx, tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var source sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT reporter_source FROM assignment_offers WHERE notify_uid='legacy'").Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source.Valid {
		t.Fatalf("legacy source became authorized: %q", source.String)
	}
}

func TestReconcileRouterAssignmentRefusalsUsesExactCurrentOutputOption(t *testing.T) {
	for _, tc := range []struct {
		name        string
		presence    []AgentPresence
		wantBlocked bool
	}{
		{name: "same output", presence: []AgentPresence{{PresenceAgent: PresenceAgent{Origin: agent.Origin{SessionID: "session"}, LastActivityAt: int64Pointer(42)}, SeenAt: 2}}, wantBlocked: true},
		{name: "new output", presence: []AgentPresence{{PresenceAgent: PresenceAgent{Origin: agent.Origin{SessionID: "session"}, LastActivityAt: int64Pointer(43)}, SeenAt: 2}}},
		{name: "identity reset to nil", presence: []AgentPresence{{PresenceAgent: PresenceAgent{Origin: agent.Origin{SessionID: "session"}}, SeenAt: 2}}},
		{name: "departed", presence: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := taskStore(t)
			migrateTestRouterAssignmentFeedback(t, s)
			if _, err := s.db.Exec("INSERT INTO assignment_refusals(session_id,output_at) VALUES('session',42)"); err != nil {
				t.Fatal(err)
			}
			got, err := reconcileRouterAssignmentRefusals(context.Background(), s.db, tc.presence)
			if err != nil {
				t.Fatal(err)
			}
			if got["session"] != tc.wantBlocked {
				t.Fatalf("blocked=%v want %v", got["session"], tc.wantBlocked)
			}
			var count int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM assignment_refusals WHERE session_id='session'").Scan(&count); err != nil {
				t.Fatal(err)
			}
			wantCount := 0
			if tc.wantBlocked {
				wantCount = 1
			}
			if count != wantCount {
				t.Fatalf("saved refusal count=%d want %d", count, wantCount)
			}
		})
	}
}

func TestRouterAssignmentFeedbackAdoptionRaceNeverReleasesAdoptedRun(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	migrateTestAssignmentAdoption(t, s)
	migrateTestRouterAssignmentFeedback(t, s)
	actor := taskActor()
	task := ownedInProgressTask(t, s, actor)
	uid := addFeedbackOffer(t, s, task, actor, "agent:captured")
	event := fism.Event{UID: uuid.NewString(), ID: "agent.notify_result", Version: 1, Source: "agent:captured", EmittedAt: task.UpdatedAt + 1, Correlation: &uid, Payload: []byte(`{"target":"` + actor.SessionID + `","status":"skipped"}`)}
	start := make(chan struct{})
	type adoptionResult struct {
		result *WorkerAssignment
		err    error
	}
	adoptDone := make(chan adoptionResult, 1)
	feedbackDone := make(chan error, 1)
	go func() {
		<-start
		result, err := s.AdoptWorkerAssignment(ctx, "tasks", actor)
		adoptDone <- adoptionResult{result, err}
	}()
	go func() { <-start; feedbackDone <- s.ReconcileRouterAssignmentFeedback(ctx, event) }()
	close(start)
	adopted := <-adoptDone
	if adopted.err != nil {
		t.Fatal(adopted.err)
	}
	if err := <-feedbackDone; err != nil {
		t.Fatal(err)
	}
	current, err := readTask(ctx, s.db, "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var runs, prompts int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_assignment_runs WHERE task_id=?", task.ID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_conversation_commands WHERE kind='prompt'").Scan(&prompts); err != nil {
		t.Fatal(err)
	}
	if adopted.result != nil {
		if current.Status != "in_progress" || runs != 1 || prompts != 1 {
			t.Fatalf("adoption race lost adopted run: task=%+v runs=%d prompts=%d", current, runs, prompts)
		}
	} else if current.Status != "todo" || runs != 0 || prompts != 0 {
		t.Fatalf("feedback winner left partial adoption: task=%+v runs=%d prompts=%d", current, runs, prompts)
	}
}
