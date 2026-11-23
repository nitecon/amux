package desktop

import (
	"context"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	fynetest "fyne.io/fyne/v2/test"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
)

type browserHTTPFixture struct {
	mu            sync.Mutex
	statusQueries []string
	todoStarted   chan struct{}
	releaseTodo   chan struct{}
	blockStatus   string
	projects      []storage.ProjectOverview
}

func (f *browserHTTPFixture) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/projects":
		writeBrowserJSON(w, f.projects)
	case strings.HasSuffix(r.URL.Path, "/tasks"):
		status := r.URL.Query().Get("status")
		f.mu.Lock()
		f.statusQueries = append(f.statusQueries, status)
		f.mu.Unlock()
		if status == f.blockStatus && f.todoStarted != nil {
			select {
			case f.todoStarted <- struct{}{}:
			default:
			}
			select {
			case <-f.releaseTodo:
			case <-r.Context().Done():
				return
			}
		}
		project := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/projects/"), "/tasks")
		title, id := "Done item", "done-id"
		if status == "todo" {
			title, id = "Todo item", "todo-id"
		}
		if project != "demo" {
			title, id = "Other project task", project+"-task"
		}
		writeBrowserJSON(w, []storage.TaskSummary{{ID: id, Title: title, Status: statusOrTodo(status), UpdatedAt: 1}})
	case strings.HasSuffix(r.URL.Path, "/conversations"):
		writeBrowserJSON(w, []storage.Conversation{})
	case strings.Contains(r.URL.Path, "/tasks/"):
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		writeBrowserJSON(w, map[string]any{"id": id, "project_ident": "demo", "title": "Browser task", "status": "todo", "specification": "Inspect this task", "comments": []storage.TaskComment{}})
	default:
		http.NotFound(w, r)
	}
}

func writeBrowserJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func statusOrTodo(status string) string {
	if status == "" {
		return "todo"
	}
	return status
}

func newBrowserTestClient(t *testing.T, fixture *browserHTTPFixture) *gatewayclient.Client {
	t.Helper()
	host := httptest.NewServer(http.HandlerFunc(fixture.handler))
	t.Cleanup(host.Close)
	client, err := gatewayclient.New(host.URL, "browser-test", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func waitBrowser(t *testing.T, q *uiCallbacks, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		q.pump()
		if condition() {
			q.pump()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	q.pump()
	if !condition() {
		t.Fatal("timed out waiting for browser update")
	}
}

func TestBrowserStatusGenerationSearchFavoriteAndPreview(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	fixture := &browserHTTPFixture{
		projects:    []storage.ProjectOverview{{Project: storage.Project{Ident: "alpha"}}, {Project: storage.Project{Ident: "demo"}}},
		todoStarted: make(chan struct{}, 1), releaseTodo: make(chan struct{}), blockStatus: "todo",
	}
	client := newBrowserTestClient(t, fixture)
	callbacks := newUICallbacks()
	browser, err := NewBrowser(BrowserConfig{Client: client, Project: "demo", Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	window := fynetest.NewWindow(browser.Content())
	window.Resize(fyne.NewSize(1280, 760))
	t.Cleanup(func() { browser.Close(); browser.Wait(); callbacks.pump(); window.Close() })
	browser.Start(context.Background())
	waitBrowser(t, callbacks, func() bool { return browser.selectedProject == "demo" && len(browser.taskItems) == 1 })
	fixture.mu.Lock()
	initialStatus := fixture.statusQueries[0]
	fixture.mu.Unlock()
	if initialStatus != "todo,in_progress" {
		t.Fatalf("default active status sent %q", initialStatus)
	}
	callbacks.dispatch(func() { browser.statusSelect.SetSelected("Queued") })
	deadline := time.Now().Add(3 * time.Second)
	for {
		callbacks.pump()
		select {
		case <-fixture.todoStarted:
			goto todoStarted
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("todo status request did not reach gateway")
		}
		time.Sleep(5 * time.Millisecond)
	}
todoStarted:
	callbacks.dispatch(func() { browser.statusSelect.SetSelected("Done") })
	waitBrowser(t, callbacks, func() bool { return len(browser.taskItems) == 1 && browser.taskItems[0].Status == "done" })
	close(fixture.releaseTodo)
	// The older all-status response must not overwrite the newer done filter.
	time.Sleep(30 * time.Millisecond)
	callbacks.pump()
	if len(browser.taskItems) != 1 || browser.taskItems[0].Status != "done" {
		t.Fatal("stale all-status response replaced the selected status results")
	}
	callbacks.dispatch(func() { browser.projectSearch.SetText("alph") })
	callbacks.pump()
	if len(browser.visibleProjects) != 1 || browser.visibleProjects[0].Ident != "alpha" || browser.selectedProject != "demo" {
		t.Fatal("project filtering changed the selected project or returned the wrong row")
	}
	callbacks.dispatch(func() { browser.projects.Select(0) })
	waitBrowser(t, callbacks, func() bool { return browser.selectedProject == "alpha" })
	callbacks.dispatch(func() { fynetest.Tap(browser.selectedProjectFavorite) })
	callbacks.pump()
	if !browser.prefs.Favorites[FavoriteKey(client.Endpoint(), "alpha")] {
		t.Fatal("favorite button did not record the selected project")
	}
	if err := os.MkdirAll(filepath.Dir("/private/tmp/amux-browser-preview.png"), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create("/private/tmp/amux-browser-preview.png")
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(file, window.Canvas().Capture()); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserRestoresSelectionFromExplicitPreferences(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	fixture := &browserHTTPFixture{projects: []storage.ProjectOverview{{Project: storage.Project{Ident: "demo"}}}}
	client := newBrowserTestClient(t, fixture)
	path := filepath.Join(t.TempDir(), "browser.json")
	prefs := NavigationPreferences{Favorites: map[string]bool{}, Selections: map[string]NavigationSelection{client.Endpoint(): {Project: "demo", Task: "done-id", Status: "all"}}}
	if err := SaveNavigationPreferences(path, prefs); err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	browser, err := NewBrowser(BrowserConfig{Client: client, PreferencesPath: path, Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { browser.Close(); browser.Wait(); callbacks.pump() })
	browser.Start(context.Background())
	waitBrowser(t, callbacks, func() bool {
		return browser.selectedProject == "demo" && browser.selectedTask == "done-id" && browser.selectedTaskDetail != nil
	})
	if browser.selection.Status != "all" {
		t.Fatalf("restored status %q", browser.selection.Status)
	}
}

func TestBrowserStatusGroupsDefaultAndCompletedScope(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	fixture := &browserHTTPFixture{projects: []storage.ProjectOverview{{Project: storage.Project{Ident: "demo"}}}}
	client := newBrowserTestClient(t, fixture)
	callbacks := newUICallbacks()
	browser, err := NewBrowser(BrowserConfig{Client: client, Project: "demo", Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	window := fynetest.NewWindow(browser.Content())
	t.Cleanup(func() { browser.Close(); browser.Wait(); callbacks.pump(); window.Close() })
	browser.Start(context.Background())
	waitBrowser(t, callbacks, func() bool { return browser.selectedProject == "demo" && len(browser.taskItems) == 1 })
	if browser.selection.Status != browserStatusActive || browser.statusSelect.Selected != "Active" {
		t.Fatalf("new workspace status = %q (%q), want Active", browser.selection.Status, browser.statusSelect.Selected)
	}
	fixture.mu.Lock()
	initial := fixture.statusQueries[0]
	fixture.mu.Unlock()
	if initial != "todo,in_progress" {
		t.Fatalf("Active scope = %q", initial)
	}

	callbacks.dispatch(func() { browser.statusSelect.SetSelected("Completed") })
	waitBrowser(t, callbacks, func() bool {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		return browser.selection.Status == browserStatusCompleted && len(fixture.statusQueries) >= 2 && fixture.statusQueries[len(fixture.statusQueries)-1] == "done,cancelled"
	})
	fixture.mu.Lock()
	completed := fixture.statusQueries[len(fixture.statusQueries)-1]
	fixture.mu.Unlock()
	if completed != "done,cancelled" {
		t.Fatalf("Completed scope = %q", completed)
	}
}
