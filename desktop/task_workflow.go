package desktop

import (
	"context"
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/storage"
)

type taskWorkflow struct {
	browser      *Browser
	close        *widget.Button
	park         *widget.Button
	resume       *widget.Button
	reviewAction *widget.Entry
	reviewPrompt *widget.Label
	status       *widget.Label
	refresh      *widget.Button
	cancellation *widget.Label

	drafts            map[string]*workflowDraft
	selected          string
	cancelGen         uint64
	cancelLoading     bool
	cancelLoaded      bool
	focusReviewAction bool
}

type workflowDraft struct {
	reviewAction string
	inFlight     bool
	unknown      bool
	resolving    bool
	operation    uint64
	status       string
	err          string
}

func newTaskWorkflow(browser *Browser) *taskWorkflow {
	w := &taskWorkflow{browser: browser, drafts: make(map[string]*workflowDraft)}
	w.close = widget.NewButton("Close task", func() { w.submit("close") })
	w.park = widget.NewButton("Send to user review", func() { w.submit("park") })
	w.resume = widget.NewButton("Return to queue", func() { w.submit("resume") })
	w.reviewAction = widget.NewMultiLineEntry()
	w.reviewAction.SetMinRowsVisible(1)
	w.reviewAction.SetPlaceHolder("What should be reviewed? (required)")
	w.reviewAction.Wrapping = fyne.TextWrapWord
	w.reviewAction.OnChanged = func(value string) {
		if draft := w.currentDraft(); draft != nil {
			draft.reviewAction = value
		}
		w.refreshWidgets()
	}
	w.reviewPrompt = selectableLabel("")
	w.reviewPrompt.Wrapping = fyne.TextWrapWord
	w.reviewPrompt.Hide()
	w.status = widget.NewLabel("Select a task to change its workflow status.")
	w.status.Wrapping = fyne.TextWrapWord
	w.refresh = widget.NewButton("Refresh task state", w.refreshUnknown)
	w.refresh.Hide()
	w.cancellation = selectableLabel("")
	w.cancellation.Wrapping = fyne.TextWrapWord
	w.cancellation.Hide()
	w.refreshWidgets()
	return w
}

// actions returns the primary workflow buttons shown in the static task header.
func (w *taskWorkflow) actions() fyne.CanvasObject {
	return container.NewHBox(w.close, w.resume)
}

func (w *taskWorkflow) panel() fyne.CanvasObject {
	return container.NewVBox(
		w.reviewPrompt,
		container.NewBorder(nil, nil, nil, w.park, w.reviewAction),
		w.status,
		w.refresh,
		w.cancellation,
	)
}

func (w *taskWorkflow) selectTask(project, task string) {
	w.selected = ""
	w.focusReviewAction = false
	if project != "" && task != "" {
		w.selected = taskEditorKey(project, task)
	}
	w.cancelGen++
	w.cancelLoading = false
	w.cancelLoaded = false
	w.cancellation.SetText("")
	w.cancellation.Hide()
	w.reviewPrompt.SetText("")
	w.reviewPrompt.Hide()
	draft := w.currentDraft()
	if draft != nil {
		w.reviewAction.SetText(draft.reviewAction)
	} else {
		w.reviewAction.SetText("")
	}
	w.refreshWidgets()
}

func (w *taskWorkflow) currentDraft() *workflowDraft {
	if w.selected == "" {
		return nil
	}
	draft := w.drafts[w.selected]
	if draft == nil {
		draft = &workflowDraft{}
		w.drafts[w.selected] = draft
	}
	return draft
}

func (w *taskWorkflow) setTask(project, task string, current *storage.Task) {
	if w.selected != taskEditorKey(project, task) {
		return
	}
	draft := w.currentDraft()
	if draft == nil {
		return
	}
	draft.status = current.Status
	if current.Status == "user_review" && current.ReviewAction != nil && strings.TrimSpace(*current.ReviewAction) != "" {
		w.reviewPrompt.SetText("Requested review: " + strings.TrimSpace(*current.ReviewAction))
		w.reviewPrompt.Show()
	} else {
		w.reviewPrompt.SetText("")
		w.reviewPrompt.Hide()
	}
	w.refreshWidgets()
	if w.focusReviewAction && (current.Status == "todo" || storage.IsActiveTaskStatus(current.Status)) {
		if canvas := fyne.CurrentApp().Driver().CanvasForObject(w.reviewAction); canvas != nil {
			canvas.Focus(w.reviewAction)
			w.focusReviewAction = false
		}
	}
	if current.Status != "cancelled" {
		w.cancelGen++
		w.cancelLoading = false
		w.cancelLoaded = false
		w.cancellation.SetText("")
		w.cancellation.Hide()
	}
	if current.Status == "cancelled" && !w.cancelLoaded && !w.cancelLoading {
		w.loadCancellation(project, task)
	}
}

func (w *taskWorkflow) requestReviewActionFocus() {
	w.focusReviewAction = true
	if draft := w.currentDraft(); draft != nil && (draft.status == "todo" || storage.IsActiveTaskStatus(draft.status)) {
		if canvas := fyne.CurrentApp().Driver().CanvasForObject(w.reviewAction); canvas != nil {
			canvas.Focus(w.reviewAction)
			w.focusReviewAction = false
		}
	}
}

func (w *taskWorkflow) refreshWidgets() {
	draft := w.currentDraft()
	if draft == nil {
		w.close.Disable()
		w.park.Disable()
		w.resume.Disable()
		w.reviewAction.Disable()
		w.close.Hide()
		w.park.Hide()
		w.resume.Hide()
		w.reviewAction.Hide()
		w.status.Hide()
		w.refresh.Hide()
		return
	}
	w.close.Enable()
	closeLabel := "Close task"
	w.close.Importance = widget.MediumImportance
	if draft.status == "user_review" {
		closeLabel = "Accept and close"
		w.close.Importance = widget.HighImportance
	}
	if w.close.Text != closeLabel {
		w.close.SetText(closeLabel)
	}
	w.park.Enable()
	w.resume.Enable()
	w.reviewAction.Enable()
	if draft.inFlight || draft.unknown || draft.resolving {
		w.close.Disable()
		w.park.Disable()
		w.resume.Disable()
	}
	if draft.status == "" || draft.status == "done" {
		w.close.Hide()
	} else {
		w.close.Show()
	}
	if draft.status != "todo" && !storage.IsActiveTaskStatus(draft.status) {
		w.park.Hide()
	} else {
		w.park.Show()
	}
	if draft.status != "user_review" {
		w.resume.Hide()
	} else {
		w.resume.Show()
	}
	if draft.status != "todo" && !storage.IsActiveTaskStatus(draft.status) {
		w.reviewAction.Hide()
	} else {
		w.reviewAction.Show()
	}
	if strings.TrimSpace(draft.reviewAction) == "" || len(strings.TrimSpace(draft.reviewAction)) > 4096 || (draft.status != "todo" && !storage.IsActiveTaskStatus(draft.status)) {
		w.park.Disable()
	} else if !draft.inFlight && !draft.unknown && !draft.resolving {
		w.park.Enable()
	}
	w.status.Show()
	if draft.inFlight {
		w.status.SetText("Updating task status…")
	} else if draft.unknown {
		message := "The status update outcome is unknown. Refresh this task before retrying."
		if draft.err != "" {
			message += " " + draft.err
		}
		w.status.SetText(message)
	} else if draft.err != "" {
		w.status.SetText(draft.err)
	} else {
		w.status.Hide()
	}
	if draft.unknown || draft.err != "" {
		w.refresh.Show()
		if draft.resolving {
			w.refresh.Disable()
		} else {
			w.refresh.Enable()
		}
	} else {
		w.refresh.Hide()
	}
}

func (w *taskWorkflow) submit(action string) {
	b := w.browser
	b.mu.Lock()
	project, task := b.selectedProject, b.selectedTask
	if b.closing || project == "" || task == "" || b.ctx == nil {
		b.mu.Unlock()
		return
	}
	draft := w.currentDraft()
	if draft == nil || draft.inFlight || draft.unknown || draft.resolving {
		b.mu.Unlock()
		return
	}
	if action == "close" && (draft.status == "" || draft.status == "done") || action == "park" && (draft.status != "todo" && !storage.IsActiveTaskStatus(draft.status) || strings.TrimSpace(draft.reviewAction) == "" || len(strings.TrimSpace(draft.reviewAction)) > 4096) || action == "resume" && draft.status != "user_review" {
		b.mu.Unlock()
		return
	}
	draft.inFlight = true
	draft.err = ""
	draft.operation++
	op := draft.operation
	reason := strings.TrimSpace(draft.reviewAction)
	w.refreshWidgets()
	b.mu.Unlock()
	if !b.launch(func(ctx context.Context) {
		var result *storage.Task
		var err error
		switch action {
		case "close":
			result, err = b.client.CloseTask(ctx, project, task)
		case "park":
			result, err = b.client.ParkTask(ctx, project, task, reason)
		case "resume":
			result, err = b.client.ResumeTask(ctx, project, task)
		}
		b.dispatch(func() { w.finish(project, task, op, action, result, err) })
	}) {
		w.finish(project, task, op, action, nil, fmt.Errorf("browser is closed"))
	}
}

func (w *taskWorkflow) finish(project, task string, op uint64, action string, result *storage.Task, err error) {
	b := w.browser
	b.mu.Lock()
	draft := w.drafts[taskEditorKey(project, task)]
	if b.closing || draft == nil || !draft.inFlight || draft.operation != op {
		b.mu.Unlock()
		return
	}
	selectedNow := !b.closing && b.selectedProject == project && b.selectedTask == task && w.selected == taskEditorKey(project, task)
	draft.inFlight = false
	if err != nil {
		if postOutcomeUnknown(err) {
			draft.unknown = true
			draft.err = ""
		} else {
			draft.err = "Status update failed: " + err.Error()
		}
		if selectedNow {
			w.refreshWidgets()
		}
		b.mu.Unlock()
		return
	}
	if result == nil || result.ID != task || result.ProjectIdent != project || !workflowAcknowledges(action, result.Status) {
		draft.unknown = true
		draft.err = ""
		if selectedNow {
			w.refreshWidgets()
		}
		b.mu.Unlock()
		return
	}
	draft.status = result.Status
	draft.unknown = false
	draft.err = ""
	if selectedNow {
		w.refreshWidgets()
	}
	b.mu.Unlock()
	if selectedNow {
		b.refreshTaskAfterMutation(project, task)
		b.refreshTasksAfterMutation(project)
	}
}

func workflowAcknowledges(action, status string) bool {
	switch action {
	case "close":
		return status == "done"
	case "park":
		return status == "user_review"
	case "resume":
		return status == "todo"
	default:
		return false
	}
}

func (w *taskWorkflow) refreshUnknown() {
	b := w.browser
	b.mu.Lock()
	project, task, taskGen := b.selectedProject, b.selectedTask, b.taskGen
	draft := w.currentDraft()
	if b.closing || b.ctx == nil || project == "" || task == "" || draft == nil || (!draft.unknown && draft.err == "") || draft.resolving {
		b.mu.Unlock()
		return
	}
	draft.resolving = true
	op := draft.operation
	b.taskGen++ // Ignore any detail GET that began before this explicit reconciliation.
	taskGen = b.taskGen
	b.detailLoading = false
	conversations := append([]storage.Conversation(nil), b.conversationItems...)
	after, more := b.conversationAfter, b.conversationsMore
	w.refreshWidgets()
	b.mu.Unlock()
	if !b.launch(func(ctx context.Context) {
		detail, err := b.client.TaskDetail(ctx, project, task)
		b.dispatch(func() {
			b.mu.Lock()
			valid := !b.closing && b.selectedProject == project && b.selectedTask == task && b.taskGen == taskGen
			current := w.drafts[taskEditorKey(project, task)]
			if current != nil && current.resolving && current.operation == op {
				current.resolving = false
				if err == nil && detail != nil && detail.Task.ID == task {
					current.unknown = false
					current.err = ""
					current.status = detail.Task.Status
				} else if current.unknown {
					current.err = "Refresh failed; the status update outcome remains unknown. " + errorText(err)
				} else {
					current.err = "Refresh failed: " + errorText(err)
				}
			}
			b.mu.Unlock()
			if !valid {
				return
			}
			if err == nil && detail != nil {
				b.applyTaskDetail(project, task, taskGen, detail, conversations, after, more, nil)
			}
			w.refreshWidgets()
		})
	}) {
		b.dispatch(func() {
			b.mu.Lock()
			current := w.drafts[taskEditorKey(project, task)]
			if current != nil && current.operation == op && current.resolving {
				current.resolving = false
			}
			visible := b.selectedProject == project && b.selectedTask == task && w.selected == taskEditorKey(project, task)
			b.mu.Unlock()
			if visible {
				w.refreshWidgets()
			}
		})
	}
}

func errorText(err error) string {
	if err == nil {
		return "task details were unavailable"
	}
	return err.Error()
}

func (w *taskWorkflow) loadCancellation(project, task string) {
	b := w.browser
	b.mu.Lock()
	if b.closing || b.ctx == nil || b.selectedProject != project || b.selectedTask != task {
		b.mu.Unlock()
		return
	}
	w.cancelGen++
	generation := w.cancelGen
	w.cancelLoading = true
	b.mu.Unlock()
	if !b.launch(func(ctx context.Context) {
		report, err := b.client.TaskCancellation(ctx, project, task)
		b.dispatch(func() {
			b.mu.Lock()
			valid := !b.closing && b.selectedProject == project && b.selectedTask == task && generation == w.cancelGen
			if generation == w.cancelGen {
				w.cancelLoading = false
				w.cancelLoaded = valid && err == nil && report != nil
			}
			b.mu.Unlock()
			if !valid {
				return
			}
			if err != nil || report == nil {
				w.cancellation.SetText("Cancellation report unavailable. Executor stop is not confirmed.")
			} else if report.ExecutorStopConfirmed {
				w.cancellation.SetText(fmt.Sprintf("Cancellation reason: %s\nExecutor stop confirmed: yes", report.Reason))
			} else {
				w.cancellation.SetText(fmt.Sprintf("Cancellation reason: %s\nExecutor stop: NOT confirmed", report.Reason))
			}
			w.cancellation.Show()
		})
	}) {
		b.mu.Lock()
		if generation == w.cancelGen {
			w.cancelLoading = false
		}
		b.mu.Unlock()
		return
	}
}
