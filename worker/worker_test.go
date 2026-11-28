package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/providers"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

type fixtureSession struct {
	done      chan struct{}
	closeOnce sync.Once
	sink      conversation.Sink
	entered   chan string
	resolved  chan struct{}
	count     atomic.Int32
}

func (f *fixtureSession) Prompt(ctx context.Context, text string) (string, error) {
	f.count.Add(1)
	f.entered <- text
	if err := f.sink(ctx, conversation.Event{Kind: "permission", SessionID: "native", RequestID: `"permission-1"`, Options: []conversation.PermissionOption{{ID: "reject", Name: "Reject", Kind: "reject_once"}}}); err != nil {
		return "", err
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-f.resolved:
	}
	if err := f.sink(ctx, conversation.Event{Kind: "turn_completed", SessionID: "native", Status: "end_turn"}); err != nil {
		return "", err
	}
	return "end_turn", nil
}
func (f *fixtureSession) ResolvePermission(_ context.Context, id, option string) error {
	if id != `"permission-1"` || option != "reject" {
		return errors.New("unexpected permission")
	}
	f.resolved <- struct{}{}
	return nil
}
func (f *fixtureSession) Done() <-chan struct{} { return f.done }
func (f *fixtureSession) Err() error            { return nil }
func (f *fixtureSession) Close() error          { f.closeOnce.Do(func() { close(f.done) }); return nil }

type flakyTransport struct {
	base      http.RoundTripper
	offline   atomic.Bool
	loseClaim atomic.Bool
	loseEvent atomic.Bool
}

func (f *flakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.offline.Load() {
		return nil, errors.New("offline")
	}
	result, err := f.base.RoundTrip(r)
	if err == nil && r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/events") && f.loseEvent.CompareAndSwap(true, false) {
		result.Body.Close()
		return nil, errors.New("event acknowledgement lost")
	}
	if err == nil && strings.HasSuffix(r.URL.Path, "/claim") && f.loseClaim.CompareAndSwap(true, false) {
		result.Body.Close()
		return nil, errors.New("claim response lost")
	}
	return result, err
}
func TestWorkerKeepsOfflineOutputAndDoesNotRepeatLostClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if err = remote.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Remote task", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "antigravity", OS: "linux"}
	bound, err := remote.CreateConversation(ctx, "demo", task.ID, &owner)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(remote, "secret")
	if err != nil {
		t.Fatal(err)
	}
	var current atomic.Value
	current.Store(handler)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { current.Load().(http.Handler).ServeHTTP(w, r) }))
	defer srv.Close()
	defer func() {
		current.Load().(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())
	}()
	transport := &flakyTransport{base: http.DefaultTransport}
	transport.loseClaim.Store(true)
	transport.loseEvent.Store(true)
	client, err := gatewayclient.New(srv.URL, "secret", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	local, err := storage.Open(ctx, filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	run := storage.WorkerRun{ID: uuid.NewString(), Server: client.Endpoint(), Project: "demo", ConversationID: bound.ID, Directory: t.TempDir(), Owner: owner}
	if err = local.SaveWorkerRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	provider := &fixtureSession{done: make(chan struct{}), entered: make(chan string, 2), resolved: make(chan struct{}, 1)}
	workerCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	var once sync.Once
	open := func(ctx context.Context, r storage.WorkerRun, sink conversation.Sink) (providers.Session, error) {
		once.Do(func() { provider.sink = sink })
		if err := sink(ctx, conversation.Event{Kind: "session_ready", SessionID: "native"}); err != nil {
			return nil, err
		}
		return provider, nil
	}
	go func() {
		defer close(done)
		done <- Run(workerCtx, Config{Store: local, Client: client, RunID: run.ID, Open: open})
	}()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("worker did not stop")
		}
	}()
	promptID := uuid.NewString()
	if err = client.Enqueue(ctx, "demo", bound.ID, promptID, conversation.Command{Kind: "prompt", Text: "Inspect the task"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.entered:
	case err := <-done:
		t.Fatalf("worker exited: %v", err)
	case <-ctx.Done():
		t.Fatal("worker never entered provider")
	}
	var permission storage.ConversationEvent
	for permission.ID == "" {
		changed := remote.ConversationChanges()
		taskChats, err := client.TaskConversations(ctx, "demo", task.ID, "", 10)
		if err != nil || len(taskChats) != 1 || taskChats[0].ID != bound.ID || taskChats[0].TaskID != task.ID {
			t.Fatalf("task history lost the execution conversation: %+v, %v", taskChats, err)
		}
		events, err := remote.ConversationEvents(ctx, "demo", taskChats[0].ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Event.Kind == "permission" {
				permission = event
			}
		}
		if permission.ID == "" {
			select {
			case <-changed:
			case <-ctx.Done():
				t.Fatal("permission was not published")
			}
		}
	}
	transport.offline.Store(true)
	if err := handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(ctx); err != nil {
		t.Fatal(err)
	}
	// Provider output continues without an HTTP connection or a UI.
	if err = provider.sink(ctx, conversation.Event{Kind: "text_delta", SessionID: "native", Text: "recorded while offline"}); err != nil {
		t.Fatal(err)
	}
	outbox, err := local.WorkerOutbox(ctx, run.ID, 100)
	if err != nil || len(outbox) == 0 {
		t.Fatalf("offline output not durable: %+v %v", outbox, err)
	}
	replacement, err := server.NewHandler(remote, "secret")
	if err != nil {
		t.Fatal(err)
	}
	current.Store(replacement)
	transport.offline.Store(false)
	if err = client.Enqueue(ctx, "demo", bound.ID, uuid.NewString(), conversation.Command{Kind: "permission", PermissionEventID: permission.ID, OptionID: "reject"}); err != nil {
		t.Fatal(err)
	}
	finished := false
	for !finished {
		changed := remote.ConversationChanges()
		commands, err := remote.ConversationCommands(ctx, "demo", bound.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		finished = true
		for _, command := range commands {
			if command.State != "done" {
				finished = false
			}
		}
		if !finished {
			select {
			case <-changed:
			case err := <-done:
				t.Fatalf("worker exited before completion: %v", err)
			case <-ctx.Done():
				t.Fatal("worker did not reconnect")
			}
		}
	}
	if provider.count.Load() != 1 {
		t.Fatalf("prompt executed %d times", provider.count.Load())
	}
	events, err := remote.ConversationEvents(ctx, "demo", bound.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	readyEvents := 0
	for _, event := range events {
		if event.Event.Kind == "session_ready" {
			readyEvents++
		}
		if event.Event.Text == "recorded while offline" {
			found = true
		}
	}
	if readyEvents != 1 {
		t.Fatalf("lost acknowledgement duplicated session event: %d", readyEvents)
	}
	if !found {
		t.Fatal("offline output disappeared on reconnect")
	}
	currentOwner, err := remote.Conversation(ctx, "demo", bound.ID)
	if err != nil {
		t.Fatal(err)
	}
	replacementOwner := currentOwner.Owner
	replacementOwner.InstanceID = uuid.NewString()
	if _, err = remote.ResumeConversation(ctx, "demo", bound.ID, currentOwner.Owner.InstanceID, &replacementOwner); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var response *gatewayclient.HTTPError
		if !errors.As(err, &response) || response.Status != 409 {
			t.Fatalf("worker did not stop on owner fence: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("stale worker continued after ownership change")
	}

}
