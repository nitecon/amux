package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

type poolFixtureSession struct {
	done        chan struct{}
	closeOnce   atomic.Bool
	sink        conversation.Sink
	started     chan string
	release     <-chan struct{}
	promptCount atomic.Int32
}

func (s *poolFixtureSession) Prompt(ctx context.Context, text string) (string, error) {
	s.promptCount.Add(1)
	s.started <- text
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-s.release:
	}
	if err := s.sink(ctx, conversation.Event{Kind: "turn_completed", SessionID: "pool-native", Status: "end_turn"}); err != nil {
		return "", err
	}
	return "end_turn", nil
}

func (s *poolFixtureSession) ResolvePermission(context.Context, string, string) error { return nil }
func (s *poolFixtureSession) Done() <-chan struct{}                                   { return s.done }
func (s *poolFixtureSession) Err() error                                              { return nil }
func (s *poolFixtureSession) Close() error {
	if s.closeOnce.CompareAndSwap(false, true) {
		close(s.done)
	}
	return nil
}

type poolOfflineTransport struct {
	base    http.RoundTripper
	offline atomic.Bool
}

func (t *poolOfflineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.offline.Load() {
		return nil, errors.New("temporary gateway outage")
	}
	return t.base.RoundTrip(r)
}

type loseFirstPresenceAck struct {
	base     http.RoundTripper
	calls    atomic.Int32
	observed chan storage.PresenceReport
}

func (t *loseFirstPresenceAck) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.observed != nil && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/worker-presence") && r.GetBody != nil {
		body, err := r.GetBody()
		if err != nil {
			return nil, err
		}
		var report storage.PresenceReport
		err = json.NewDecoder(body).Decode(&report)
		body.Close()
		if err != nil {
			return nil, err
		}
		t.observed <- report
	}
	response, err := t.base.RoundTrip(r)
	if err == nil && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/worker-presence") && t.calls.Add(1) == 1 {
		response.Body.Close()
		return nil, errors.New("presence acknowledgement lost")
	}
	return response, err
}

func TestPoolPresenceRetriesOnePersistedSequenceAndUsesCurrentRunOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if err = remote.InsertProject(ctx, storage.Project{Ident: "pool-test", ChannelName: "pool-test", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "pool-test", Title: "Native assignment", Reporter: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(remote, "pool-secret")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	defer handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())
	transport := &loseFirstPresenceAck{base: http.DefaultTransport, observed: make(chan storage.PresenceReport, 4)}
	client, err := gatewayclient.New(host.URL, "pool-secret", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	local, err := storage.Open(ctx, filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	pool, err := local.CreateWorkerPool(ctx, storage.WorkerPool{ID: uuid.NewString(), Server: host.URL, Project: "pool-test", Directory: t.TempDir(), Provider: "codex", OS: osName, SlotCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	slots, err := local.WorkerPoolSlots(ctx, pool.ID)
	if err != nil || len(slots) != 1 {
		t.Fatalf("slots = %+v, err = %v", slots, err)
	}
	conversation, err := remote.CreateConversation(ctx, "pool-test", task.ID, &slots[0].Owner)
	if err != nil {
		t.Fatal(err)
	}
	run := storage.WorkerRun{ID: uuid.NewString(), Server: host.URL, Project: pool.Project, ConversationID: conversation.ID, Directory: pool.Directory, Owner: slots[0].Owner}
	if err = local.AttachWorkerPoolRun(ctx, pool.ID, slots[0].ID, run); err != nil {
		t.Fatal(err)
	}
	prepared, err := local.PrepareWorkerInstance(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = local.ActivateWorkerInstance(ctx, run.ID, prepared.PendingInstanceID); err != nil {
		t.Fatal(err)
	}
	clientRun := uuid.NewString()
	config := PoolConfig{Store: local, Client: client, PoolID: pool.ID, ClientRun: clientRun}
	if err = poolPresence(ctx, config, *pool, slots[0], true); err != nil {
		t.Fatal(err)
	}
	updatedSlots, err := local.WorkerPoolSlots(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	updatedRun, err := local.WorkerRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedSlots[0].ReportSequence != 1 || updatedSlots[0].Owner != updatedRun.Owner {
		t.Fatalf("report sequence/owner = %d/%+v, want 1/%+v", updatedSlots[0].ReportSequence, updatedSlots[0].Owner, updatedRun.Owner)
	}
	if transport.calls.Load() != 2 {
		t.Fatalf("presence requests = %d, want one lost ack and one retry", transport.calls.Load())
	}
	for i := 0; i < 2; i++ {
		select {
		case report := <-transport.observed:
			if report.ClientRun == nil || *report.ClientRun != clientRun || report.Sequence != 1 {
				t.Fatalf("retry changed lifetime or report identity: %+v", report)
			}
		default:
			t.Fatal("missing captured presence request")
		}
	}
	presence, err := remote.Presence(ctx, pool.Project)
	if err != nil || len(presence) != 1 || presence[0].Origin != updatedRun.Owner || !presence[0].Busy || presence[0].AutoAssign {
		t.Fatalf("reported presence = %+v, err = %v", presence, err)
	}
	if presence[0].Generation == nil || *presence[0].Generation != updatedRun.Owner.InstanceID {
		t.Fatalf("target generation did not follow the current execution: %+v", presence[0])
	}
	omitPoolSlot(config, *pool, slots[0])
	select {
	case report := <-transport.observed:
		if report.ClientRun == nil || *report.ClientRun != clientRun || report.Sequence != 2 || len(report.Agents) != 0 {
			t.Fatalf("shutdown report changed lifetime: %+v", report)
		}
	default:
		t.Fatal("missing shutdown presence request")
	}
}

func TestRunPoolFinishesTaskBeforeReusingSlotAndSurvivesOfflinePrompt(t *testing.T) {
	t.Run("polling", func(t *testing.T) { testRunPoolFollowUps(t, false) })
	t.Run("streamed", func(t *testing.T) { testRunPoolFollowUps(t, true) })
}

func testRunPoolFollowUps(t *testing.T, streamed bool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if err = remote.InsertProject(ctx, storage.Project{Ident: "pool-flow", ChannelName: "pool-flow", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	first, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "pool-flow", Title: "First assigned task", Reporter: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "pool-flow", Title: "Second assigned task", Reporter: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(remote, "pool-flow-secret")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	defer handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())
	transport := &poolOfflineTransport{base: http.DefaultTransport}
	client, err := gatewayclient.New(host.URL, "pool-flow-secret", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	local, err := storage.Open(ctx, filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	pool, err := local.CreateWorkerPool(ctx, storage.WorkerPool{ID: uuid.NewString(), Server: host.URL, Project: "pool-flow", Directory: t.TempDir(), Provider: "codex", OS: osName, SlotCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	slots, err := local.WorkerPoolSlots(ctx, pool.ID)
	if err != nil || len(slots) != 1 {
		t.Fatalf("slots = %+v, err = %v", slots, err)
	}
	firstRelease := make(chan struct{})
	secondRelease := make(chan struct{})
	close(secondRelease)
	sessions := make(chan *poolFixtureSession, 2)
	var opened atomic.Int32
	openedRuns := make(chan storage.WorkerRun, 3)
	open := func(_ context.Context, run storage.WorkerRun, sink conversation.Sink) (Session, error) {
		release := firstRelease
		if opened.Add(1) > 1 {
			release = secondRelease
		}
		session := &poolFixtureSession{done: make(chan struct{}), sink: sink, started: make(chan string, 1), release: release}
		if err := sink(ctx, conversation.Event{Kind: "session_ready", SessionID: "pool-native"}); err != nil {
			return nil, err
		}
		openedRuns <- run
		sessions <- session
		return session, nil
	}
	poolCtx, stopPool := context.WithCancel(ctx)
	if streamed {
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			WatchAssignments(poolCtx, local, client)
		}()
		defer func() {
			stopPool()
			<-watchDone
		}()
	}
	poolDone := make(chan error, 1)
	go func() {
		defer close(poolDone)
		poolDone <- RunPool(poolCtx, PoolConfig{Store: local, Client: client, PoolID: pool.ID, Open: open, StreamAssignments: streamed, PollInterval: 10 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond})
	}()
	defer func() {
		stopPool()
		select {
		case <-poolDone:
		case <-time.After(3 * time.Second):
			t.Error("worker pool did not stop")
		}
	}()
	waitFor := func(label string, condition func() (bool, error)) {
		t.Helper()
		for {
			ok, err := condition()
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("timed out waiting for %s", label)
			case err := <-poolDone:
				t.Fatalf("pool exited waiting for %s: %v", label, err)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	waitFor("initial source presence", func() (bool, error) {
		presence, err := remote.Presence(ctx, pool.Project)
		return len(presence) == 1, err
	})
	if err = remote.ReconcileAssignments(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	var firstConversation string
	waitFor("first provider prompt", func() (bool, error) {
		conversations, err := remote.TaskConversations(ctx, pool.Project, first.ID, "", 10)
		if err == nil && len(conversations) == 1 {
			firstConversation = conversations[0].ID
		}
		return firstConversation != "" && len(sessions) == 1, err
	})
	firstSession := <-sessions
	select {
	case <-firstSession.started:
	case <-ctx.Done():
		t.Fatal("first prompt did not start")
	}
	transport.offline.Store(true)
	select {
	case <-firstSession.done:
		t.Fatal("offline reporting stopped the active provider")
	case <-time.After(150 * time.Millisecond):
	}
	transport.offline.Store(false)
	close(firstRelease)
	waitFor("first provider turn completion", func() (bool, error) {
		events, err := remote.ConversationEvents(ctx, pool.Project, firstConversation, 0, 100)
		for _, event := range events {
			if event.Event.Kind == "turn_completed" {
				task, taskErr := remote.Task(ctx, pool.Project, first.ID)
				return taskErr == nil && task.Task.Status == "in_progress", taskErr
			}
		}
		return false, err
	})
	firstDetail, err := remote.Task(ctx, pool.Project, first.ID)
	if err != nil || firstDetail.Task.Status != "in_progress" {
		t.Fatalf("turn completion changed task status: %+v, err = %v", firstDetail, err)
	}
	review := "Accept the worker's submitted result."
	if _, err = remote.UpdateTaskWorkflow(ctx, pool.Project, first.ID, storage.TaskWorkflowUpdate{
		Status: "user_review", ReviewAction: &review, Changeset: json.RawMessage("null"),
	}, firstDetail.Task.OwnerOrigin); err != nil {
		t.Fatal(err)
	}
	waitFor("first run to flush and release the slot", func() (bool, error) {
		fresh, err := local.WorkerPoolSlots(ctx, pool.ID)
		if err != nil || len(fresh) != 1 || fresh[0].RunID != "" {
			return false, err
		}
		presence, err := remote.Presence(ctx, pool.Project)
		if err != nil {
			return false, err
		}
		for _, agent := range presence {
			if agent.Origin == fresh[0].Owner && agent.State == "idle" && !agent.Busy && agent.AutoAssign {
				return true, nil
			}
		}
		return false, nil
	})
	firstRun := <-openedRuns
	followUpID := uuid.NewString()
	const followUp = "What did you change?"
	if err := client.Enqueue(ctx, pool.Project, firstConversation, followUpID, conversation.Command{Kind: "prompt", Text: followUp}); err != nil {
		t.Fatal(err)
	}
	waitFor("original provider session to reopen", func() (bool, error) {
		return len(sessions) == 1, nil
	})
	resumedSession := <-sessions
	resumedRun := <-openedRuns
	if resumedRun.ID != firstRun.ID || resumedRun.ConversationID != firstConversation ||
		resumedRun.NativeSessionID != "pool-native" || resumedRun.Owner.SessionID != firstRun.Owner.SessionID ||
		resumedRun.Owner.InstanceID == firstRun.Owner.InstanceID {
		t.Fatalf("follow-up opened the wrong provider session: %+v", resumedRun)
	}
	select {
	case text := <-resumedSession.started:
		if text != followUp {
			t.Fatalf("resumed provider received %q, want %q", text, followUp)
		}
	case <-ctx.Done():
		t.Fatal("follow-up did not reach the original provider session")
	}
	waitFor("follow-up result in the original conversation", func() (bool, error) {
		commands, err := remote.ConversationCommands(ctx, pool.Project, firstConversation, 0, 100)
		for _, command := range commands {
			if command.ID == followUpID && command.State == "done" {
				events, err := remote.ConversationEvents(ctx, pool.Project, firstConversation, 0, 100)
				for _, event := range events {
					if event.Event.Kind == "user_message" && event.Event.MessageID == followUpID && event.Event.Text == followUp {
						return true, nil
					}
				}
				return false, err
			}
		}
		return false, err
	})
	firstDetail, err = remote.Task(ctx, pool.Project, first.ID)
	if err != nil || firstDetail.Task.Status != "user_review" {
		t.Fatalf("follow-up changed task workflow: %+v, err = %v", firstDetail, err)
	}
	waitFor("follow-up slot to return to idle", func() (bool, error) {
		presence, err := remote.Presence(ctx, pool.Project)
		for _, agent := range presence {
			if agent.Origin.SessionID == firstRun.Owner.SessionID && agent.AutoAssign && !agent.Busy {
				return true, nil
			}
		}
		return false, err
	})
	if err = remote.ReconcileAssignments(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitFor("second provider prompt", func() (bool, error) {
		conversations, err := remote.TaskConversations(ctx, pool.Project, second.ID, "", 10)
		return err == nil && len(conversations) == 1 && len(sessions) == 1, err
	})
	secondSession := <-sessions
	select {
	case <-secondSession.started:
	case <-ctx.Done():
		t.Fatal("second prompt did not start")
	}
	secondConversations, err := remote.TaskConversations(ctx, pool.Project, second.ID, "", 10)
	if err != nil || len(secondConversations) != 1 {
		t.Fatalf("second assignment conversations = %+v, err = %v", secondConversations, err)
	}
	// Session events are queued locally and flushed asynchronously, so wait
	// for the upload before counting; open calls guard against a reopen.
	readyCount := 0
	waitFor("second session_ready upload", func() (bool, error) {
		events, err := remote.ConversationEvents(ctx, pool.Project, secondConversations[0].ID, 0, 100)
		readyCount = 0
		for _, event := range events {
			if event.Event.Kind == "session_ready" {
				readyCount++
			}
		}
		return readyCount > 0, err
	})
	if readyCount != 1 || opened.Load() != 3 {
		t.Fatalf("second task native session_ready events = %d, open calls = %d, want 1 and 3", readyCount, opened.Load())
	}
	stopPool()
	select {
	case err := <-poolDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("pool did not stop before heartbeat audit")
	}
	registry, err := router.NewRegistry([]router.EventDefinition{plugins.ClientHeartbeatDefinition()}, []router.PluginDefinition{{
		ID: "lifetime-observer", Subscriptions: []router.Subscription{{Event: plugins.ClientHeartbeatID, MinVersion: 1, MaxVersion: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		n, err := remote.DrainRouterHeartbeatOutbox(ctx, registry, 200)
		if err != nil {
			t.Fatal(err)
		}
		if n < 200 {
			break
		}
	}
	var cursor int64
	var lifetime string
	reports := 0
	for {
		events, err := remote.RouterEvents(ctx, cursor, 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			cursor = event.Seq
			if event.ID != plugins.ClientHeartbeatID {
				continue
			}
			var heartbeat struct {
				Run string `json:"client_run"`
			}
			if err := json.Unmarshal(event.Payload, &heartbeat); err != nil {
				t.Fatal(err)
			}
			parsed, err := uuid.Parse(heartbeat.Run)
			if err != nil || parsed.String() != heartbeat.Run || (lifetime != "" && heartbeat.Run != lifetime) {
				t.Fatalf("worker lifetime changed across offline recovery or task reuse: %s", event.Payload)
			}
			lifetime = heartbeat.Run
			reports++
		}
		if len(events) < 200 {
			break
		}
	}
	if reports < 2 {
		t.Fatalf("expected multiple reports through worker lifetime, got %d", reports)
	}
}
