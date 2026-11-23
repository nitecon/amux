package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fynetest "fyne.io/fyne/v2/test"
	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

const assignmentFixtureRemote = "github.com/example/desktop-assignment"

func TestBrowserRequesterSetAndClearPreserveOwnerForUserReviewTask(t *testing.T) {
	type observedRequest struct {
		present bool
		value   json.RawMessage
		actor   string
	}
	requests := make(chan observedRequest, 2)
	fixture, identity := prepareAssignmentFixture(t, func(api http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/requester") {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read requester request: %v", err)
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Errorf("decode requester request %s: %v", body, err)
				}
				value, present := fields["requester"]
				requests <- observedRequest{present: present, value: value, actor: r.Header.Get("X-Agent-Session-Id")}
			}
			api.ServeHTTP(w, r)
		})
	})
	ctx := context.Background()
	owner := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "claude", OS: "linux"}
	if _, err := fixture.store.ApplyTaskTransition(ctx, "demo", fixture.first.ID, "in_progress", &owner, storage.TaskTransitionPolicy{}); err != nil {
		t.Fatal(err)
	}
	reviewAction := "Review the final result"
	if _, err := fixture.store.UpdateTaskWorkflow(ctx, "demo", fixture.first.ID, storage.TaskWorkflowUpdate{
		Status: "user_review", ReviewAction: &reviewAction, Changeset: json.RawMessage("null"),
	}, &owner); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.store.Task(ctx, "demo", fixture.first.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.browser.statusSelect.SetSelected("User Review")
	waitBrowser(t, fixture.callbacks, func() bool {
		return len(fixture.browser.visibleTasks) == 1 && fixture.browser.visibleTasks[0].ID == fixture.first.ID
	})
	fixture.browser.tasks.Select(0)
	waitBrowser(t, fixture.callbacks, func() bool {
		return fixture.browser.selectedTaskDetail != nil && fixture.browser.selectedTaskDetail.Task.Status == "user_review"
	})
	a := fixture.browser.taskAssignment
	waitBrowser(t, fixture.callbacks, func() bool { return len(a.target.Options) == 1 })
	a.target.SetSelected(a.target.Options[0])
	if !a.assign.Disabled() || a.watch.Disabled() {
		t.Fatalf("result recipient controls should remain available for user review while assignment is active-gated: assignDisabled=%t watchDisabled=%t", a.assign.Disabled(), a.watch.Disabled())
	}
	fynetest.Tap(a.watch)
	draft := a.drafts[taskEditorKey("demo", fixture.first.ID)]
	waitBrowser(t, fixture.callbacks, func() bool {
		return !draft.inFlight && draft.requester != nil && *draft.requester == identity
	})
	assigned, err := fixture.store.Task(ctx, "demo", fixture.first.ID)
	if err != nil || string(assigned.Task.Requester) == "null" || assigned.Task.Status != "user_review" || !reflect.DeepEqual(assigned.Task.OwnerOrigin, before.Task.OwnerOrigin) || !reflect.DeepEqual(assigned.Task.OwnerAgentID, before.Task.OwnerAgentID) || !reflect.DeepEqual(assigned.Task.StartedAt, before.Task.StartedAt) || !reflect.DeepEqual(assigned.Task.CompletionOwner, before.Task.CompletionOwner) || before.Task.CompletionOwner == nil {
		t.Fatalf("setting result recipient changed task ownership/workflow: before=%+v after=%+v err=%v", before.Task, assigned.Task, err)
	}
	setRequest := <-requests
	var setTarget *storage.RegisteredTargetIdentity
	if !setRequest.present || json.Unmarshal(setRequest.value, &setTarget) != nil || setTarget == nil || *setTarget != identity || setRequest.actor != "" {
		t.Fatalf("save did not send exact requester without impersonating an agent: %+v", setRequest)
	}
	if a.clearWatch.Disabled() {
		t.Fatal("clear result recipient was unavailable after save")
	}
	fynetest.Tap(a.clearWatch)
	waitBrowser(t, fixture.callbacks, func() bool { return !draft.inFlight && draft.requester == nil })
	cleared, err := fixture.store.Task(ctx, "demo", fixture.first.ID)
	if err != nil || requesterFromTask(cleared.Task) != nil || cleared.Task.Status != "user_review" || !reflect.DeepEqual(cleared.Task.OwnerOrigin, before.Task.OwnerOrigin) || !reflect.DeepEqual(cleared.Task.OwnerAgentID, before.Task.OwnerAgentID) || !reflect.DeepEqual(cleared.Task.StartedAt, before.Task.StartedAt) || !reflect.DeepEqual(cleared.Task.CompletionOwner, before.Task.CompletionOwner) || before.Task.CompletionOwner == nil {
		t.Fatalf("clearing result recipient changed task ownership/workflow: before=%+v after=%+v err=%v", before.Task, cleared.Task, err)
	}
	clearRequest := <-requests
	var clearedTarget *storage.RegisteredTargetIdentity
	if !clearRequest.present || json.Unmarshal(clearRequest.value, &clearedTarget) != nil || clearedTarget != nil || clearRequest.actor != "" {
		t.Fatalf("clear did not send explicit requester null without impersonation: %+v", clearRequest)
	}
}

func TestBrowserRequesterSetPreservesLiveInProgressOwner(t *testing.T) {
	fixture, identity := prepareAssignmentFixture(t, nil)
	ctx := context.Background()
	owner := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "claude", OS: "linux"}
	if _, err := fixture.store.ApplyTaskTransition(ctx, "demo", fixture.first.ID, "in_progress", &owner, storage.TaskTransitionPolicy{}); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.store.Task(ctx, "demo", fixture.first.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.browser.Refresh()
	waitBrowser(t, fixture.callbacks, func() bool {
		return fixture.browser.selectedTaskDetail != nil && fixture.browser.selectedTaskDetail.Task.Status == "in_progress"
	})
	a := fixture.browser.taskAssignment
	waitBrowser(t, fixture.callbacks, func() bool { return len(a.target.Options) == 1 })
	a.target.SetSelected(a.target.Options[0])
	fynetest.Tap(a.watch)
	draft := a.drafts[taskEditorKey("demo", fixture.first.ID)]
	waitBrowser(t, fixture.callbacks, func() bool { return !draft.inFlight && draft.requester != nil && *draft.requester == identity })
	after, err := fixture.store.Task(ctx, "demo", fixture.first.ID)
	if err != nil || after.Task.Status != "in_progress" || !reflect.DeepEqual(after.Task.OwnerOrigin, before.Task.OwnerOrigin) || !reflect.DeepEqual(after.Task.OwnerAgentID, before.Task.OwnerAgentID) || !reflect.DeepEqual(after.Task.StartedAt, before.Task.StartedAt) || after.Task.OwnerOrigin == nil || *after.Task.OwnerOrigin != owner {
		t.Fatalf("setting result recipient changed live task owner: before=%+v after=%+v err=%v", before.Task, after.Task, err)
	}
}

func prepareAssignmentFixture(t *testing.T, wrap func(http.Handler) http.Handler) (editorBrowserFixture, storage.RegisteredTargetIdentity) {
	t.Helper()
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "assignment.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	remote := assignmentFixtureRemote
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "repo", CanonicalRemote: &remote}); err != nil {
		t.Fatal(err)
	}
	first, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "First", Reporter: "user", Labels: []string{"os:linux", "keep:fixture"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Second", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	api, err := server.NewHandler(store, "editor-key")
	if err != nil {
		t.Fatal(err)
	}
	var handler http.Handler = api
	if wrap != nil {
		handler = wrap(api)
	}
	host := httptest.NewServer(handler)
	t.Cleanup(host.Close)
	t.Cleanup(func() { _ = api.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(ctx) })
	client, err := gatewayclient.New(host.URL, "editor-key", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	origin := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	identity := storage.RegisteredTargetIdentity{Origin: origin, Generation: uuid.NewString()}
	if _, err := client.RegisterTarget(ctx, "demo", storage.TargetRegistration{Origin: origin, Generation: identity.Generation, CanonicalRemote: &remote, DisplayName: stringPtrDesktop("Fixture Agent"), Hostname: stringPtrDesktop("worker-host")}); err != nil {
		t.Fatal(err)
	}
	wrongOS := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "windows"}
	if _, err := client.RegisterTarget(ctx, "demo", storage.TargetRegistration{Origin: wrongOS, Generation: uuid.NewString(), CanonicalRemote: &remote}); err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	browser, err := NewBrowser(BrowserConfig{Client: client, Project: "demo", Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { browser.Close(); browser.Wait(); callbacks.pump() })
	browser.Start(ctx)
	waitBrowser(t, callbacks, func() bool { return len(browser.taskItems) == 2 })
	browser.selectTask(first.ID)
	waitBrowser(t, callbacks, func() bool {
		return browser.selectedTaskDetail != nil && browser.selectedTaskDetail.Task.ID == first.ID
	})
	fixture := editorBrowserFixture{store: store, first: first, second: second, browser: browser, callbacks: callbacks}
	fixture.browser.taskAssignment.loadTargets(false)
	waitBrowser(t, fixture.callbacks, func() bool {
		d := fixture.browser.taskAssignment.currentDraft()
		return d != nil && d.targetsLoaded && !d.loading
	})
	return fixture, identity
}

func stringPtrDesktop(value string) *string { return &value }

func TestTaskOwnerLabelUsesFreshSessionMetadataAfterInstanceRestart(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	session := uuid.NewString()
	owner := storage.AgentOrigin{SessionID: session, InstanceID: uuid.NewString(), Provider: "claude", OS: "linux"}
	nameOld, hostOld := "Old display", "Old machine"
	nameCurrent, hostCurrent := "Current display", "Current machine"
	task := storage.Task{OwnerAgentID: stringPtrDesktop("legacy-agent-id"), OwnerOrigin: &owner, Hostname: stringPtrDesktop("task-creation-machine")}
	targets := []storage.RegisteredTarget{
		{
			Target: storage.RegisteredTargetIdentity{Origin: owner, Generation: uuid.NewString()},
			SeenAt: now.Add(-time.Minute).UnixMilli(), ExpiresAt: now.Add(time.Minute).UnixMilli(),
			DisplayName: &nameOld, Hostname: &hostOld,
		},
		{
			Target: storage.RegisteredTargetIdentity{Origin: storage.AgentOrigin{SessionID: session, InstanceID: uuid.NewString(), Provider: "codex", OS: "windows"}, Generation: uuid.NewString()},
			SeenAt: now.Add(-time.Second).UnixMilli(), ExpiresAt: now.Add(time.Minute).UnixMilli(),
			DisplayName: &nameCurrent, Hostname: &hostCurrent,
		},
		{
			Target: storage.RegisteredTargetIdentity{Origin: storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}, Generation: uuid.NewString()},
			SeenAt: now.UnixMilli(), ExpiresAt: now.Add(time.Minute).UnixMilli(),
			DisplayName: &nameOld, Hostname: &hostOld,
		},
		{
			Target: storage.RegisteredTargetIdentity{Origin: storage.AgentOrigin{SessionID: session, InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}, Generation: uuid.NewString()},
			SeenAt: now.UnixMilli(), ExpiresAt: now.UnixMilli(),
			DisplayName: &nameOld, Hostname: &hostOld,
		},
	}
	checkedAt := now.Add(-time.Second)
	if got := taskOwnerLabel(task, targets, checkedAt, now); got != "Current display · Current machine (external)" {
		t.Fatalf("owner label=%q", got)
	}
	if got := taskOwnerLabel(task, targets, now.Add(-time.Minute-time.Second), now); got != "legacy-agent-id · machine unavailable (external)" {
		t.Fatalf("stale metadata label=%q", got)
	}
	if got := taskOwnerLabel(storage.Task{}, targets, checkedAt, now); got != "" {
		t.Fatalf("unowned task label=%q", got)
	}
}

func TestBrowserAssignmentFencesOSAndCompletesAfterReturningDuringFlight(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var writes atomic.Int32
	fixture, identity := prepareAssignmentFixture(t, func(api http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/assign") {
				writes.Add(1)
				entered <- struct{}{}
				<-release
			}
			api.ServeHTTP(w, r)
		})
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	b, a := fixture.browser, fixture.browser.taskAssignment
	if len(a.target.Options) != 1 || !strings.Contains(a.target.Options[0], identity.Origin.SessionID) {
		t.Fatalf("eligible target choices=%v", a.target.Options)
	}
	a.target.SetSelected(a.target.Options[0])
	fynetest.Tap(a.assign)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("assignment request did not reach local server")
	}
	draft := a.drafts[taskEditorKey("demo", fixture.first.ID)]
	b.selectTask(fixture.second.ID)
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.second.ID })
	b.selectTask(fixture.first.ID)
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.first.ID })
	if !draft.inFlight {
		t.Fatal("returning to a task cleared its unresolved assignment")
	}
	a.submit()
	if writes.Load() != 1 {
		t.Fatalf("unresolved assignment was submitted %d times", writes.Load())
	}
	releaseOnce.Do(func() { close(release) })
	waitBrowser(t, fixture.callbacks, func() bool {
		return !draft.inFlight && draft.task.OwnerOrigin != nil && *draft.task.OwnerOrigin == identity.Origin
	})
	if draft.unknown || writes.Load() != 1 {
		t.Fatalf("assignment completion state unknown=%t writes=%d", draft.unknown, writes.Load())
	}
	stored, err := fixture.store.Task(context.Background(), "demo", fixture.first.ID)
	if err != nil || stored.Task.OwnerOrigin == nil || *stored.Task.OwnerOrigin != identity.Origin {
		t.Fatalf("stored assignment=%+v err=%v", stored, err)
	}
}

func TestBrowserUnknownAssignmentPersistsAcrossNavigationUntilRefresh(t *testing.T) {
	var writes atomic.Int32
	fixture, identity := prepareAssignmentFixture(t, func(api http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/assign") {
				writes.Add(1)
				recorder := httptest.NewRecorder()
				api.ServeHTTP(recorder, r)
				http.Error(w, "acknowledgement lost", http.StatusServiceUnavailable)
				return
			}
			api.ServeHTTP(w, r)
		})
	})
	b, a := fixture.browser, fixture.browser.taskAssignment
	a.target.SetSelected(a.target.Options[0])
	fynetest.Tap(a.assign)
	draft := a.drafts[taskEditorKey("demo", fixture.first.ID)]
	waitBrowser(t, fixture.callbacks, func() bool { return draft.unknown && !draft.inFlight })
	b.selectTask(fixture.second.ID)
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.second.ID })
	b.selectTask(fixture.first.ID)
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.first.ID })
	if !draft.unknown {
		t.Fatal("returning to a task cleared its unknown assignment outcome")
	}
	a.submit()
	if writes.Load() != 1 {
		t.Fatalf("unknown assignment was resubmitted before refresh: %d writes", writes.Load())
	}
	fynetest.Tap(a.refresh)
	waitBrowser(t, fixture.callbacks, func() bool { return !draft.unknown && !draft.resolving })
	if draft.task.OwnerOrigin == nil || *draft.task.OwnerOrigin != identity.Origin || writes.Load() != 1 {
		t.Fatalf("refresh did not reconcile assignment: owner=%+v writes=%d", draft.task.OwnerOrigin, writes.Load())
	}
}
