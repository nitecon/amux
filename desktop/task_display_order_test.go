package desktop

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestBrowserTasksSortByUpdateSecondAndTitleWithoutReorderingQueue(t *testing.T) {
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler { return next })
	b := fixture.browser
	b.taskItems = []storage.TaskSummary{
		{ID: "0", Title: "Alpha", UpdatedAt: 1999, Status: "in_progress"},
		{ID: "1", Title: "Zulu", UpdatedAt: 3000, Status: "done"},
		{ID: "2", Title: "beta 2", UpdatedAt: 2999, Status: "todo"},
		{ID: "3", Title: "Alpha 1", UpdatedAt: 2000, Status: "user_review"},
		{ID: "4", Title: "alpha 0", UpdatedAt: 2500, Status: "todo"},
		{ID: "5", Title: "Unknown", Status: "todo"},
	}
	before := append([]storage.TaskSummary(nil), b.taskItems...)
	b.applyTaskFilter()
	if want := []string{"1", "4", "3", "2", "0", "5"}; !reflect.DeepEqual(taskIDs(b.visibleTasks), want) {
		t.Errorf("display order = %v, want %v", taskIDs(b.visibleTasks), want)
	}
	if !reflect.DeepEqual(b.taskItems, before) {
		t.Fatal("display sort changed the underlying queue")
	}
}

func TestTaskBoardKeepsGatewayRankAfterAReorder(t *testing.T) {
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler { return next })
	board := fixture.browser.taskBoard
	board.setItems([]storage.TaskSummary{
		{ID: "later-title", Title: "Zulu", Status: "done", Rank: 0, UpdatedAt: 2000},
		{ID: "earlier-title", Title: "Alpha", Status: "done", Rank: 1, UpdatedAt: 2000},
	})
	if got, want := taskIDs(board.lanes[2].items), []string{"later-title", "earlier-title"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Complete lane order = %v, want gateway rank %v", got, want)
	}
}
