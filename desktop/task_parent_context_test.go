package desktop

import (
	"context"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestTaskParentStaysVisibleThroughStatusAndSearchFilters(t *testing.T) {
	parent := &storage.TaskParent{ID: "parent", ProjectIdent: "agent-gateway", Title: "Parent in review"}
	items := []storage.TaskSummary{
		{ID: "child", Title: "Queued validation", Status: "todo", Parent: parent},
		{ID: "sibling", Title: "Other subtask", Status: "todo", Parent: parent},
	}
	root := &storage.TaskParent{ID: "sre-root", ProjectIdent: "sre", Title: "Delegation root"}
	calls := 0
	all := loadTaskParents(context.Background(), "agent-gateway", items, func(_ context.Context, project, id string) (*storage.TaskDetail, error) {
		calls++
		if project != parent.ProjectIdent || id != parent.ID {
			t.Fatalf("unexpected parent lookup %s/%s", project, id)
		}
		return &storage.TaskDetail{Task: storage.Task{ID: id, Title: parent.Title, Status: "user_review", Parent: root}}, nil
	})
	visible := groupTaskRows(includeTaskParents(FilterTasks(all, "queued validation"), all))
	if calls != 1 || len(visible) != 2 || visible[0].ID != parent.ID ||
		visible[0].Status != "user_review" || visible[0].Parent != root || visible[1].ID != "child" {
		t.Fatalf("expected review parent above matching queued child, got %+v (%d lookups)", visible, calls)
	}
}
