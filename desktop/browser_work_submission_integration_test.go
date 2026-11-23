package desktop

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	fynetest "fyne.io/fyne/v2/test"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestNewBrowserWithoutUserConfigKeepsExplicitNavigationAndShowsWorkSettingsError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", "")
	t.Setenv("AMUX_WORK_SUBMISSION_PREFERENCES", "")

	fixture := newWorkSettingsBrowser(t, "", true)
	panel := fixture.browser.workSubmissionSettings
	if fixture.browser.prefsPath != fixture.navigationPath {
		t.Fatalf("explicit navigation path lost: got %q want %q", fixture.browser.prefsPath, fixture.navigationPath)
	}
	if panel.status.Text == "" || !strings.Contains(panel.status.Text, "Could not load work submission preferences") {
		t.Fatalf("missing visible work preferences error: %q (path %q)", panel.status.Text, panel.path)
	}
	if panel.accordion == nil || len(panel.accordion.Items) != 1 {
		t.Fatal("work submission settings are not available in the Browser")
	}
	panel.accordion.Open(0)
	if !panel.accordion.Items[0].Open {
		t.Fatal("settings error is not visible because its section did not open")
	}
}

func TestBrowserWorkSettingsDraftSurvivesNavigationAndCancelDoesNotWrite(t *testing.T) {
	initial := []byte(`{"work_submission":{"mode":"commit_and_push","branch_prefix":"amux/"},"unrelated":{"keep":true}}`)
	workPath := filepath.Join(t.TempDir(), "work-preferences.json")
	if err := os.WriteFile(workPath, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := newWorkSettingsBrowser(t, workPath, false)
	b, p := fixture.browser, fixture.browser.workSubmissionSettings
	composer, editor := b.taskComposer, b.taskEditor

	composer.createTitle.SetText("Unsaved task composer draft")
	composer.commentEntry.SetText("Unsaved comment draft")
	editor.titleEntry.SetText("Unsaved editor draft")
	initialWorkerSelection := b.prefs.WorkerPools[b.endpoint]
	if initialWorkerSelection != "saved-pool" {
		t.Fatalf("fixture worker selection missing: %q", initialWorkerSelection)
	}

	p.mode.SetSelected("Pull request")
	p.prefix.SetText("  review/ ")
	p.accordion.Close(0)
	if p.accordion.Items[0].Open {
		t.Fatal("settings accordion did not close")
	}
	p.accordion.Open(0)
	if !p.accordion.Items[0].Open || p.draft.Mode != "pull_request" || p.draft.BranchPrefix != "  review/ " {
		t.Fatalf("settings draft changed while closing/opening accordion: %+v", p.draft)
	}
	if composer.createTitle.Text != "Unsaved task composer draft" || composer.commentEntry.Text != "Unsaved comment draft" || editor.titleEntry.Text != "Unsaved editor draft" {
		t.Fatal("opening settings changed task composer or editor drafts")
	}

	b.selectTask(fixture.second.ID)
	waitBrowser(t, fixture.callbacks, func() bool {
		return b.selectedTask == fixture.second.ID && b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.second.ID
	})
	if p.draft.Mode != "pull_request" || p.draft.BranchPrefix != "  review/ " || composer.createTitle.Text != "Unsaved task composer draft" {
		t.Fatal("task navigation changed work settings or project-level composer draft")
	}
	b.selectTask(fixture.first.ID)
	waitBrowser(t, fixture.callbacks, func() bool {
		return b.selectedTask == fixture.first.ID && b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == fixture.first.ID
	})
	if editor.titleEntry.Text != "Unsaved editor draft" || composer.commentEntry.Text != "Unsaved comment draft" {
		t.Fatal("task navigation discarded the task-scoped editor or comment draft")
	}

	b.selectProject("beta")
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedProject == "beta" && !activeTaskListLoading(b) })
	if p.draft.Mode != "pull_request" || p.draft.BranchPrefix != "  review/ " || b.prefs.WorkerPools[b.endpoint] != initialWorkerSelection {
		t.Fatal("project navigation changed work settings draft or saved worker selection")
	}
	b.selectProject("demo")
	waitBrowser(t, fixture.callbacks, func() bool { return b.selectedProject == "demo" && !activeTaskListLoading(b) })

	p.cancelDraft()
	if p.draft.Mode != "commit_and_push" || p.draft.BranchPrefix != "amux/" || p.mode.Selected != "Commit and push" || p.prefix.Text != "amux/" {
		t.Fatalf("cancel did not restore confirmed settings: draft=%+v mode=%q prefix=%q", p.draft, p.mode.Selected, p.prefix.Text)
	}
	after, err := os.ReadFile(workPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(initial) {
		t.Fatalf("cancel mutated preferences file:\nbefore %s\nafter  %s", initial, after)
	}
	first, err := fixture.store.Task(context.Background(), "demo", fixture.first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Task.Title != "First" || len(first.Comments) != 0 {
		t.Fatalf("settings navigation mutated task data: %+v", first)
	}
	if b.prefs.WorkerPools[b.endpoint] != initialWorkerSelection {
		t.Fatal("settings cancel changed saved worker selection")
	}
}

type workSettingsBrowserFixture struct {
	store          *storage.Store
	first, second  *storage.Task
	browser        *Browser
	callbacks      *uiCallbacks
	navigationPath string
}

func newWorkSettingsBrowser(t *testing.T, workPath string, omitWorkPath bool) workSettingsBrowserFixture {
	t.Helper()
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "browser.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, project := range []string{"demo", "beta"} {
		if err := store.InsertProject(ctx, storage.Project{Ident: project, ChannelName: project, Kind: "adhoc"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "First", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Second", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertTask(ctx, storage.TaskInsert{Project: "beta", Title: "Other project", Reporter: "user"}, nil); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "work-settings-test-key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	t.Cleanup(host.Close)
	t.Cleanup(func() { _ = handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(ctx) })
	client, err := gatewayclient.New(host.URL, "work-settings-test-key", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	navigationPath := filepath.Join(t.TempDir(), "navigation.json")
	if err := SaveNavigationPreferences(navigationPath, NavigationPreferences{
		WorkerPools: map[string]string{client.Endpoint(): "saved-pool"},
	}); err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	config := BrowserConfig{Client: client, Project: "demo", PreferencesPath: navigationPath, Dispatch: callbacks.dispatch}
	if !omitWorkPath {
		config.WorkPreferencesPath = workPath
	}
	browser, err := NewBrowser(config)
	if err != nil {
		t.Fatal(err)
	}
	window := app.NewWindow("Work submission settings fixture")
	window.SetContent(browser.Content())
	window.Resize(fyne.NewSize(1280, 820))
	window.Show()
	t.Cleanup(window.Close)
	t.Cleanup(func() { browser.Close(); browser.Wait(); callbacks.pump() })
	browser.Start(ctx)
	waitBrowser(t, callbacks, func() bool { return len(browser.taskItems) == 2 && browser.selectedProject == "demo" })
	browser.selectTask(first.ID)
	waitBrowser(t, callbacks, func() bool {
		return browser.selectedTaskDetail != nil && browser.selectedTaskDetail.Task.ID == first.ID
	})
	return workSettingsBrowserFixture{store: store, first: first, second: second, browser: browser, callbacks: callbacks, navigationPath: navigationPath}
}
