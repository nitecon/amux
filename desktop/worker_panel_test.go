package desktop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/storage"
)

type fakeWorkerPanelManager struct {
	mu            sync.Mutex
	snapshot      WorkerManagerSnapshot
	createStarted chan storage.WorkerPool
	createRelease chan struct{}
	resumeStarted chan string
	resumeErr     error
}

func (m *fakeWorkerPanelManager) Start(context.Context) error { return nil }
func (m *fakeWorkerPanelManager) CreateAndStartPool(pool storage.WorkerPool) (*storage.WorkerPool, error) {
	if m.createStarted != nil {
		m.createStarted <- pool
	}
	if m.createRelease != nil {
		<-m.createRelease
	}
	m.mu.Lock()
	m.snapshot.Generation++
	m.snapshot.State = WorkerManagerRunning
	m.snapshot.Pool = &pool
	m.mu.Unlock()
	copy := pool
	return &copy, nil
}
func (m *fakeWorkerPanelManager) ResumePool(id string) error {
	if m.resumeStarted != nil {
		m.resumeStarted <- id
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot.Generation++
	if m.resumeErr != nil {
		m.snapshot.State = WorkerManagerFailed
		m.snapshot.Pool = nil
		m.snapshot.Error = m.resumeErr.Error()
		return m.resumeErr
	}
	return nil
}
func (m *fakeWorkerPanelManager) Snapshot() WorkerManagerSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot := m.snapshot
	if snapshot.Pool != nil {
		pool := *snapshot.Pool
		snapshot.Pool = &pool
	}
	return snapshot
}
func (m *fakeWorkerPanelManager) Stop() {
	m.mu.Lock()
	m.snapshot.State = WorkerManagerStopped
	m.mu.Unlock()
}
func (m *fakeWorkerPanelManager) Close() error { return nil }
func (m *fakeWorkerPanelManager) Wait() error  { return nil }

func newWorkerPanelTestBrowser(t *testing.T, manager workerPanelController, ctx context.Context, dispatch func(func())) *Browser {
	t.Helper()
	b := &Browser{
		endpoint: "https://gateway.test", prefKey: "https://gateway.test",
		selectedProject: "demo", started: true, ctx: ctx, dispatch: dispatch,
		browserStatus: widget.NewLabel("Ready"), prefs: emptyNavigationPreferences(),
		selectedProjectFavorite: widget.NewButton("Favorite", nil),
	}
	b.workerPanel = newWorkerPanel(b, manager)
	b.workerPanel.started = true
	return b
}

func TestWorkerPanelRestoresSavedPoolForEndpoint(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	manager := &fakeWorkerPanelManager{snapshot: WorkerManagerSnapshot{State: WorkerManagerIdle}}
	b := newWorkerPanelTestBrowser(t, manager, context.Background(), func(fn func()) { fn() })
	b.prefs.WorkerPools = map[string]string{"https://gateway.test": "saved-pool"}
	p := newWorkerPanel(b, manager)
	if p.savedID.Text != "saved-pool" {
		t.Fatalf("restored pool ID = %q, want saved-pool", p.savedID.Text)
	}
	if len(p.slots.Options) != 32 || p.slots.Options[0] != "1" || p.slots.Options[31] != "32" {
		t.Fatal("worker slots should offer exactly 1 through 32")
	}
}

func TestWorkerPanelDiscoverySelectionOnlyFillsCheckoutField(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	parent := t.TempDir()
	checkout := filepath.Join(parent, "existing checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", checkout, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("initialize fixture checkout: %s: %v", output, err)
	}
	nested := filepath.Join(parent, "nested", "checkout")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", nested, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("initialize nested fixture checkout: %s: %v", output, err)
	}
	configPath := filepath.Join(checkout, ".git", "config")
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	callbacks := newUICallbacks()
	ctx, cancel := context.WithCancel(context.Background())
	manager := &fakeWorkerPanelManager{snapshot: WorkerManagerSnapshot{State: WorkerManagerIdle}}
	b := newWorkerPanelTestBrowser(t, manager, ctx, callbacks.dispatch)
	t.Cleanup(func() {
		cancel()
		b.Close()
		callbacks.pump()
		b.Wait()
	})
	p := b.workerPanel
	p.discoveryParent.SetText(parent)
	p.discover.OnTapped()
	waitBrowser(t, callbacks, func() bool { return len(p.checkouts.Options) == 1 })
	if p.checkout.Text != "" {
		t.Fatal("discovery changed the checkout field before selection")
	}
	resolvedCheckout, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if p.checkouts.Options[0] != resolvedCheckout {
		t.Fatalf("discovered options=%v, want only immediate checkout %q", p.checkouts.Options, resolvedCheckout)
	}
	p.checkouts.SetSelected(resolvedCheckout)
	if p.checkout.Text != resolvedCheckout {
		t.Fatalf("selected checkout field=%q, want %q", p.checkout.Text, resolvedCheckout)
	}
	if snapshot := manager.Snapshot(); snapshot.State != WorkerManagerIdle || snapshot.Pool != nil {
		t.Fatalf("selection started or mutated a worker pool: %+v", snapshot)
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil || string(configAfter) != string(configBefore) {
		t.Fatalf("discovery or selection modified checkout config: err=%v", err)
	}
}

func TestWorkspaceCheckoutAttachmentRequiresGitAndExactProjectBinding(t *testing.T) {
	for _, tc := range []struct {
		name, projectKind string
		projectRemote     *string
		checkoutRemote    string
		wantAttached      bool
	}{
		{name: "exact canonical remote", projectKind: "repo", projectRemote: stringPtrDesktop("https://github.com/example/demo.git"), checkoutRemote: "git@github.com:example/demo.git", wantAttached: true},
		{name: "mismatched remote", projectKind: "repo", projectRemote: stringPtrDesktop("github.com/example/demo"), checkoutRemote: "https://github.com/other/repo.git"},
		{name: "adhoc without origin", projectKind: "adhoc", checkoutRemote: "", wantAttached: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := fynetest.NewApp()
			defer app.Quit()
			checkout := filepath.Join(t.TempDir(), "checkout")
			if err := os.MkdirAll(checkout, 0o755); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("git", "-C", checkout, "init", "--quiet").CombinedOutput(); err != nil {
				t.Fatalf("initialize checkout: %s: %v", output, err)
			}
			if tc.checkoutRemote != "" {
				if output, err := exec.Command("git", "-C", checkout, "remote", "add", "origin", tc.checkoutRemote).CombinedOutput(); err != nil {
					t.Fatalf("set checkout origin: %s: %v", output, err)
				}
			}
			callbacks := newUICallbacks()
			ctx, cancel := context.WithCancel(context.Background())
			manager := &fakeWorkerPanelManager{snapshot: WorkerManagerSnapshot{State: WorkerManagerIdle}}
			b := newWorkerPanelTestBrowser(t, manager, ctx, callbacks.dispatch)
			b.prefsPath = filepath.Join(t.TempDir(), "navigation.json")
			b.activeWorkspaceID = "workspace-1"
			b.workspaceIDs = []string{"workspace-1"}
			b.workspaceBindingGeneration = map[string]uint64{"workspace-1": 1}
			b.selection = NavigationSelection{Project: "demo", Status: "all"}
			b.prefs.Selections = map[string]NavigationSelection{}
			b.projectItems = []storage.ProjectOverview{{Project: storage.Project{Ident: "demo", Kind: tc.projectKind, CanonicalRemote: tc.projectRemote}}}
			t.Cleanup(func() { cancel(); b.Close(); callbacks.pump(); b.Wait() })

			p := b.workerPanel
			p.checkout.SetText(checkout)
			p.attachCheckoutToWorkspace()
			wantStatus := "Checkout was not bound:"
			if tc.wantAttached {
				wantStatus = "Existing checkout attached"
			}
			waitBrowser(t, callbacks, func() bool { return strings.Contains(p.discoveryStatus.Text, wantStatus) })
			saved := b.prefs.Selections[workspaceSelectionKey(b.prefKey, "workspace-1")]
			if tc.wantAttached {
				resolved, err := filepath.EvalSymlinks(checkout)
				if err != nil || saved.CheckoutPath != resolved || p.checkout.Text != resolved {
					t.Fatalf("attached path=%q field=%q, want %q (err=%v)", saved.CheckoutPath, p.checkout.Text, resolved, err)
				}
				if tc.checkoutRemote == "" && saved.CheckoutOrigin != nil || tc.checkoutRemote != "" && (saved.CheckoutOrigin == nil || *saved.CheckoutOrigin != tc.checkoutRemote) {
					t.Fatalf("attached origin=%v, want %q", saved.CheckoutOrigin, tc.checkoutRemote)
				}
				reloaded, err := LoadNavigationPreferences(b.prefsPath)
				if err != nil {
					t.Fatal(err)
				}
				if restored := reloaded.Selections[workspaceSelectionKey(b.prefKey, "workspace-1")]; restored.CheckoutPath != resolved || restored.Project != "demo" {
					t.Fatalf("reloaded workspace binding = %+v", restored)
				}
			} else if saved.CheckoutPath != "" {
				t.Fatalf("mismatched checkout was persisted: %+v", saved)
			}
			if snapshot := manager.Snapshot(); snapshot.State != WorkerManagerIdle || snapshot.Pool != nil {
				t.Fatalf("attaching checkout started a worker: %+v", snapshot)
			}
		})
	}
}

func TestWorkerPanelExplicitCloneUsesLocalFixtureAndBindsOnlyMatchingProject(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      string
		remote    *string
		wantBound bool
	}{
		{name: "adhoc local source", kind: "adhoc", wantBound: true},
		{name: "repository binding mismatch", kind: "repo", remote: stringPtrDesktop("github.com/example/expected")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := fynetest.NewApp()
			defer app.Quit()
			root := t.TempDir()
			source := filepath.Join(root, "fixture-source.git")
			if output, err := exec.Command("git", "init", "--bare", "--initial-branch=main", source).CombinedOutput(); err != nil {
				t.Fatalf("initialize local bare source: %s: %v", output, err)
			}
			parent := filepath.Join(root, "projects")
			if err := os.MkdirAll(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			callbacks := newUICallbacks()
			ctx, cancel := context.WithCancel(context.Background())
			manager := &fakeWorkerPanelManager{snapshot: WorkerManagerSnapshot{State: WorkerManagerIdle}}
			b := newWorkerPanelTestBrowser(t, manager, ctx, callbacks.dispatch)
			b.activeWorkspaceID = "workspace-1"
			b.workspaceIDs = []string{"workspace-1"}
			b.workspaceBindingGeneration = map[string]uint64{"workspace-1": 1}
			b.selection = NavigationSelection{Project: "demo", Status: "all"}
			b.prefs.Selections = map[string]NavigationSelection{}
			b.projectItems = []storage.ProjectOverview{{Project: storage.Project{Ident: "demo", Kind: tc.kind, CanonicalRemote: tc.remote}}}
			t.Cleanup(func() { cancel(); b.Close(); callbacks.pump(); b.Wait() })

			p := b.workerPanel
			p.discoveryParent.SetText(parent)
			p.cloneSource.SetText(source)
			clonePath := filepath.Join(parent, "fixture-source")
			if _, err := os.Stat(clonePath); !os.IsNotExist(err) {
				t.Fatalf("clone happened before explicit action: stat err=%v", err)
			}
			p.cloneCheckoutForWorkspace()
			wantStatus := "Checkout was not bound:"
			if tc.wantBound {
				wantStatus = "Clone completed and checkout attached"
			}
			waitBrowser(t, callbacks, func() bool { return strings.Contains(p.discoveryStatus.Text, wantStatus) })
			if tc.wantBound {
				resolved, err := filepath.EvalSymlinks(clonePath)
				if err != nil {
					t.Fatal(err)
				}
				if got := b.prefs.Selections[workspaceSelectionKey(b.prefKey, "workspace-1")].CheckoutPath; got != resolved {
					t.Fatalf("persisted checkout=%q, want %q", got, resolved)
				}
				if got := b.prefs.Selections[workspaceSelectionKey(b.prefKey, "workspace-1")].CheckoutOrigin; got == nil || *got != source {
					t.Fatalf("persisted clone origin=%v, want actual local origin %q", got, source)
				}
			} else if got := b.prefs.Selections[workspaceSelectionKey(b.prefKey, "workspace-1")].CheckoutPath; got != "" {
				t.Fatalf("mismatched clone was bound: %q", got)
			}
			if snapshot := manager.Snapshot(); snapshot.State != WorkerManagerIdle || snapshot.Pool != nil {
				t.Fatalf("clone action started a worker: %+v", snapshot)
			}
		})
	}
}

func TestMissingWorkspaceCheckoutOffersOnlyMatchingReportedMachineOrigins(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	callbacks := newUICallbacks()
	ctx, cancel := context.WithCancel(context.Background())
	manager := &fakeWorkerPanelManager{snapshot: WorkerManagerSnapshot{State: WorkerManagerIdle}}
	b := newWorkerPanelTestBrowser(t, manager, ctx, callbacks.dispatch)
	b.activeWorkspaceID = "workspace-1"
	b.workspaceIDs = []string{"workspace-1"}
	b.workspaceBindingGeneration = map[string]uint64{"workspace-1": 3}
	missing := filepath.Join(t.TempDir(), "removed-checkout")
	origin := "https://github.com/example/demo.git"
	wrongOrigin := "https://github.com/other/project.git"
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	b.selection = NavigationSelection{Project: "demo", CheckoutPath: missing, CheckoutOrigin: &origin}
	b.prefs.Selections = map[string]NavigationSelection{}
	b.projectItems = []storage.ProjectOverview{{Project: storage.Project{Ident: "demo", Kind: "repo", CanonicalRemote: stringPtrDesktop("github.com/example/demo")}}}
	b.checkoutItems = []storage.CheckoutOverview{{
		Ident: "demo", Machines: []storage.MachineCheckout{{Hostname: hostname, Origin: &origin}, {Hostname: "host-a", Origin: &origin}, {Hostname: "host-b", Origin: &origin}, {Hostname: "host-c", Origin: &wrongOrigin}},
	}}
	t.Cleanup(func() { cancel(); b.Close(); callbacks.pump(); b.Wait() })

	p := b.workerPanel
	p.restoreWorkspaceCheckout()
	if len(p.recoveryOrigins.Options) != 1 || !strings.Contains(p.recoveryOrigins.Options[0], "host-a") || p.cloneSource.Text != "" {
		t.Fatalf("recovery options=%v source=%q", p.recoveryOrigins.Options, p.cloneSource.Text)
	}
	report, err := machineCheckoutReport(context.Background(), b.machineCheckoutBindings())
	if err != nil || len(report) != 0 {
		t.Fatalf("missing checkout must not be reported from this machine: report=%+v err=%v", report, err)
	}
	p.recoveryOrigins.SetSelected(p.recoveryOrigins.Options[0])
	if p.cloneSource.Text != origin || !strings.Contains(p.discoveryStatus.Text, "Choose Clone checkout") {
		t.Fatalf("selection did not seed explicit recovery source: %q status=%q", p.cloneSource.Text, p.discoveryStatus.Text)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("selecting reported origin created checkout: %v", err)
	}
}

func TestMissingCheckoutRecoveryClonesSelectedMachineOriginAndPersistsBinding(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	root := t.TempDir()
	source := filepath.Join(root, "fixture-source.git")
	if output, err := exec.Command("git", "init", "--bare", "--initial-branch=main", source).CombinedOutput(); err != nil {
		t.Fatalf("initialize local bare source: %s: %v", output, err)
	}
	parent := filepath.Join(root, "projects")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(parent, "previous-checkout")
	origin := source
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	ctx, cancel := context.WithCancel(context.Background())
	manager := &fakeWorkerPanelManager{snapshot: WorkerManagerSnapshot{State: WorkerManagerIdle}}
	b := newWorkerPanelTestBrowser(t, manager, ctx, callbacks.dispatch)
	b.activeWorkspaceID = "workspace-1"
	b.workspaceIDs = []string{"workspace-1"}
	b.workspaceBindingGeneration = map[string]uint64{"workspace-1": 9}
	b.selection = NavigationSelection{Project: "adhoc-project", CheckoutPath: missing}
	b.prefs.Selections = map[string]NavigationSelection{}
	b.projectItems = []storage.ProjectOverview{{Project: storage.Project{Ident: "adhoc-project", Kind: "adhoc"}}}
	b.checkoutItems = []storage.CheckoutOverview{{
		Ident:    "adhoc-project",
		Machines: []storage.MachineCheckout{{Hostname: hostname + "-other", Origin: &origin}},
	}}
	b.prefsPath = filepath.Join(t.TempDir(), "navigation.json")
	t.Cleanup(func() { cancel(); b.Close(); callbacks.pump(); b.Wait() })

	p := b.workerPanel
	p.discoveryParent.SetText(parent)
	savedPrefs, err := LoadNavigationPreferences(b.prefsPath)
	if err != nil || savedPrefs.ProjectsDirectories[b.prefKey] != parent {
		t.Fatalf("projects directory was not saved: prefs=%+v err=%v", savedPrefs.ProjectsDirectories, err)
	}
	p = newWorkerPanel(b, manager)
	b.workerPanel = p
	if p.discoveryParent.Text != parent {
		t.Fatalf("restored projects directory=%q, want %q", p.discoveryParent.Text, parent)
	}
	p.restoreWorkspaceCheckout()
	if len(p.recoveryOrigins.Options) != 1 || p.cloneSource.Text != "" {
		t.Fatalf("recovery sources=%v clone source=%q", p.recoveryOrigins.Options, p.cloneSource.Text)
	}
	p.recoveryOrigins.SetSelected(p.recoveryOrigins.Options[0])
	if p.cloneSource.Text != source {
		t.Fatalf("selected source=%q, want reported actual origin %q", p.cloneSource.Text, source)
	}
	if _, err := os.Stat(filepath.Join(parent, "fixture-source")); !os.IsNotExist(err) {
		t.Fatalf("selecting source cloned before explicit action: %v", err)
	}
	p.cloneCheckoutForWorkspace()
	waitBrowser(t, callbacks, func() bool { return strings.Contains(p.discoveryStatus.Text, "Clone completed and checkout attached") })

	cloned, err := filepath.EvalSymlinks(filepath.Join(parent, "fixture-source"))
	if err != nil {
		t.Fatal(err)
	}
	key := workspaceSelectionKey(b.prefKey, "workspace-1")
	if b.selection.Project != "adhoc-project" || b.selection.CheckoutPath != cloned || b.prefs.Selections[key].Project != "adhoc-project" || b.prefs.Selections[key].CheckoutPath != cloned {
		t.Fatalf("recovery changed or lost workspace binding: selection=%+v saved=%+v", b.selection, b.prefs.Selections[key])
	}
	if b.prefs.Selections[key].CheckoutOrigin == nil || *b.prefs.Selections[key].CheckoutOrigin != source {
		t.Fatalf("recovered raw clone origin=%v, want %q", b.prefs.Selections[key].CheckoutOrigin, source)
	}
	reloaded, err := LoadNavigationPreferences(b.prefsPath)
	if err != nil || reloaded.Selections[key].Project != "adhoc-project" || reloaded.Selections[key].CheckoutPath != cloned || reloaded.ProjectsDirectories[b.prefKey] != parent {
		t.Fatalf("recovery preferences did not round-trip: %+v err=%v", reloaded, err)
	}
	if snapshot := manager.Snapshot(); snapshot.State != WorkerManagerIdle || snapshot.Pool != nil {
		t.Fatalf("recovery started a worker: %+v", snapshot)
	}
}

func TestWorkerPanelCapturesCreateScopeAndPersistsReceiptWithoutReplacingDraft(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	callbacks := newUICallbacks()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := &fakeWorkerPanelManager{
		snapshot:      WorkerManagerSnapshot{State: WorkerManagerIdle},
		createStarted: make(chan storage.WorkerPool, 1), createRelease: make(chan struct{}),
	}
	b := newWorkerPanelTestBrowser(t, manager, ctx, callbacks.dispatch)
	b.workerPanel.checkout.SetText(t.TempDir())
	b.workerPanel.provider.SetSelected("claude")
	b.workerPanel.slots.SetSelected("2")
	b.workerPanel.goal.SetText("review-goal")
	p := b.workerPanel
	p.startPool()
	var request storage.WorkerPool
	select {
	case request = <-manager.createStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("worker manager did not receive create request")
	}
	if request.Project != "demo" || request.Provider != "claude" || request.SlotCount != 2 || request.Goal == nil || *request.Goal != "review-goal" {
		t.Fatalf("create request did not capture panel values: %#v", request)
	}
	b.mu.Lock()
	b.selectedProject = "another-project"
	b.mu.Unlock()
	p.savedID.SetText("newer saved ID draft")
	close(manager.createRelease)
	waitBrowser(t, callbacks, func() bool { return !p.busy && manager.Snapshot().State == WorkerManagerRunning })
	if b.prefs.WorkerPools[b.prefKey] != request.ID {
		t.Fatal("successful create receipt was not saved by endpoint")
	}
	if p.savedID.Text != "newer saved ID draft" {
		t.Fatal("create receipt erased a newer saved-ID edit")
	}
	if p.identity.Text == "" || p.identity.Text == request.Project {
		t.Fatal("saved worker identity was not displayed")
	}

	p.stopPool()
	waitBrowser(t, callbacks, func() bool { return !p.busy && manager.Snapshot().State == WorkerManagerStopped })
	if b.prefs.WorkerPools[b.prefKey] != request.ID {
		t.Fatal("stop unexpectedly replaced the saved pool selection")
	}
}

func TestWorkerPanelFailedResumeDoesNotPersistUnverifiedID(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	callbacks := newUICallbacks()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := &fakeWorkerPanelManager{
		snapshot:      WorkerManagerSnapshot{State: WorkerManagerIdle},
		resumeStarted: make(chan string, 1), resumeErr: errors.New("saved pool not found"),
	}
	b := newWorkerPanelTestBrowser(t, manager, ctx, callbacks.dispatch)
	b.prefs.WorkerPools = map[string]string{b.prefKey: "previous-saved-pool"}
	p := newWorkerPanel(b, manager)
	p.started = true
	p.savedID.SetText("misspelled-pool")
	p.resumePool()
	select {
	case got := <-manager.resumeStarted:
		if got != "misspelled-pool" {
			t.Fatalf("resume used %q, want the captured ID", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resume was not requested")
	}
	waitBrowser(t, callbacks, func() bool { return !p.busy })
	if b.prefs.WorkerPools[b.prefKey] != "previous-saved-pool" {
		t.Fatal("failed resume replaced the previous saved pool selection")
	}
}

func TestBrowserWorkerPanelIsOptional(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	b, err := NewBrowser(BrowserConfig{Client: inertEditorClient(t)})
	if err != nil {
		t.Fatal(err)
	}
	if b.workerPanel != nil {
		t.Fatal("nil worker manager should preserve the browser without a worker panel")
	}
	b.Close()
}

func TestWorkerPanelWrappedIdentityRelayoutsBeforeStatus(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	manager := &fakeWorkerPanelManager{snapshot: WorkerManagerSnapshot{State: WorkerManagerIdle}}
	b := &Browser{endpoint: "https://gateway.test", prefKey: "https://gateway.test", prefs: emptyNavigationPreferences(), browserStatus: widget.NewLabel("Ready")}
	p := newWorkerPanel(b, manager)
	p.setRoot(container.NewHSplit(p.accordion, widget.NewLabel("Details")))
	window := fynetest.NewWindow(p.root)
	defer window.Close()
	window.Resize(fyne.NewSize(460, 420))
	window.Show()
	p.accordion.Open(0)
	pool := storage.WorkerPool{
		ID: "founded-1234-5678", Project: "demo", Provider: "codex", SlotCount: 2,
		Directory: "/" + strings.Repeat("long-existing-checkout-segment/", 9) + "checkout",
	}
	p.renderSnapshot(WorkerManagerSnapshot{State: WorkerManagerRunning, Pool: &pool})
	_ = window.Canvas().Capture() // Fyne capture flushes the normal container layout pass.
	identityBottom := p.identity.Position().Y + p.identity.Size().Height
	if p.identity.Size().Height+1 < p.identity.MinSize().Height {
		t.Fatalf("wrapped identity got %v height, needs %v", p.identity.Size().Height, p.identity.MinSize().Height)
	}
	if identityBottom > p.status.Position().Y+1 {
		t.Fatalf("status starts at y=%v before identity ends at y=%v", p.status.Position().Y, identityBottom)
	}
}
