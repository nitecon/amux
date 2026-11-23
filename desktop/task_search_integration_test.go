package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nitecon/amux/storage"
)

type taskSearchResponseFixture struct {
	active atomic.Bool
	mu     sync.Mutex
	reads  [][]string
	rows   []storage.TaskSummary
}

func (f *taskSearchResponseFixture) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.active.Load() || r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/v1/projects/") || !strings.HasSuffix(r.URL.Path, "/tasks") {
			next.ServeHTTP(w, r)
			return
		}
		statuses := strings.Split(r.URL.Query().Get("status"), ",")
		f.mu.Lock()
		f.reads = append(f.reads, append([]string(nil), statuses...))
		f.mu.Unlock()
		allowed := make(map[string]bool, len(statuses))
		for _, status := range statuses {
			allowed[status] = true
		}
		rows := make([]storage.TaskSummary, 0, len(f.rows))
		for _, row := range f.rows {
			if allowed[row.Status] {
				rows = append(rows, row)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rows)
	})
}

func (f *taskSearchResponseFixture) requestStatuses() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([][]string, len(f.reads))
	for i := range f.reads {
		result[i] = append([]string(nil), f.reads[i]...)
	}
	return result
}

func TestBrowserTaskSearchCrossesStatusesWithoutChangingStatusFilter(t *testing.T) {
	search := &taskSearchResponseFixture{rows: []storage.TaskSummary{
		{ID: "01a16c87-1234-4567-890a-123456789abc", Title: "Windows integration check", Status: "user_review", Labels: []string{"os:windows"}},
		{ID: "task-linux", Title: "Linux terminal", Status: "todo"},
		{ID: "task-done", Title: "Windows check complete", Status: "done", Labels: []string{"bug"}},
	}}
	fixture := startEditorBrowser(t, search.wrap)
	b := fixture.browser
	allStatuses := []string{"todo", "in_progress", "user_review", "done", "cancelled"}
	originalTasks, err := fixture.store.Tasks(context.Background(), "demo", allStatuses, false)
	if err != nil {
		t.Fatal(err)
	}
	b.statusSelect.SetSelected("Queued")
	waitBrowser(t, fixture.callbacks, func() bool {
		state := b.taskLists[b.activeWorkspaceID]
		return state != nil && !state.loading && state.status == "todo" && !state.allStates
	})
	search.active.Store(true)

	b.taskSearch.SetText(" #01A16C87 ")
	waitBrowser(t, fixture.callbacks, func() bool {
		state := b.taskLists[b.activeWorkspaceID]
		return state != nil && !state.loading && state.allStates && len(state.items) == 3 && len(b.visibleTasks) == 1
	})
	if got := b.visibleTasks[0].ID; got != "01a16c87-1234-4567-890a-123456789abc" {
		t.Fatalf("short-ID search selected %q", got)
	}
	if b.selection.Status != "todo" || b.prefs.Selections[workspaceSelectionKey(b.prefKey, b.activeWorkspaceID)].Status != "todo" {
		t.Fatal("cross-state search changed the saved status filter")
	}

	for query, want := range map[string][]string{
		"01a16c87-1234-4567-890a-123456789abc": {"01a16c87-1234-4567-890a-123456789abc"},
		"check WINDOWS":                        {"01a16c87-1234-4567-890a-123456789abc", "task-done"},
		"os:WINDOWS":                           {"01a16c87-1234-4567-890a-123456789abc"},
		"bug":                                  {"task-done"},
		"nonexistent":                          {},
	} {
		b.taskSearch.SetText(query)
		wantSet := make(map[string]bool, len(want))
		for _, id := range want {
			wantSet[id] = true
		}
		waitBrowser(t, fixture.callbacks, func() bool { return visibleTaskIDsMatch(b.visibleTasks, wantSet) })
		if b.selection.Status != "todo" || !b.taskLists[b.activeWorkspaceID].allStates {
			t.Fatalf("query %q changed the status/search scope", query)
		}
	}

	b.taskSearch.SetText("")
	waitBrowser(t, fixture.callbacks, func() bool {
		state := b.taskLists[b.activeWorkspaceID]
		return state != nil && !state.loading && !state.allStates && len(b.taskItems) == 1 && len(b.visibleTasks) == 1
	})
	if b.taskItems[0].Status != "todo" || b.selection.Status != "todo" || b.selection.TaskFilter != "" {
		t.Fatalf("blank search did not restore the selected status scope: status=%q query=%q tasks=%+v", b.selection.Status, b.selection.TaskFilter, b.taskItems)
	}
	if got := search.requestStatuses(); !reflect.DeepEqual(got, [][]string{allStatuses, {"todo"}}) {
		t.Fatalf("list request status scopes = %v, want all states then todo", got)
	}
	afterTasks, err := fixture.store.Tasks(context.Background(), "demo", allStatuses, false)
	if err != nil || !reflect.DeepEqual(afterTasks, originalTasks) {
		t.Fatalf("search changed the gateway task queue: before=%+v after=%+v err=%v", originalTasks, afterTasks, err)
	}
}

func visibleTaskIDsMatch(rows []storage.TaskSummary, expected map[string]bool) bool {
	if len(rows) != len(expected) {
		return false
	}
	for _, row := range rows {
		if !expected[row.ID] {
			return false
		}
	}
	return true
}
