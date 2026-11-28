package worker

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/providers"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestProjectInboxSharedTickKeepsNudgesOutOfTaskWorker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	remote, local, client, host, pool, slot := poolRegistrationFixture(t, ctx, "", false, http.DefaultTransport)
	defer remote.Close()
	defer local.Close()
	defer host.Close()
	defer host.Config.Handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())
	runtime, err := server.NewGatewayFISM(remote, 7, 7)
	if err != nil {
		t.Fatal(err)
	}
	routerCtx, stopRouter := context.WithCancel(ctx)
	routerDone := make(chan error, 1)
	ready := make(chan struct{})
	go func() { routerDone <- runtime.Run(routerCtx, ready) }()
	defer func() {
		stopRouter()
		select {
		case err := <-routerDone:
			if err != nil {
				t.Errorf("inbox router shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("inbox router did not stop")
		}
	}()
	opened := make(chan storage.WorkerRun, 1)
	sessions := make(chan *poolFixtureSession, 1)
	release := make(chan struct{})
	open := func(openCtx context.Context, run storage.WorkerRun, sink conversation.Sink) (providers.Session, error) {
		if err := sink(openCtx, conversation.Event{Kind: "session_ready", SessionID: "pool-native"}); err != nil {
			return nil, err
		}
		session := &poolFixtureSession{done: make(chan struct{}), sink: sink, started: make(chan string, 4), release: release}
		opened <- run
		sessions <- session
		return session, nil
	}
	poolCtx, stopPool := context.WithCancel(ctx)
	poolDone := make(chan error, 1)
	go func() {
		poolDone <- RunPool(poolCtx, PoolConfig{Store: local, Client: client, PoolID: pool.ID, Open: open, PollInterval: 10 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond})
	}()
	defer func() {
		stopPool()
		select {
		case err := <-poolDone:
			if err != nil {
				t.Errorf("inbox pool shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("inbox pool did not stop")
		}
	}()
	for {
		agents, err := remote.Presence(ctx, pool.Project)
		if err != nil {
			t.Fatal(err)
		}
		if len(agents) == 1 && agents[0].Origin == slot.Owner && agents[0].AutoAssign {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("pool did not advertise")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := remote.ReconcilePresence(ctx, time.Now().Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.InsertTask(ctx, storage.TaskInsert{Project: pool.Project, Title: "Inbox worker context", Reporter: "fixture"}, nil); err != nil {
		t.Fatal(err)
	}
	tick := func() fism.Event {
		t.Helper()
		result, err := remote.AcceptObservation(ctx, runtime.Registry(), fism.EmitRequest{ID: "clock.tick", Version: 1, Source: "clock", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		return result.Event
	}
	tick()
	var run storage.WorkerRun
	select {
	case run = <-opened:
	case <-ctx.Done():
		t.Fatal("worker did not adopt task")
	}
	var session *poolFixtureSession
	select {
	case session = <-sessions:
	case <-ctx.Done():
		t.Fatal("worker session did not open")
	}
	select {
	case <-session.started:
	case <-ctx.Done():
		t.Fatal("initial prompt did not start")
	}
	for {
		agents, err := remote.Presence(ctx, pool.Project)
		if err != nil {
			t.Fatal(err)
		}
		current := false
		for _, agent := range agents {
			if agent.Origin == run.Owner && agent.State != "gone" {
				current = true
			}
		}
		if current {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("resumed worker did not advertise its exact native identity")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := remote.InsertProject(ctx, storage.Project{Ident: "inbox-sender", ChannelName: "none", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	delegated, err := remote.DelegateTask(ctx, storage.TaskDelegationInput{SourceProject: "inbox-sender", TargetProject: pool.Project, Title: "FIFO nudge fixture", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pendingTick := tick()
	for {
		pending, err := remote.PendingObservations(ctx, 200)
		if err != nil {
			t.Fatal(err)
		}
		complete := true
		for _, observation := range pending {
			if observation.Event.UID == pendingTick.UID {
				complete = false
				break
			}
		}

		if complete {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("project inbox tick did not complete")
		case <-time.After(10 * time.Millisecond):
		}
	}
	commands, err := remote.ConversationCommands(ctx, pool.Project, run.ConversationID, 0, 100)
	if err != nil || len(commands) != 1 || session.promptCount.Load() != 1 {
		t.Fatalf("task worker should receive only the assigned prompt: commands=%+v count=%d err=%v", commands, session.promptCount.Load(), err)
	}
	unread, err := remote.GetUnconfirmedProjectMessages(ctx, pool.Project, run.Owner.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range unread {
		if message.ID == delegated.MessageID {
			close(release)
			return
		}
	}
	t.Fatal("delegated task message did not remain in the project inbox")
}
