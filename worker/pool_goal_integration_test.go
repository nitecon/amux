package worker

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/providers"
	"github.com/nitecon/amux/storage"
)

func TestRunPoolCarriesSelectedGoalThroughAllocationAndSavedNativeRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	remote, local, client, host, base, _ := poolRegistrationFixture(t, ctx, "", false, http.DefaultTransport)
	defer remote.Close()
	defer local.Close()
	defer host.Close()
	goal, other := "release", "other"
	config := *base
	config.ID, config.Goal = uuid.NewString(), &goal
	pool, err := local.CreateWorkerPool(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := remote.InsertTask(ctx, storage.TaskInsert{Project: pool.Project, Title: "Earlier different queue", Reporter: "fixture", Goal: &other}, nil)
	if err != nil {
		t.Fatal(err)
	}
	right, err := remote.InsertTask(ctx, storage.TaskInsert{Project: pool.Project, Title: "Selected queue", Reporter: "fixture", Goal: &goal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	opened := make(chan storage.WorkerRun, 2)
	sessions := make(chan *poolFixtureSession, 2)
	release := make(chan struct{})
	open := func(openCtx context.Context, run storage.WorkerRun, sink conversation.Sink) (providers.Session, error) {
		opened <- run
		if err := sink(openCtx, conversation.Event{Kind: "session_ready", SessionID: "selected-goal-native"}); err != nil {
			return nil, err
		}
		session := &poolFixtureSession{done: make(chan struct{}), sink: sink, started: make(chan string, 1), release: release}
		sessions <- session
		return session, nil
	}
	poolCtx, stopPool := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- RunPool(poolCtx, PoolConfig{Store: local, Client: client, PoolID: pool.ID, Open: open, PollInterval: 10 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond})
	}()
	defer func() {
		stopPool()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("goal pool did not stop")
		}
	}()
	for {
		agents, err := remote.Presence(ctx, pool.Project)
		if err != nil {
			t.Fatal(err)
		}
		if len(agents) == 1 && agents[0].Goal != nil && *agents[0].Goal == goal && agents[0].AutoAssign && !agents[0].Busy {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("pool stopped before advertising scope: %v", err)
		case <-ctx.Done():
			t.Fatal("selected goal was not advertised")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := remote.ReconcileAssignments(ctx, time.Now().Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	var run storage.WorkerRun
	select {
	case run = <-opened:
	case <-ctx.Done():
		t.Fatal("selected queue did not start a native run")
	}
	if run.Goal == nil || *run.Goal != goal {
		t.Fatalf("provider lost selected queue: %+v", run)
	}
	var session *poolFixtureSession
	select {
	case session = <-sessions:
	case <-ctx.Done():
		t.Fatal("native session was not created")
	}
	select {
	case <-session.started:
	case <-ctx.Done():
		t.Fatal("initial prompt was not executed")
	}
	saved, err := local.WorkerRun(ctx, run.ID)
	if err != nil || saved.Goal == nil || *saved.Goal != goal {
		t.Fatalf("saved recovery scope: %+v %v", saved, err)
	}
	selected, err := remote.Task(ctx, pool.Project, right.ID)
	if err != nil || selected.Task.OwnerOrigin == nil || selected.Task.Status != "in_progress" {
		t.Fatalf("selected allocation: %+v %v", selected, err)
	}
	skipped, err := remote.Task(ctx, pool.Project, wrong.ID)
	if err != nil || skipped.Task.OwnerOrigin != nil || skipped.Task.Status != "todo" {
		t.Fatalf("other queue was claimed: %+v %v", skipped, err)
	}
	commands, err := remote.ConversationCommands(ctx, pool.Project, run.ConversationID, 0, 100)
	if err != nil || len(commands) != 1 || commands[0].Command.Kind != "prompt" || session.promptCount.Load() != 1 {
		t.Fatalf("assignment duplicated initial intent: %+v %v", commands, err)
	}
	stopPool()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("selected pool failed to stop before resume")
	}
	poolCtx, stopPool = context.WithCancel(ctx)
	defer stopPool()
	done = make(chan error, 1)
	go func() {
		defer close(done)
		done <- RunPool(poolCtx, PoolConfig{Store: local, Client: client, PoolID: pool.ID, Open: open, PollInterval: 10 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond})
	}()
	select {
	case resumed := <-opened:
		if resumed.ID != run.ID || resumed.Goal == nil || *resumed.Goal != goal || resumed.NativeSessionID != "selected-goal-native" {
			t.Fatalf("native reconnect lost durable goal/session: %+v", resumed)
		}
	case err := <-done:
		t.Fatalf("pool stopped before native resume: %v", err)
	case <-ctx.Done():
		t.Fatal("selected pool failed to resume its saved run")
	}
}
