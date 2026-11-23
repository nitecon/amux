package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

type reviewPoolSession struct {
	done      chan struct{}
	sink      conversation.Sink
	nativeID  string
	closeOnce sync.Once
}

type reviewPoolOpenedRun struct {
	run    storage.WorkerRun
	prompt chan struct{}
}

func (s *reviewPoolSession) Prompt(ctx context.Context, _ string) (string, error) {
	if err := s.sink(ctx, conversation.Event{Kind: "turn_completed", SessionID: s.nativeID, Status: "end_turn"}); err != nil {
		return "", err
	}
	return "end_turn", nil
}

func (s *reviewPoolSession) ResolvePermission(context.Context, string, string) error { return nil }
func (s *reviewPoolSession) Done() <-chan struct{}                                   { return s.done }
func (s *reviewPoolSession) Err() error                                              { return nil }
func (s *reviewPoolSession) Close() error {
	s.closeOnce.Do(func() { close(s.done) })
	return nil
}

func TestRunPoolClaimsFallbackReviewWithFreshContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if err = remote.InsertProject(ctx, storage.Project{Ident: "review-pool", ChannelName: "review-pool", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	source, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "review-pool", Title: "Implement feature", Reporter: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(remote, "review-pool-secret")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	defer handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())
	client, err := gatewayclient.New(host.URL, "review-pool-secret", nil)
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
	pool, err := local.CreateWorkerPool(ctx, storage.WorkerPool{ID: uuid.NewString(), Server: host.URL, Project: "review-pool", Directory: t.TempDir(), Provider: "codex", OS: osName, SlotCount: 1})
	if err != nil {
		t.Fatal(err)
	}

	opened := make(chan reviewPoolOpenedRun, 3)
	var openCount atomic.Int32
	open := func(openCtx context.Context, run storage.WorkerRun, sink conversation.Sink) (Session, error) {
		if run.NativeSessionID != "" {
			return nil, fmt.Errorf("new task run reused native session %q", run.NativeSessionID)
		}
		n := openCount.Add(1)
		session := &reviewPoolSession{done: make(chan struct{}), sink: sink, nativeID: fmt.Sprintf("review-pool-native-%d", n)}
		if err := sink(openCtx, conversation.Event{Kind: "session_ready", SessionID: session.nativeID}); err != nil {
			return nil, err
		}
		entry := reviewPoolOpenedRun{run: run, prompt: make(chan struct{}, 1)}
		opened <- entry
		return &reviewPoolPromptSession{reviewPoolSession: session, prompt: entry.prompt}, nil
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
				t.Errorf("worker pool shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("worker pool did not stop")
		}
	}()

	waitFor := func(label string, condition func() (bool, error)) {
		t.Helper()
		for {
			ok, err := condition()
			if err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			if ok {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("timed out waiting for %s", label)
			case err := <-poolDone:
				t.Fatalf("worker pool exited waiting for %s: %v", label, err)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	waitFor("pool review capability presence", func() (bool, error) {
		presence, err := remote.Presence(ctx, pool.Project)
		return hasOneLiveFreshReviewPresence(presence), err
	})
	if err = remote.ReconcileAssignments(ctx, time.Now().Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	var sourceConversation string
	waitFor("source assignment", func() (bool, error) {
		conversations, err := remote.TaskConversations(ctx, pool.Project, source.ID, "", 10)
		if err == nil && len(conversations) == 1 {
			sourceConversation = conversations[0].ID
		}
		return sourceConversation != "", err
	})
	sourceRun := receiveReviewPoolRun(t, ctx, opened, poolDone, "source provider prompt")
	select {
	case <-sourceRun.prompt:
	case <-ctx.Done():
		t.Fatal("source provider prompt did not start")
	}
	sourceDetail, err := remote.Task(ctx, pool.Project, source.ID)
	if err != nil || sourceDetail.Task.OwnerOrigin == nil || sourceDetail.Task.OwnerOrigin.Provider != "codex" {
		t.Fatalf("source ownership = %+v, err = %v", sourceDetail, err)
	}
	changes := json.RawMessage(`["main.go"]`)
	if _, err = remote.UpdateTaskWorkflow(ctx, pool.Project, source.ID, storage.TaskWorkflowUpdate{Status: "done", Changeset: changes}, sourceDetail.Task.OwnerOrigin); err == nil {
		t.Fatal("native software assignment bypassed review via task completion")
	}
	implementer := sourceDetail.Task.OwnerOrigin
	receipt, err := remote.AddTaskComment(ctx, pool.Project, source.ID, implementer.SessionID, "agent", "Work submitted: fixture commit", nil, implementer)
	if err != nil {
		t.Fatal(err)
	}
	action := "Review the fixture commit."
	if _, err = remote.UpdateTaskWorkflow(ctx, pool.Project, source.ID, storage.TaskWorkflowUpdate{
		Status: "user_review", ReviewAction: &action, Changeset: changes, SubmissionCommentID: receipt.ID,
	}, implementer); err != nil {
		t.Fatal(err)
	}
	reviews, err := remote.TaskReviewChildren(ctx, pool.Project, source.ID)
	if err != nil || len(reviews) != 1 {
		t.Fatalf("source review children = %+v, err = %v", reviews, err)
	}
	for _, review := range reviews {
		if review.Status != "todo" || !containsReviewLabel(review.Labels, "review-exclude:"+sourceDetail.Task.OwnerOrigin.SessionID) || !containsReviewLabel(review.Labels, "review-provider:claude") {
			t.Fatalf("review did not preserve contributor/provider preferences: %+v", review)
		}
	}
	waitFor("source run release", func() (bool, error) {
		slots, err := local.WorkerPoolSlots(ctx, pool.ID)
		return err == nil && len(slots) == 1 && slots[0].RunID == "", err
	})
	waitFor("fresh review capability re-advertised", func() (bool, error) {
		presence, err := remote.Presence(ctx, pool.Project)
		return hasOneLiveFreshReviewPresence(presence), err
	})

	// One eligible Codex slot is the source contributor and misses the preferred
	// Claude provider. Its fresh-context capability allows it to take fallback.
	if err = remote.ReconcileAssignments(ctx, time.Now().Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	waitFor("one fallback review claimed", func() (bool, error) {
		for i := range reviews {
			current, err := remote.Task(ctx, pool.Project, reviews[i].ID)
			if err != nil {
				return false, err
			}
			reviews[i] = current.Task
		}
		claimed := 0
		for _, review := range reviews {
			if review.Status == "in_progress" && review.OwnerOrigin != nil && review.OwnerOrigin.SessionID == sourceDetail.Task.OwnerOrigin.SessionID {
				claimed++
			}
		}
		return claimed == 1, nil
	})
	claimed := 0
	var reviewTask storage.Task
	for _, review := range reviews {
		if review.Status == "in_progress" {
			claimed++
			reviewTask = review
		}
	}
	if claimed != 1 {
		t.Fatalf("reviews after one scheduler tick = %+v; want exactly one claimed", reviews)
	}
	reviewRun := receiveReviewPoolRun(t, ctx, opened, poolDone, "fallback review provider prompt")
	select {
	case <-reviewRun.prompt:
	case <-ctx.Done():
		t.Fatal("fallback review provider prompt did not start")
	}
	if sourceRun.run.ID == reviewRun.run.ID || sourceRun.run.ConversationID == reviewRun.run.ConversationID {
		t.Fatalf("review reused source run/conversation: source=%+v review=%+v", sourceRun.run, reviewRun.run)
	}
	currentReview, err := remote.Task(ctx, pool.Project, reviewTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = remote.FinishSubmissionReview(ctx, pool.Project, reviewTask.ID, "approved", "Verified the fixture commit.", nil, currentReview.Task.OwnerOrigin); err != nil {
		t.Fatal(err)
	}
	parent, err := remote.Task(ctx, pool.Project, source.ID)
	if err != nil || parent.Task.Status != "done" {
		t.Fatalf("fallback review did not close the software task: %+v %v", parent, err)
	}
	children, err := remote.TaskReviewChildren(ctx, pool.Project, reviewTask.ID)
	if err != nil || len(children) != 0 {
		t.Fatalf("completed review task has recursive review children %+v, err = %v", children, err)
	}
}

type reviewPoolPromptSession struct {
	*reviewPoolSession
	prompt chan struct{}
}

func (s *reviewPoolPromptSession) Prompt(ctx context.Context, text string) (string, error) {
	s.prompt <- struct{}{}
	return s.reviewPoolSession.Prompt(ctx, text)
}

func receiveReviewPoolRun(t *testing.T, ctx context.Context, opened <-chan reviewPoolOpenedRun, poolDone <-chan error, label string) reviewPoolOpenedRun {
	t.Helper()
	select {
	case run := <-opened:
		return run
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", label)
	case err := <-poolDone:
		t.Fatalf("worker pool exited waiting for %s: %v", label, err)
	}
	return reviewPoolOpenedRun{}
}

func containsReviewLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func hasOneLiveFreshReviewPresence(presence []storage.AgentPresence) bool {
	live := 0
	ready := false
	for _, agent := range presence {
		if agent.State == "gone" {
			continue
		}
		live++
		ready = !agent.Busy && agent.AutoAssign && agent.ReviewContextClear
	}
	return live == 1 && ready
}
