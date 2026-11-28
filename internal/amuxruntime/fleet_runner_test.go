package amuxruntime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/providers"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestFleetRetryBackoff(t *testing.T) {
	var retry fleetRetry
	now := time.Now()
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		retry.failed(now)
		if retry.delay != want || !retry.next.Equal(now.Add(want)) {
			t.Fatalf("retry = %+v, want delay %s", retry, want)
		}
		now = retry.next
	}
}

func TestFleetRetryFinished(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name     string
		delay    time.Duration
		duration time.Duration
		err      error
		want     time.Duration
	}{
		{"clean first run", 0, time.Second, nil, 0},
		{"clean stop clears failures", 8 * time.Minute, time.Second, nil, 0},
		{"short failure escalates", 2 * time.Minute, time.Second, errors.New("crash"), 4 * time.Minute},
		{"delay boundary escalates", 2 * time.Minute, 2 * time.Minute, errors.New("crash"), 4 * time.Minute},
		{"healthy run resets", 2 * time.Minute, 2*time.Minute + time.Second, errors.New("crash"), time.Minute},
		{"healthy run resets cap", 15 * time.Minute, 24 * time.Hour, errors.New("crash"), time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			retry := fleetRetry{delay: test.delay, next: now.Add(test.delay)}
			retry.finished(now.Add(-test.duration), now, test.err)
			if retry.delay != test.want {
				t.Fatalf("delay = %s, want %s", retry.delay, test.want)
			}
			if test.err == nil {
				if !retry.next.IsZero() {
					t.Fatalf("clean exit left retry deadline %s", retry.next)
				}
			} else if !retry.next.Equal(now.Add(test.want)) {
				t.Fatalf("retry deadline = %s, want %s", retry.next, now.Add(test.want))
			}
		})
	}
}

type fleetFixture struct {
	remote, local *storage.Store
	client        *gatewayclient.Client
	snapshots     chan []storage.FleetConversation
	failovers     chan string
	directory     string
	chat          *storage.Conversation
}

func newFleetFixture(t *testing.T, ctx context.Context) fleetFixture {
	t.Helper()
	openStore := func() *storage.Store {
		s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	remote, local := openStore(), openStore()
	if err := remote.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(remote, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	snapshots := make(chan []storage.FleetConversation)
	failovers := make(chan string, 10)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/failover") {
			failovers <- r.URL.Path
		}
		if r.URL.Path != "/v1/fleet/conversations/stream" {
			handler.ServeHTTP(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		stream := conn.CloseRead(r.Context())
		for {
			select {
			case <-stream.Done():
				return
			case chats := <-snapshots:
				if err := wsjson.Write(stream, conn, map[string]any{"version": 1, "chats": chats}); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(host.Close)
	client, err := gatewayclient.New(host.URL, "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	pool, err := local.CreateWorkerPool(ctx, storage.WorkerPool{ID: uuid.NewString(), Server: client.Endpoint(), Project: "demo", Directory: directory, Provider: "codex", OS: platform, SlotCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	slots, err := local.WorkerPoolSlots(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner := slots[0].Owner
	chat, err := remote.CreateConversation(ctx, "demo", "", &owner)
	if err != nil {
		t.Fatal(err)
	}
	return fleetFixture{remote, local, client, snapshots, failovers, directory, chat}
}

func TestFleetMissingRolloutDoesNotRestartOnReconcile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := newFleetFixture(t, ctx)
	remote, local, client, snapshots, directory, chat := fixture.remote, fixture.local, fixture.client, fixture.snapshots, fixture.directory, fixture.chat
	owner := chat.Owner
	if err := local.SaveWorkerRun(ctx, storage.WorkerRun{ID: chat.ID, ConversationID: chat.ID, Project: "demo", Server: client.Endpoint(), Directory: directory, Owner: owner}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.QueueWorkerEvent(ctx, chat.ID, conversation.Event{Kind: "session_ready", SessionID: "missing-rollout"}); err != nil {
		t.Fatal(err)
	}
	starts := make(chan string, 10)
	open := func(_ context.Context, run storage.WorkerRun, _ conversation.Sink) (providers.Session, error) {
		starts <- run.NativeSessionID
		return nil, errors.New("provider RPC error -32600: no rollout found for thread id missing-rollout")
	}
	changed := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runFleetConversations(ctx, local, client, open, nil, nil, func(string, string) string { return directory }, changed, io.Discard)
	}()
	defer func() { cancel(); <-done }()
	select {
	case snapshots <- []storage.FleetConversation{{Conversation: *chat}}:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case native := <-starts:
		if native != "missing-rollout" {
			t.Fatalf("resume lost native session: %q", native)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Wait for the exit handler to publish the user-visible retry explanation.
	for {
		events, err := remote.ConversationEvents(ctx, "demo", chat.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, event := range events {
			found = found || event.Event.Kind == "local_error" && strings.Contains(event.Event.Text, "no rollout found") && strings.Contains(event.Event.Text, "fleet close")
		}
		if found {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("missing user-visible failure")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// The unrecoverable thread asks for failover first; the gateway refuses this
	// bearer-only client, so the capped retry remains in force.
	select {
	case path := <-fixture.failovers:
		if !strings.Contains(path, chat.ID) {
			t.Fatalf("failover requested for %s", path)
		}
	default:
		t.Fatal("missing rollout did not request failover")
	}
	for range 20 {
		select {
		case changed <- struct{}{}:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	// Include a repeated desired-state snapshot, as on stream reconnect.
	select {
	case snapshots <- []storage.FleetConversation{{Conversation: *chat}}:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case native := <-starts:
		t.Fatalf("failed rollout restarted during backoff: %q", native)
	case <-time.After(100 * time.Millisecond):
	}
}

type blockingCloseSession struct {
	done    chan struct{}
	release <-chan struct{}
}

func (s *blockingCloseSession) Done() <-chan struct{}                          { return s.done }
func (s *blockingCloseSession) Err() error                                     { return nil }
func (s *blockingCloseSession) Prompt(context.Context, string) (string, error) { return "", nil }
func (s *blockingCloseSession) ResolvePermission(context.Context, string, string) error {
	return nil
}
func (s *blockingCloseSession) Close() error { <-s.release; return nil }

func TestFleetRestartsConversationDesiredAgainDuringStop(t *testing.T) {
	t.Run("running", func(t *testing.T) { testFleetRestartDuringStop(t, false) })
	t.Run("starting", func(t *testing.T) { testFleetRestartDuringStop(t, true) })
}

func testFleetRestartDuringStop(t *testing.T, duringStartup bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := newFleetFixture(t, ctx)
	starts := make(chan struct{}, 10)
	release := make(chan struct{})
	open := func(ctx context.Context, _ storage.WorkerRun, _ conversation.Sink) (providers.Session, error) {
		starts <- struct{}{}
		if duringStartup {
			<-ctx.Done()
			<-release
			return nil, ctx.Err()
		}
		return &blockingCloseSession{done: make(chan struct{}), release: release}, nil
	}
	// Every reconcile resolves the pool checkout once.
	reconciled := make(chan struct{}, 100)
	checkout := func(string, string) string {
		reconciled <- struct{}{}
		return fixture.directory
	}
	done := make(chan struct{})
	var stderr bytes.Buffer
	go func() {
		defer close(done)
		runFleetConversations(ctx, fixture.local, fixture.client, open, nil, nil, checkout, nil, &stderr)
	}()
	defer func() {
		cancel()
		<-done
		if strings.Contains(stderr.String(), "Fleet conversation stopped:") {
			t.Errorf("deliberate stop reported as a failure: %s", stderr.String())
		}
	}()
	defer close(release)
	desired := []storage.FleetConversation{{Conversation: *fixture.chat}}
	send := func(chats []storage.FleetConversation) {
		t.Helper()
		select {
		case fixture.snapshots <- chats:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case <-reconciled:
		case <-ctx.Done():
			t.Fatal("snapshot was not reconciled")
		}
	}
	waitStart := func() {
		t.Helper()
		select {
		case <-starts:
		case <-time.After(5 * time.Second):
			t.Fatal("conversation did not start")
		}
	}
	send(desired)
	waitStart()
	// The stop cannot finish until release, so the conversation is still
	// active when it becomes desired again.
	send(nil)
	send(desired)
	release <- struct{}{}
	waitStart()
}
