package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fynetest "fyne.io/fyne/v2/test"
	"github.com/google/uuid"
	"github.com/nitecon/amux/storage"
)

func TestBrowserWorkspaceAddSwitchClosePersistsProjectBinding(t *testing.T) {
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler { return next })
	b := fixture.browser
	prefsPath := filepath.Join(t.TempDir(), "navigation.json")
	b.prefsPath = prefsPath
	if err := fixture.store.InsertProject(context.Background(), storage.Project{Ident: "beta", ChannelName: "beta", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	b.Refresh()
	waitBrowser(t, fixture.callbacks, func() bool { return projectExists(b.projectItems, "beta") })

	first := b.activeWorkspaceID
	manager := &fakeWorkerPanelManager{snapshot: WorkerManagerSnapshot{State: WorkerManagerIdle}}
	b.workerPanel = newWorkerPanel(b, manager)
	firstCheckout := filepath.Join(t.TempDir(), "demo-checkout")
	b.selection.CheckoutPath = firstCheckout
	b.workerPanel.checkout.SetText(firstCheckout)
	b.persist()
	b.projectSearch.SetText("demo")
	b.taskSearch.SetText("First")
	b.statusSelect.SetSelected("Queued")
	fynetest.Tap(b.workspaceAdd)
	second := b.activeWorkspaceID
	if first == second || len(b.workspaceIDs) != 2 {
		t.Fatalf("workspace add did not allocate stable identity: ids=%v active=%q", b.workspaceIDs, second)
	}
	b.projectSearch.SetText("beta")
	b.taskSearch.SetText("Beta task")
	b.statusSelect.SetSelected("All")
	for i, item := range b.visibleProjects {
		if item.Ident == "beta" {
			b.projects.Select(i)
			break
		}
	}
	if b.selectedProject != "beta" || b.prefs.Selections[workspaceSelectionKey(b.prefKey, second)].Project != "beta" {
		t.Fatalf("project selector did not bind beta to the active workspace: selected=%q prefs=%+v", b.selectedProject, b.prefs.Selections)
	}
	secondCheckout := filepath.Join(t.TempDir(), "beta-checkout")
	b.selection.CheckoutPath = secondCheckout
	b.persist()

	selectWorkspaceID(t, b, first)
	if b.selectedProject != "demo" || b.projectSearch.Text != "demo" || b.taskSearch.Text != "First" || b.statusSelect.Selected != "Queued" || b.workerPanel.checkout.Text != firstCheckout {
		t.Fatalf("switch did not restore first workspace selection: project=%q project-filter=%q task-filter=%q status=%q", b.selectedProject, b.projectSearch.Text, b.taskSearch.Text, b.statusSelect.Selected)
	}
	selectWorkspaceID(t, b, second)
	if b.selectedProject != "beta" || b.projectSearch.Text != "beta" || b.taskSearch.Text != "Beta task" || b.statusSelect.Selected != "All" || b.workerPanel.checkout.Text != secondCheckout {
		t.Fatalf("switch did not restore second workspace selection: project=%q project-filter=%q task-filter=%q status=%q", b.selectedProject, b.projectSearch.Text, b.taskSearch.Text, b.statusSelect.Selected)
	}
	b.projectSearch.SetText("")
	for i, item := range b.visibleProjects {
		if item.Ident == "demo" {
			b.projects.Select(i)
			break
		}
	}
	if b.selection.CheckoutPath != "" || b.prefs.Selections[workspaceSelectionKey(b.prefKey, second)].CheckoutPath != "" || b.workerPanel.checkout.Text != "" {
		t.Fatalf("changing project retained the previous checkout binding: selection=%+v", b.selection)
	}
	fynetest.Tap(b.workspaceClose)
	if len(b.workspaceIDs) != 1 || b.workspaceIDs[0] != first || b.activeWorkspaceID != first || b.selectedProject != "demo" {
		t.Fatalf("close did not select the surviving workspace: ids=%v active=%q project=%q", b.workspaceIDs, b.activeWorkspaceID, b.selectedProject)
	}

	b.Close()
	b.Wait()
	reopened, err := NewBrowser(BrowserConfig{Client: b.client, PreferencesPath: prefsPath, Dispatch: fixture.callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close(); reopened.Wait() })
	if len(reopened.workspaceIDs) != 1 || reopened.activeWorkspaceID != first || reopened.selection.Project != "demo" {
		t.Fatalf("workspace state did not survive browser recreation: ids=%v active=%q selection=%+v", reopened.workspaceIDs, reopened.activeWorkspaceID, reopened.selection)
	}
}

func TestBrowserWorkspaceCloseBlocksPendingAndUnconfirmedCreate(t *testing.T) {
	t.Run("pending", func(t *testing.T) {
		gate := newCommittedTaskResponse(t, http.MethodPost, "/v1/projects/demo/tasks")
		fixture := startEditorBrowser(t, gate.wrap)
		b, c := fixture.browser, fixture.browser.taskComposer
		workspaceID := b.activeWorkspaceID
		c.createTitle.SetText("Pending task receipt")
		fynetest.Tap(c.createSubmit)
		gate.await(t)
		fynetest.Tap(b.workspaceClose)
		if !b.hasWorkspace(workspaceID) || !strings.Contains(b.browserStatus.Text, "pending") {
			t.Fatal("pending create did not block workspace close with an explanation")
		}
		gate.unblock()
		waitBrowser(t, fixture.callbacks, func() bool { return c.createDraft("demo").state == taskCreateAccepted })
	})

	t.Run("unconfirmed", func(t *testing.T) {
		fixture := startEditorBrowser(t, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/v1/projects/demo/tasks" {
					result := httptest.NewRecorder()
					next.ServeHTTP(result, r)
					http.Error(w, "acknowledgement lost", http.StatusServiceUnavailable)
					return
				}
				next.ServeHTTP(w, r)
			})
		})
		b, c := fixture.browser, fixture.browser.taskComposer
		workspaceID := b.activeWorkspaceID
		c.createTitle.SetText("Unconfirmed task receipt")
		fynetest.Tap(c.createSubmit)
		waitBrowser(t, fixture.callbacks, func() bool { return c.createDraft("demo").state == taskCreateUnknown })
		fynetest.Tap(b.workspaceClose)
		if !b.hasWorkspace(workspaceID) || !strings.Contains(b.browserStatus.Text, "unconfirmed") {
			t.Fatal("unconfirmed create did not block workspace close with an explanation")
		}
	})
}

func TestBrowserCloseInactivePeerKeepsActiveTaskDetailCallback(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var hold atomic.Bool
	var held atomic.Bool
	const responseTitle = "Updated by the pending detail response"
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			isDetail := r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/projects/demo/tasks/") && !strings.HasSuffix(r.URL.Path, "/conversations")
			if !isDetail || !hold.Load() || !held.CompareAndSwap(false, true) {
				next.ServeHTTP(w, r)
				return
			}
			result := httptest.NewRecorder()
			next.ServeHTTP(result, r)
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(result.Body.Bytes(), &payload); err != nil {
				http.Error(w, "invalid synthetic task response", http.StatusInternalServerError)
				return
			}
			updatedTitle, _ := json.Marshal(responseTitle)
			payload["title"] = updatedTitle
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(result.Code)
			_ = json.NewEncoder(w).Encode(payload)
		})
	})
	t.Cleanup(unblock)
	b := fixture.browser
	activeWorkspace := b.activeWorkspaceID
	fynetest.Tap(b.workspaceAdd)
	peerWorkspace := b.activeWorkspaceID
	if peerWorkspace == activeWorkspace {
		t.Fatal("workspace add did not create a peer")
	}
	selectWorkspaceID(t, b, activeWorkspace)
	waitBrowser(t, fixture.callbacks, func() bool {
		return b.activeWorkspaceID == activeWorkspace && !b.detailLoading && b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.first.ID
	})

	hold.Store(true)
	b.refreshTaskDetail()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("active task detail request did not start")
	}
	requestGeneration := b.taskGen
	if !b.detailLoading {
		t.Fatal("active task detail request was not marked in flight")
	}
	peerLabel := ""
	for label, id := range b.workspacePeerOptionIDs {
		if id == peerWorkspace {
			peerLabel = label
			break
		}
	}
	if peerLabel == "" {
		t.Fatalf("inactive peer %q is absent from close-peer options: %v", peerWorkspace, b.workspacePeerOptionIDs)
	}
	b.workspacePeerSelect.SetSelected(peerLabel)
	if b.workspacePeerClose.Disabled() {
		t.Fatal("close-peer action did not enable for selected inactive workspace")
	}
	fynetest.Tap(b.workspacePeerClose)
	if b.activeWorkspaceID != activeWorkspace || !b.hasWorkspace(activeWorkspace) || b.hasWorkspace(peerWorkspace) || b.taskGen != requestGeneration || !b.detailLoading {
		t.Fatalf("peer close disturbed active task request: active=%q ids=%v generation=%d/%d loading=%v", b.activeWorkspaceID, b.workspaceIDs, b.taskGen, requestGeneration, b.detailLoading)
	}
	unblock()
	waitBrowser(t, fixture.callbacks, func() bool {
		return !b.detailLoading && b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.Title == responseTitle
	})
	if b.activeWorkspaceID != activeWorkspace || b.selectedProject != "demo" || b.selectedTask != fixture.first.ID || b.taskTitle.Text != responseTitle || !strings.HasPrefix(b.taskMeta.Text, "Queued") {
		t.Fatalf("active detail callback did not settle after peer close: active=%q project=%q task=%q detail=%+v title=%q", b.activeWorkspaceID, b.selectedProject, b.selectedTask, b.selectedTaskDetail, b.taskTitle.Text)
	}
}

func TestBrowserLateCreateReceiptUpdatesOriginWorkspace(t *testing.T) {
	gate := newCommittedTaskResponse(t, http.MethodPost, "/v1/projects/demo/tasks")
	fixture := startEditorBrowser(t, gate.wrap)
	b, c := fixture.browser, fixture.browser.taskComposer
	if err := fixture.store.InsertProject(context.Background(), storage.Project{Ident: "beta", ChannelName: "beta", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	b.Refresh()
	waitBrowser(t, fixture.callbacks, func() bool { return projectExists(b.projectItems, "beta") })
	originWorkspace := b.activeWorkspaceID
	c.createTitle.SetText("Create in origin workspace")
	fynetest.Tap(c.createSubmit)
	gate.await(t)
	b.selectProject("beta")
	if b.activeWorkspaceID != originWorkspace || b.selectedProject != "beta" || c.receiptDraft(originWorkspace).state != taskCreatePending {
		t.Fatal("rebinding the originating workspace did not preserve its pending receipt")
	}
	fynetest.Tap(b.workspaceAdd)
	otherWorkspace := b.activeWorkspaceID
	for i, item := range b.visibleProjects {
		if item.Ident == "beta" {
			b.projects.Select(i)
			break
		}
	}
	c.createTitle.SetText("Independent beta draft")
	gate.unblock()
	originDraft := c.createDraftFor(originWorkspace, "demo")
	waitBrowser(t, fixture.callbacks, func() bool { return originDraft.state == taskCreateAccepted && !originDraft.inFlight })
	otherDraft := c.createDraftFor(otherWorkspace, "beta")
	if b.activeWorkspaceID != otherWorkspace || b.selectedProject != "beta" || otherDraft.title != "Independent beta draft" || otherDraft.state != taskCreateReady {
		t.Fatalf("late receipt retargeted the active workspace: active=%q project=%q origin=%+v other=%+v", b.activeWorkspaceID, b.selectedProject, originDraft, otherDraft)
	}
	selectWorkspaceID(t, b, originWorkspace)
	if b.selectedProject != "beta" || originDraft.createdTaskID == "" || c.receiptDraft(originWorkspace) != originDraft || !strings.Contains(c.createStatus.Text, originDraft.createdTaskID) {
		t.Fatal("origin workspace did not retain its accepted receipt")
	}
}

func TestBrowserDirectoryCallbackSurvivesClosingPeerWorkspace(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var hold atomic.Bool
	var targetReads atomic.Int32
	now := time.Now().UnixMilli()
	target := storage.RegisteredTarget{
		Target: storage.RegisteredTargetIdentity{Origin: storage.AgentOrigin{
			SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux",
		}, Generation: uuid.NewString()},
		SeenAt: now, ExpiresAt: now + 60000,
	}
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/projects/demo/task-targets" && hold.Load() {
				targetReads.Add(1)
				select {
				case <-entered:
				default:
					close(entered)
				}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(storage.TargetDirectory{Targets: []storage.RegisteredTarget{target}})
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	t.Cleanup(unblock)
	b, c := fixture.browser, fixture.browser.taskComposer
	originWorkspace := b.activeWorkspaceID
	waitBrowser(t, fixture.callbacks, func() bool {
		draft := c.createDraftFor(originWorkspace, "demo")
		assignment := b.taskAssignment.currentDraft()
		return !draft.targetsLoading && draft.targetsLoaded &&
			assignment != nil && assignment.targetsLoaded && !assignment.loading
	})
	hold.Store(true)
	c.loadRoutingTargets()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("target directory request did not start")
	}
	fynetest.Tap(b.workspaceAdd)
	peerWorkspace := b.activeWorkspaceID
	if peerWorkspace == originWorkspace {
		t.Fatal("workspace add did not switch to the peer")
	}
	fynetest.Tap(b.workspaceClose)
	if b.activeWorkspaceID != originWorkspace || !b.hasWorkspace(originWorkspace) || b.hasWorkspace(peerWorkspace) {
		t.Fatalf("closing peer did not restore surviving workspace: active=%q ids=%v", b.activeWorkspaceID, b.workspaceIDs)
	}
	if targetReads.Load() != 1 {
		t.Fatalf("switching back duplicated the pending target read: %d", targetReads.Load())
	}
	unblock()
	waitBrowser(t, fixture.callbacks, func() bool {
		draft := c.createDraftFor(originWorkspace, "demo")
		return !draft.targetsLoading && draft.targetsLoaded && len(draft.targets) == 1
	})
	if c.createDraftFor(originWorkspace, "demo").targets[0].Target != target.Target {
		t.Fatal("directory callback did not retain the exact target for the surviving workspace")
	}
}

func selectWorkspaceID(t *testing.T, b *Browser, id string) {
	t.Helper()
	for label, candidate := range b.workspaceOptionIDs {
		if candidate == id {
			b.workspaceSelect.SetSelected(label)
			if b.activeWorkspaceID != id {
				t.Fatalf("workspace selector did not activate %q", id)
			}
			return
		}
	}
	t.Fatalf("workspace %q is absent from selector options", id)
}
