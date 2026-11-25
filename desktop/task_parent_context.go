package desktop

import (
	"context"

	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/storage"
)

func (b *Browser) showTaskParent(project, task string, parent *storage.TaskParent) {
	b.taskParent.RemoveAll()
	if parent == nil {
		return
	}
	if parent.ProjectIdent != "" && parent.ProjectIdent != project {
		b.taskParent.Add(widget.NewLabel("Upstream parent (" + parent.ProjectIdent + ") · " + parent.Title))
		return
	}
	button := widget.NewButton("Parent · "+parent.Title, func() {
		if b.selectedProject == project && b.selectedTask == task {
			b.selectTask(parent.ID)
		}
	})
	button.Alignment = widget.ButtonAlignLeading
	b.taskParent.Add(button)
}

// loadTaskParents fetches same-project ancestors omitted by the status filter.
// A child still supplies the parent title if its detail is unavailable.
func loadTaskParents(ctx context.Context, project string, rows []storage.TaskSummary, lookup func(context.Context, string, string) (*storage.TaskDetail, error)) []storage.TaskSummary {
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		seen[row.ID] = true
	}
	for i := 0; i < len(rows); i++ {
		parent := rows[i].Parent
		if parent == nil || parent.ID == "" || seen[parent.ID] ||
			(parent.ProjectIdent != "" && parent.ProjectIdent != project) {
			continue
		}
		seen[parent.ID] = true
		row := storage.TaskSummary{ID: parent.ID, Title: parent.Title}
		if ctx.Err() == nil {
			if detail, err := lookup(ctx, project, parent.ID); err == nil && detail != nil {
				task := detail.Task
				row.Title, row.Status, row.Parent = task.Title, task.Status, task.Parent
				row.UpdatedAt, row.Labels = task.UpdatedAt, task.Labels
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// includeTaskParents restores the context that title or ID search can hide.
func includeTaskParents(visible, all []storage.TaskSummary) []storage.TaskSummary {
	byID := make(map[string]storage.TaskSummary, len(all))
	for _, row := range all {
		byID[row.ID] = row
	}
	seen := make(map[string]bool, len(visible))
	for _, row := range visible {
		seen[row.ID] = true
	}
	for i := 0; i < len(visible); i++ {
		parent := visible[i].Parent
		if parent == nil || seen[parent.ID] {
			continue
		}
		if row, ok := byID[parent.ID]; ok {
			seen[row.ID] = true
			visible = append(visible, row)
		}
	}
	return visible
}
