package desktop

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	fynetest "fyne.io/fyne/v2/test"
	"github.com/nitecon/amux/storage"
)

type heldTaskListResponse struct {
	entered chan struct{}
	release chan struct{}
	active  atomic.Bool
	once    sync.Once
}

func (g *heldTaskListResponse) unblock() { g.once.Do(func() { close(g.release) }) }

func (g *heldTaskListResponse) hasEntered() bool {
	select {
	case <-g.entered:
		return true
	default:
		return false
	}
}

func (g *heldTaskListResponse) wrap(next http.Handler) http.Handler {
	var held atomic.Bool
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/v1/projects/") || !strings.HasSuffix(r.URL.Path, "/tasks") || !g.active.Load() || !held.CompareAndSwap(false, true) {
			next.ServeHTTP(w, r)
			return
		}
		result := httptest.NewRecorder()
		next.ServeHTTP(result, r)
		close(g.entered)
		select {
		case <-g.release:
		case <-r.Context().Done():
			return
		}
		for key, values := range result.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(result.Code)
		_, _ = w.Write(result.Body.Bytes())
	})
}

func TestBrowserTaskListResponseStaysWithOriginWorkspace(t *testing.T) {
	gate := &heldTaskListResponse{entered: make(chan struct{}), release: make(chan struct{})}
	fixture := startEditorBrowser(t, gate.wrap)
	t.Cleanup(gate.unblock)
	b := fixture.browser
	if err := fixture.store.InsertProject(t.Context(), storage.Project{Ident: "beta", ChannelName: "beta", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	b.Refresh()
	waitBrowser(t, fixture.callbacks, func() bool { return projectExists(b.projectItems, "beta") })
	origin := b.activeWorkspaceID
	gate.active.Store(true)
	b.refreshTasks()
	waitBrowser(t, fixture.callbacks, gate.hasEntered)
	fynetest.Tap(b.workspaceAdd)
	peer := b.activeWorkspaceID
	for i, project := range b.visibleProjects {
		if project.Ident == "beta" {
			b.projects.Select(i)
			break
		}
	}
	if b.selectedProject != "beta" {
		t.Fatalf("peer workspace did not bind beta: %q", b.selectedProject)
	}
	gate.unblock()
	waitBrowser(t, fixture.callbacks, func() bool {
		state := b.taskLists[origin]
		return state != nil && !state.loading && len(state.items) == 2
	})
	activeContainsOriginTask := false
	for _, task := range b.taskItems {
		activeContainsOriginTask = activeContainsOriginTask || task.Title == "First"
	}
	if b.activeWorkspaceID != peer || b.selectedProject != "beta" || activeContainsOriginTask {
		t.Fatalf("origin list response changed the active workspace: workspace=%q project=%q tasks=%+v", b.activeWorkspaceID, b.selectedProject, b.taskItems)
	}
	if got := b.taskLists[origin].items[0].Title; got != "First" {
		t.Fatalf("origin workspace did not retain its response: title=%q", got)
	}
	selectWorkspaceID(t, b, origin)
	if b.selectedProject != "demo" || len(b.taskItems) != 2 || b.taskItems[0].Title != "First" {
		t.Fatalf("origin workspace did not restore its task list snapshot: project=%q tasks=%+v", b.selectedProject, b.taskItems)
	}
}

func TestBrowserTaskListResponseIsRejectedAfterWorkspaceRebind(t *testing.T) {
	gate := &heldTaskListResponse{entered: make(chan struct{}), release: make(chan struct{})}
	fixture := startEditorBrowser(t, gate.wrap)
	t.Cleanup(gate.unblock)
	b := fixture.browser
	if err := fixture.store.InsertProject(t.Context(), storage.Project{Ident: "beta", ChannelName: "beta", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	b.Refresh()
	waitBrowser(t, fixture.callbacks, func() bool { return projectExists(b.projectItems, "beta") })
	workspace := b.activeWorkspaceID
	gate.active.Store(true)
	b.refreshTasks()
	waitBrowser(t, fixture.callbacks, gate.hasEntered)
	for i, project := range b.visibleProjects {
		if project.Ident == "beta" {
			b.projects.Select(i)
			break
		}
	}
	state := b.taskLists[workspace]
	if b.selectedProject != "beta" || state.project != "beta" || !state.loading {
		t.Fatalf("new project list query was not established: selected=%q state=%+v", b.selectedProject, state)
	}
	gate.unblock()
	waitBrowser(t, fixture.callbacks, func() bool { return !state.loading })
	containsOldTask := false
	for _, task := range state.items {
		containsOldTask = containsOldTask || task.Title == "First"
	}
	if containsOldTask || b.selectedProject != "beta" || len(b.taskItems) != 0 {
		t.Fatalf("old project response overrode the rebound workspace: selected=%q state=%+v", b.selectedProject, state)
	}
}
