package worker

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
	"github.com/nitecon/amux/storage"
)

func TestProjectInboxSharedTickDeliversNudgeThroughNativeWorkerFIFO(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	remote, local, client, host, pool, slot := poolRegistrationFixture(t, ctx, "", false, http.DefaultTransport)
	defer remote.Close()
	defer local.Close()
	defer host.Close()
	defer host.Config.Handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())
	definitions := append(append(storage.RouterPresenceEventDefinitions(), storage.RouterTaskEventDefinitions()...), plugins.ClockDefinition(), plugins.ClientHeartbeatDefinition())
	definitions = append(definitions, plugins.ActivityEventDefinitions()...)
	definitions = append(definitions, plugins.NotificationEventDefinitions()...)
	runtime, err := router.NewRuntime(remote, remote.EventBus(), definitions, []router.Plugin{
		plugins.PresencePlugin(remote), plugins.AssignmentPlugin(remote), plugins.NativeDeliveryPlugin(remote), plugins.ProjectInboxPlugin(remote),
	}, router.RunConfig{Capacity: 8})
	if err != nil {
		t.Fatal(err)
	}
	routerCtx, stopRouter := context.WithCancel(ctx)
	routerDone := make(chan error, 1)
	go func() { routerDone <- runtime.Run(routerCtx) }()
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
	open := func(openCtx context.Context, run storage.WorkerRun, sink conversation.Sink) (Session, error) {
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
	tick := func() router.Event {
		t.Helper()
		result, err := remote.EmitRouterEvent(ctx, runtime.Registry(), router.EmitRequest{ID: "clock.tick", Version: 1, Source: "clock", Payload: []byte(`{}`)})
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
		unread, err := remote.GetUnconfirmedProjectMessages(ctx, pool.Project, run.Owner.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, message := range unread {
			if message.ID == delegated.MessageID {
				found = true
			}
		}
		if !found {
			break
		}
		// Heartbeats may advance the receiver sequence between the inbox
		// snapshot and its transaction. Model the server's next shared tick
		// after a completed reconciliation leaves the message unread.
		deliveries, err := remote.RouterDeliveries(ctx, pendingTick.Seq)
		if err != nil {
			t.Fatal(err)
		}
		for _, delivery := range deliveries {
			if delivery.Plugin == "project-inbox" && delivery.State == "completed" {
				pendingTick = tick()
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("tick did not durably accept inbox message")
		case <-time.After(10 * time.Millisecond):
		}
	}
	commands, err := remote.ConversationCommands(ctx, pool.Project, run.ConversationID, 0, 100)
	if err != nil || len(commands) != 2 || session.promptCount.Load() != 1 {
		t.Fatalf("inbox did not queue behind initial turn: commands=%+v count=%d err=%v", commands, session.promptCount.Load(), err)
	}
	close(release)
	select {
	case text := <-session.started:
		if !strings.Contains(text, "Delegated task created") || !strings.Contains(text, "inbox-sender") {
			t.Fatalf("provider prompt omitted project nudge context: %s", text)
		}
	case <-ctx.Done():
		t.Fatal("queued inbox message did not reach provider")
	}
	if session.promptCount.Load() != 2 {
		t.Fatalf("provider received %d prompts, want one initial prompt and one inbox prompt", session.promptCount.Load())
	}
	repeated := tick()
	for {
		deliveries, err := remote.RouterDeliveries(ctx, repeated.Seq)
		if err != nil {
			t.Fatal(err)
		}
		done := false
		for _, delivery := range deliveries {
			if delivery.Plugin == "project-inbox" && delivery.State == "completed" {
				done = true
			}
		}
		if done {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("repeated inbox tick did not complete")
		case <-time.After(10 * time.Millisecond):
		}
	}
	commands, err = remote.ConversationCommands(ctx, pool.Project, run.ConversationID, 0, 100)
	if err != nil || len(commands) != 2 {
		t.Fatalf("repeated tick duplicated inbox command: %+v %v", commands, err)
	}
}
