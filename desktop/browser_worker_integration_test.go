package desktop

import (
	"context"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	fynetest "fyne.io/fyne/v2/test"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/providers"
	"github.com/nitecon/amux/storage"
)

func TestBrowserWorkerControlsResumeSavedScopeAndDrainOnClose(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var opened atomic.Int32
	manager, local, remote, client, _ := workerManagerFixture(t, func(context.Context, storage.WorkerRun, conversation.Sink) (providers.Session, error) {
		opened.Add(1)
		return nil, fmt.Errorf("fixture has no assigned work")
	})
	if err := remote.InsertProject(ctx, storage.Project{Ident: "other", ChannelName: "other", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	preferences := filepath.Join(t.TempDir(), "navigation.json")
	b, err := NewBrowser(BrowserConfig{Client: client, Project: "demo", PreferencesPath: preferences, WorkerManager: manager, Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(); b.Wait(); callbacks.pump() })
	b.Start(ctx)
	p := b.workerPanel
	window := app.NewWindow("Worker pool fixture")
	window.SetContent(b.Content())
	window.Resize(fyne.NewSize(1280, 820))
	window.Show()
	t.Cleanup(window.Close)
	openBrowserSettingsPage(t, b, "Workers")
	p.accordion.Open(0)
	waitBrowser(t, callbacks, func() bool { return b.selectedProject == "demo" && !p.start.Disabled() })
	directory := t.TempDir()
	p.checkout.SetText(directory)
	b.saveConversationLaunchPreferences("codex", ConversationLaunchPreferences{Model: "gpt-5.1"})
	p.provider.SetSelected("codex")
	p.slots.SetSelected("2")
	p.goal.SetText("  Release  ")
	fynetest.Tap(p.start)
	waitBrowser(t, callbacks, func() bool {
		snapshot := manager.Snapshot()
		return !p.busy && snapshot.State == WorkerManagerRunning && p.savedID.Text != ""
	})
	waitForManagerPresence(t, ctx, manager, remote)
	created := manager.Snapshot().Pool
	wantCommand, _ := providerLaunchCommand("codex", ConversationLaunchPreferences{Model: "gpt-5.1"})
	if created == nil || created.Directory != directory || created.Project != "demo" || created.Provider != "codex" || !reflect.DeepEqual(created.Command, wantCommand) || created.SlotCount != 2 || created.Goal == nil || *created.Goal != "Release" {
		t.Fatalf("wrong created scope: %+v", created)
	}
	if p.identity.Size().Height < p.identity.MinSize().Height || p.status.Position().Y < p.identity.Position().Y+p.identity.Size().Height {
		t.Fatal("wrapped saved identity overlaps the worker status")
	}
	preview, err := os.Create(filepath.Join(t.TempDir(), "amux-worker-panel-preview.png"))
	if err != nil {
		t.Fatal(err)
	}
	encodeErr := png.Encode(preview, window.Canvas().Capture())
	closeErr := preview.Close()
	if encodeErr != nil || closeErr != nil {
		t.Fatalf("capture worker panel: %v %v", encodeErr, closeErr)
	}
	beforeSlots, err := local.WorkerPoolSlots(ctx, created.ID)
	if err != nil || len(beforeSlots) != 2 {
		t.Fatalf("slots: %+v %v", beforeSlots, err)
	}
	fynetest.Tap(p.stop)
	waitBrowser(t, callbacks, func() bool { return !p.busy && manager.Snapshot().State == WorkerManagerStopped })
	// Editing the new-pool form and browsing another project must not become
	// implicit configuration for an explicit saved-pool resume.
	b.selectProject("other")
	waitBrowser(t, callbacks, func() bool { return b.selectedProject == "other" && !activeTaskListLoading(b) })
	p.checkout.SetText(t.TempDir())
	p.provider.SetSelected("claude")
	p.slots.SetSelected("1")
	p.goal.SetText("")
	p.savedID.SetText(created.ID)
	fynetest.Tap(p.resume)
	waitBrowser(t, callbacks, func() bool { return !p.busy && manager.Snapshot().State == WorkerManagerRunning })
	resumed := manager.Snapshot().Pool
	if !reflect.DeepEqual(created, resumed) || b.selectedProject != "other" {
		t.Fatalf("resume changed saved scope or retargeted browser: before=%+v after=%+v project=%q", created, resumed, b.selectedProject)
	}
	fynetest.Tap(p.stop)
	waitBrowser(t, callbacks, func() bool { return !p.busy && manager.Snapshot().State == WorkerManagerStopped })
	p.savedID.SetText("nonexistent-saved-pool")
	fynetest.Tap(p.resume)
	waitBrowser(t, callbacks, func() bool { return !p.busy && manager.Snapshot().State == WorkerManagerFailed })
	if b.prefs.WorkerPools[client.Endpoint()] != created.ID || p.savedID.Text != "nonexistent-saved-pool" {
		t.Fatal("failed resume replaced the valid saved selection or discarded the typed ID")
	}
	b.Close()
	b.Wait()
	if snapshot := manager.Snapshot(); snapshot.State != WorkerManagerFailed {
		t.Fatalf("browser shutdown did not join worker pool: %+v", snapshot)
	}
	afterSlots, err := local.WorkerPoolSlots(ctx, created.ID)
	if err != nil || len(afterSlots) != len(beforeSlots) {
		t.Fatalf("saved slots after shutdown: %+v %v", afterSlots, err)
	}
	for i := range beforeSlots {
		if beforeSlots[i].ID != afterSlots[i].ID || beforeSlots[i].Owner != afterSlots[i].Owner || beforeSlots[i].RunID != afterSlots[i].RunID {
			t.Fatal("stop/resume replaced saved slot identity or recovery state")
		}
	}
	if opened.Load() != 0 {
		t.Fatal("idle pool unexpectedly opened a provider")
	}
	callbacks.pump()
	reopenedManager, err := NewWorkerManager(WorkerManagerConfig{Store: local, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedManager.Close() })
	reopened, err := NewBrowser(BrowserConfig{Client: client, PreferencesPath: preferences, WorkerManager: reopenedManager, Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close(); reopened.Wait(); callbacks.pump() })
	if reopened.workerPanel.savedID.Text != created.ID || reopenedManager.Snapshot().State != WorkerManagerIdle {
		t.Fatal("desktop restart lost the saved pool selection or started workers without a user action")
	}
}

type gatedDesktopSession struct {
	done, closing chan struct{}
	release       <-chan struct{}
	once          sync.Once
}

func (s *gatedDesktopSession) Done() <-chan struct{} { return s.done }
func (s *gatedDesktopSession) Err() error            { return nil }
func (s *gatedDesktopSession) Prompt(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}
func (s *gatedDesktopSession) ResolvePermission(context.Context, string, string) error { return nil }
func (s *gatedDesktopSession) Close() error {
	s.once.Do(func() {
		close(s.closing)
		<-s.release
		close(s.done)
	})
	return nil
}

func TestBrowserWorkerCloseReturnsBeforeProviderJoinsButWaitDrains(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	session := &gatedDesktopSession{done: make(chan struct{}), closing: make(chan struct{}), release: release}
	opened := make(chan struct{})
	manager, _, remote, client, _ := workerManagerFixture(t, func(ctx context.Context, _ storage.WorkerRun, sink conversation.Sink) (providers.Session, error) {
		if err := sink(ctx, conversation.Event{Kind: "session_ready", SessionID: "synthetic-desktop-session"}); err != nil {
			return nil, err
		}
		close(opened)
		return session, nil
	})
	if _, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Synthetic shutdown", Reporter: "test"}, nil); err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	b, err := NewBrowser(BrowserConfig{Client: client, Project: "demo", WorkerManager: manager, Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(); b.Wait(); callbacks.pump() })
	b.Start(ctx)
	p := b.workerPanel
	waitBrowser(t, callbacks, func() bool { return b.selectedProject == "demo" && !p.start.Disabled() })
	p.checkout.SetText(t.TempDir())
	p.provider.SetSelected("codex")
	fynetest.Tap(p.start)
	waitBrowser(t, callbacks, func() bool { return !p.busy && manager.Snapshot().State == WorkerManagerRunning })
	waitForManagerPresence(t, ctx, manager, remote)
	if err := remote.ReconcileAssignments(ctx, time.Now().Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened:
	case <-ctx.Done():
		t.Fatal("synthetic native session did not start")
	}
	returned := make(chan struct{})
	go func() { b.Close(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("browser close blocked on native provider shutdown")
	}
	joined := make(chan struct{})
	go func() { b.Wait(); close(joined) }()
	select {
	case <-session.closing:
	case <-ctx.Done():
		t.Fatal("native session shutdown never began")
	}
	select {
	case <-joined:
		t.Fatal("browser wait returned while native shutdown was still blocked")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-joined:
	case <-ctx.Done():
		t.Fatal("browser wait failed to drain native shutdown")
	}
}
