package desktop

import (
	"context"
	"fmt"
	"image/color"
	"sort"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/storage"
)

type taskBoardLane struct {
	title    string
	statuses []string
	list     *widget.List
	heading  *canvas.Text
	items    []storage.TaskSummary
	cards    map[string]*taskBoardCard
}

type taskBoardCard struct {
	widget.BaseWidget
	board   *taskBoard
	content fyne.CanvasObject
	task    storage.TaskSummary
	last    fyne.Position
}

func newTaskBoardCard(board *taskBoard) *taskBoardCard {
	c := &taskBoardCard{board: board}
	c.ExtendBaseWidget(c)
	return c
}

func (c *taskBoardCard) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(c.content)
}

func (c *taskBoardCard) Dragged(e *fyne.DragEvent) { c.last = e.AbsolutePosition }
func (c *taskBoardCard) DragEnd()                  { c.board.drop(c.task, c.last) }

type taskBoard struct {
	browser        *Browser
	content        fyne.CanvasObject
	message        *widget.Label
	lanes          []*taskBoardLane
	project        string
	loading        bool
	generation     uint64
	readGeneration uint64
	writePending   bool
}

func newTaskBoard(b *Browser) *taskBoard {
	board := &taskBoard{browser: b, message: widget.NewLabel("")}
	lanes := []taskBoardLane{
		{title: "Active", statuses: []string{browserStatusInProgress, browserStatusStalled, browserStatusTodo}},
		{title: "Blocked / User Review", statuses: browserTaskStatuses(browserStatusUserReview)},
		{title: "Complete", statuses: browserTaskStatuses(browserStatusCompleted)},
	}
	columns := container.NewGridWithColumns(len(lanes))
	cardLists := cardListTheme{Theme: fyne.CurrentApp().Settings().Theme()}
	for i := range lanes {
		lane := &lanes[i]
		lane.cards = make(map[string]*taskBoardCard)
		lane.list = widget.NewList(func() int { return len(lane.items) }, func() fyne.CanvasObject {
			parent := widget.NewLabel("")
			parent.Truncation = fyne.TextTruncateEllipsis
			parent.SizeName = theme.SizeNameCaptionText
			content := container.NewBorder(nil, parent, nil, nil, taskCardTitle())
			card := newTaskBoardCard(board)
			card.content = container.NewStack(browserCardSurface(), container.NewPadded(content))
			return card
		}, func(id widget.ListItemID, object fyne.CanvasObject) {
			if id >= len(lane.items) {
				return
			}
			task := lane.items[id]
			boardCard := object.(*taskBoardCard)
			if boardCard.task.ID != "" {
				delete(lane.cards, boardCard.task.ID)
			}
			boardCard.task = task
			lane.cards[task.ID] = boardCard
			card := updateBrowserListCard(boardCard.content, task.ID == b.selectedTask)
			browserCardObject(card, 0).(*fyne.Container).Objects[0].(*widget.Label).SetText(task.Title)
			parent := browserCardObject(card, 1).(*widget.Label)
			caption := browserStatusLabel(task.Status)
			if task.Parent != nil {
				caption += " · Subtask of " + task.Parent.Title
			}
			parent.SetText(caption)
		})
		lane.list.HideSeparators = true
		lane.list.OnSelected = func(id widget.ListItemID) {
			if id >= len(lane.items) || b.selectedProject != board.project {
				return
			}
			taskID := lane.items[id].ID
			lane.list.UnselectAll()
			b.openTaskFromProject(board.project, taskID)
		}
		board.lanes = append(board.lanes, lane)
		lane.heading = browserSectionHeading(lane.title)
		lane.heading.Color = taskStatusColor(lane.statuses[0])
		columns.Add(browserPanel(container.NewBorder(lane.heading, nil, nil, nil, container.NewThemeOverride(lane.list, cardLists))))
	}
	board.content = container.NewBorder(board.message, nil, nil, nil, columns)
	return board
}

func (board *taskBoard) drop(task storage.TaskSummary, position fyne.Position) {
	b := board.browser
	// A read in flight must not discard the drop: background refreshes start at
	// any time, and the readGeneration bump below supersedes that read.
	if board.writePending || board.project == "" || b.selectedProject != board.project {
		return
	}
	var target *taskBoardLane
	for _, lane := range board.lanes {
		pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(lane.list)
		size := lane.list.Size()
		if position.X >= pos.X && position.X <= pos.X+size.Width && position.Y >= pos.Y && position.Y <= pos.Y+size.Height {
			target = lane
			break
		}
	}
	if target == nil {
		return
	}
	if target.statuses[0] == browserStatusUserReview && task.Status != browserStatusUserReview {
		if task.Status != browserStatusTodo && task.Status != browserStatusInProgress {
			board.message.SetText("Only queued or active tasks can be sent to user review.")
			board.message.Show()
			return
		}
		b.openTaskFromProject(board.project, task.ID)
		b.taskWorkflow.requestReviewActionFocus()
		board.message.SetText("Add a specific review request, then choose Send to user review.")
		board.message.Show()
		return
	}
	destination := task.Status
	if destination == browserStatusUserReview || destination == browserStatusDone {
		destination = browserStatusTodo
	}
	if !containsStatus(target.statuses, destination) {
		destination = target.statuses[0]
	}
	if len(target.statuses) > 1 {
		closest := float32(1e9)
		for _, row := range target.items {
			if row.ID == task.ID {
				continue
			}
			card := target.cards[row.ID]
			if card == nil {
				continue
			}
			pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(card)
			distance := absFloat(position.Y - (pos.Y + card.Size().Height/2))
			if distance < closest {
				closest, destination = distance, row.Status
			}
		}
	}
	order := make([]string, 0, len(target.items)+1)
	insertAt := len(target.items)
	for _, row := range target.items {
		if row.ID == task.ID {
			continue
		}
		if row.Status == destination {
			rowCard := target.cards[row.ID]
			if rowCard != nil {
				rowPos := fyne.CurrentApp().Driver().AbsolutePositionForObject(rowCard)
				if position.Y < rowPos.Y+rowCard.Size().Height/2 && insertAt == len(target.items) {
					insertAt = len(order)
				}
			}
			order = append(order, row.ID)
		}
	}
	if insertAt > len(order) {
		insertAt = len(order)
	}
	order = append(order, "")
	copy(order[insertAt+1:], order[insertAt:])
	order[insertAt] = task.ID
	project, generation := board.project, board.generation
	board.writePending = true
	board.readGeneration++
	board.loading = false
	started := b.launch(func(ctx context.Context) {
		rows, err := b.client.ReorderTasks(ctx, project, destination, order)
		b.dispatch(func() {
			if board.generation != generation || board.project != project || b.selectedProject != project {
				board.writePending = false
				board.refresh()
				return
			}
			board.writePending = false
			if err != nil {
				board.message.SetText("Task move failed: " + err.Error())
				board.message.Show()
				return
			}
			board.applyReorderAcknowledgement(task.ID, rows)
		})
	})
	if !started {
		board.writePending = false
		board.refresh()
	}
}

func (board *taskBoard) applyReorderAcknowledgement(movedID string, rows []storage.TaskSummary) {
	replaced := make(map[string]bool, len(rows)+1)
	replaced[movedID] = true
	for _, row := range rows {
		replaced[row.ID] = true
	}
	items := make([]storage.TaskSummary, 0)
	for _, lane := range board.lanes {
		for _, row := range lane.items {
			if !replaced[row.ID] {
				items = append(items, row)
			}
		}
	}
	items = append(items, rows...)
	board.setItems(items)
	board.message.Hide()
}

func containsStatus(statuses []string, status string) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func absFloat(value float32) float32 {
	if value < 0 {
		return -value
	}
	return value
}

func taskStatusColor(status string) color.Color {
	switch status {
	case browserStatusInProgress:
		return theme.PrimaryColor()
	case browserStatusUserReview, browserStatusStalled:
		return theme.WarningColor()
	case browserStatusDone:
		return theme.SuccessColor()
	default:
		return theme.ForegroundColor()
	}
}

func (b *Browser) openTaskFromProject(project, taskID string) {
	if b.selectedProject != project {
		b.selectProject(project)
	}
	if b.selection.Status != browserStatusAll {
		b.statusSelect.SetSelected(browserStatusLabel(browserStatusAll))
	}
	b.setMainView("Tasks")
	b.selectTask(taskID)
}

func (b *Browser) showTaskSubtasks(children []storage.Task, err error) {
	b.taskSubtasks.RemoveAll()
	if err != nil || len(children) == 0 {
		return
	}
	b.taskSubtasks.Add(browserSectionHeading("SUBTASKS"))
	for _, child := range children {
		button := widget.NewButton(child.Title+" · "+browserStatusLabel(child.Status), func() {
			b.openTaskFromProject(child.ProjectIdent, child.ID)
		})
		button.Alignment = widget.ButtonAlignLeading
		b.taskSubtasks.Add(button)
	}
}

func (board *taskBoard) refresh() {
	b := board.browser
	project := b.selectedProject
	if board.project != project {
		board.project = project
		board.generation++
		board.readGeneration++
		board.loading = false
		board.setItems(nil)
		board.message.SetText("Select a project to view its tasks.")
		board.message.Show()
	}
	if project == "" || board.loading || board.writePending {
		return
	}
	generation := board.generation
	board.readGeneration++
	readGeneration := board.readGeneration
	board.loading = b.launch(func(ctx context.Context) {
		items, err := b.client.Tasks(ctx, project, browserTaskStatuses(browserStatusAll))
		b.dispatch(func() {
			if b.closing || generation != board.generation || readGeneration != board.readGeneration || project != b.selectedProject {
				return
			}
			board.loading = false
			if err != nil {
				board.message.SetText("Task board unavailable: " + err.Error())
				board.message.Show()
				return
			}
			board.message.Hide()
			board.setItems(items)
		})
	})
}

func (board *taskBoard) setItems(items []storage.TaskSummary) {
	byStatus := make(map[string][]storage.TaskSummary)
	for _, task := range items {
		byStatus[task.Status] = append(byStatus[task.Status], task)
	}
	for _, lane := range board.lanes {
		lane.items = nil
		for _, status := range lane.statuses {
			rows := byStatus[status]
			sort.SliceStable(rows, func(i, j int) bool {
				if rows[i].Rank != rows[j].Rank {
					return rows[i].Rank < rows[j].Rank
				}
				return rows[i].Title < rows[j].Title
			})
			lane.items = append(lane.items, rows...)
		}
		lane.heading.Text = fmt.Sprintf("%s · %d", lane.title, len(lane.items))
		lane.heading.Refresh()
		lane.list.Refresh()
	}
}
