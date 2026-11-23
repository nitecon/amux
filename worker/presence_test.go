package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
)

type runPresenceRequest struct {
	Source    string                  `json:"source"`
	Sequence  int64                   `json:"sequence"`
	ClientRun *string                 `json:"client_run"`
	OS        string                  `json:"os"`
	Agents    []storage.PresenceAgent `json:"agents"`
}

type loseRunPresenceAck struct {
	base    http.RoundTripper
	mu      sync.Mutex
	reports []runPresenceRequest
	lose    atomic.Bool
}

func (t *loseRunPresenceAck) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/worker-presence") {
		var report runPresenceRequest
		body, err := request.GetBody()
		if err != nil {
			return nil, err
		}
		decodeErr := json.NewDecoder(body).Decode(&report)
		body.Close()
		if decodeErr != nil {
			return nil, decodeErr
		}
		t.mu.Lock()
		t.reports = append(t.reports, report)
		t.mu.Unlock()
		response, err := t.base.RoundTrip(request)
		if err == nil && t.lose.CompareAndSwap(true, false) {
			response.Body.Close()
			return nil, errors.New("presence acknowledgement lost")
		}
		return response, err
	}
	return t.base.RoundTrip(request)
}

type waitingPresenceSession struct {
	done chan struct{}
	once sync.Once
}

func (s *waitingPresenceSession) Done() <-chan struct{} { return s.done }
func (s *waitingPresenceSession) Err() error            { return nil }
func (s *waitingPresenceSession) Prompt(context.Context, string) (string, error) {
	return "", nil
}
func (s *waitingPresenceSession) ResolvePermission(context.Context, string, string) error {
	return nil
}
func (s *waitingPresenceSession) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

func TestRunPresenceRetriesLostAckAndOmitsAfterProviderStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	transport := &loseRunPresenceAck{base: http.DefaultTransport}
	transport.lose.Store(true)
	remote, local, _, host, pool, slot := poolRegistrationFixture(t, ctx, "", false, transport)
	defer remote.Close()
	defer local.Close()
	defer host.Close()
	defer host.Config.Handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())

	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: pool.Project, Title: "selected execution", Reporter: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = remote.TransitionTask(ctx, pool.Project, task.ID, "in_progress", &slot.Owner); err != nil {
		t.Fatal(err)
	}
	conv, err := remote.CreateConversation(ctx, pool.Project, task.ID, &slot.Owner)
	if err != nil {
		t.Fatal(err)
	}
	run := storage.WorkerRun{ID: uuid.NewString(), Server: host.URL, Project: pool.Project, ConversationID: conv.ID, Directory: pool.Directory, Owner: slot.Owner}
	if err = local.SaveWorkerRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	workerCtx, stop := context.WithCancel(ctx)
	client := mustPoolClient(t, host.URL, transport)
	opened := make(chan struct{}, 1)
	done := make(chan error, 1)
	provider := &waitingPresenceSession{done: make(chan struct{})}
	go func() {
		done <- Run(workerCtx, Config{Store: local, Client: client, RunID: run.ID, AdvertisePresence: true, Open: func(context.Context, storage.WorkerRun, conversation.Sink) (Session, error) {
			opened <- struct{}{}
			return provider, nil
		}})
	}()
	defer stop()
	select {
	case <-opened:
	case err := <-done:
		t.Fatalf("worker exited before opening provider: %v", err)
	case <-ctx.Done():
		t.Fatal("provider did not open")
	}
	for {
		transport.mu.Lock()
		count := len(transport.reports)
		transport.mu.Unlock()
		if count >= 2 {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("lost acknowledgement was not retried")
		}
	}
	transport.mu.Lock()
	first, retried := transport.reports[0], transport.reports[1]
	transport.mu.Unlock()
	if first.Source != "worker:"+run.ID || first.Sequence != 1 || first.ClientRun == nil || *first.ClientRun == "" || first.Source != retried.Source || first.Sequence != retried.Sequence || *first.ClientRun != *retried.ClientRun || len(first.Agents) != 1 {
		t.Fatalf("lost-ack retry changed stable presence body: first=%+v retry=%+v", first, retried)
	}
	agent := first.Agents[0]
	current, err := local.WorkerRun(ctx, run.ID)
	if err != nil || agent.Origin != current.Owner || agent.Generation == nil || *agent.Generation != current.Owner.InstanceID || !agent.Busy || agent.AutoAssign || agent.ReviewContextClear || agent.Goal != nil || agent.LastActivityAt != nil {
		t.Fatalf("selected worker presence=%+v current=%+v err=%v", agent, current, err)
	}
	targets, err := remote.Targets(ctx, pool.Project, time.Now())
	if err != nil || len(targets) != 1 || targets[0].Target.Origin != current.Owner || targets[0].Target.Generation != current.Owner.InstanceID {
		t.Fatalf("registered target did not follow current worker owner: targets=%+v current=%+v err=%v", targets, current.Owner, err)
	}
	stop()
	select {
	case err = <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("worker shutdown: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("worker did not finish cleanup")
	}
	transport.mu.Lock()
	cleanup := transport.reports[len(transport.reports)-1]
	transport.mu.Unlock()
	if len(cleanup.Agents) != 0 || cleanup.Sequence <= first.Sequence || cleanup.Source != first.Source || *cleanup.ClientRun != *first.ClientRun {
		t.Fatalf("cleanup report=%+v after initial=%+v", cleanup, first)
	}
	detail, err := remote.Task(ctx, pool.Project, task.ID)
	if err != nil || detail.Task.Status != "in_progress" {
		t.Fatalf("presence cleanup released selected work: task=%+v err=%v", detail, err)
	}
}

func TestRunPresenceRejectsWrongCheckoutBeforeProviderOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	remote, local, client, host, pool, slot := poolRegistrationFixture(t, ctx, "github.com/example/native-pool", true, http.DefaultTransport)
	defer remote.Close()
	defer local.Close()
	defer host.Close()
	defer host.Config.Handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())

	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: pool.Project, Title: "keep selected work", Reporter: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = remote.TransitionTask(ctx, pool.Project, task.ID, "in_progress", &slot.Owner); err != nil {
		t.Fatal(err)
	}
	conv, err := remote.CreateConversation(ctx, pool.Project, task.ID, &slot.Owner)
	if err != nil {
		t.Fatal(err)
	}
	run := storage.WorkerRun{ID: uuid.NewString(), Server: host.URL, Project: pool.Project, ConversationID: conv.ID, Directory: pool.Directory, Owner: slot.Owner}
	if err = local.SaveWorkerRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	runGit(t, pool.Directory, "remote", "set-url", "origin", "git@github.com:example/replacement.git")
	var opened atomic.Int32
	err = Run(ctx, Config{Store: local, Client: client, RunID: run.ID, AdvertisePresence: true, Open: func(context.Context, storage.WorkerRun, conversation.Sink) (Session, error) {
		opened.Add(1)
		return nil, errors.New("unexpected provider open")
	}})
	var response *gatewayclient.HTTPError
	if !errors.As(err, &response) || response.Status != http.StatusConflict || opened.Load() != 0 {
		t.Fatalf("wrong checkout: err=%v opened=%d", err, opened.Load())
	}
	stored, err := local.WorkerRun(ctx, run.ID)
	if err != nil || stored.Owner.InstanceID == slot.Owner.InstanceID || stored.PresenceSequence < 2 {
		t.Fatalf("cleanup did not use current owner/sequence: run=%+v err=%v", stored, err)
	}
	detail, err := remote.Task(ctx, pool.Project, task.ID)
	if err != nil || detail.Task.Status != "in_progress" || detail.Task.OwnerOrigin == nil || detail.Task.OwnerOrigin.InstanceID != stored.Owner.InstanceID {
		t.Fatalf("wrong checkout released task: task=%+v err=%v", detail, err)
	}
}

func mustPoolClient(t *testing.T, endpoint string, transport http.RoundTripper) *gatewayclient.Client {
	t.Helper()
	client, err := gatewayclient.New(endpoint, "pool-registration-secret", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
