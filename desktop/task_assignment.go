package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/taskplatform"
)

type taskAssignmentDraft struct {
	task             storage.Task
	hasTask          bool
	targets          []storage.RegisteredTarget
	targetsLoaded    bool
	targetsCheckedAt time.Time
	selected         *storage.RegisteredTargetIdentity
	requester        *storage.RegisteredTargetIdentity
	loading          bool
	inFlight         bool
	unknown          bool
	resolving        bool
	operation        uint64
	operationKind    assignmentOperationKind
	err              string
	watchTarget      *storage.RegisteredTargetIdentity
}

type assignmentOperationKind uint8

const (
	assignmentOperationNone assignmentOperationKind = iota
	assignmentOperationAssign
	assignmentOperationWatch
	assignmentOperationClearWatch
)

type taskAssignment struct {
	browser         *Browser
	section         *fyne.Container
	target          *widget.Select
	assign          *widget.Button
	refresh         *widget.Button
	status          *widget.Label
	ownerStatus     *widget.Label
	watch           *widget.Button
	clearWatch      *widget.Button
	requesterStatus *widget.Label

	selectedKey string
	drafts      map[string]*taskAssignmentDraft
	choices     map[string]storage.RegisteredTargetIdentity
	updating    bool
}

func newTaskAssignment(browser *Browser) *taskAssignment {
	a := &taskAssignment{browser: browser, drafts: make(map[string]*taskAssignmentDraft), choices: make(map[string]storage.RegisteredTargetIdentity)}
	a.target = widget.NewSelect(nil, a.selectTarget)
	a.target.PlaceHolder = "Select a live agent"
	a.assign = widget.NewButton("Assign task", a.submit)
	a.refresh = widget.NewButton("Refresh agents", a.refreshState)
	a.watch = widget.NewButton("Save result recipient", a.saveRequester)
	a.clearWatch = widget.NewButton("Clear result recipient", a.clearRequester)
	a.status = widget.NewLabel("Select a task to assign it to a live agent.")
	a.status.Wrapping = fyne.TextWrapWord
	a.ownerStatus = widget.NewLabel("")
	a.ownerStatus.Wrapping = fyne.TextWrapWord
	a.requesterStatus = widget.NewLabel("")
	a.requesterStatus.Wrapping = fyne.TextWrapWord
	a.section = container.NewVBox(
		widget.NewLabelWithStyle("Task assignment and result recipient", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		a.target,
		container.NewHBox(a.assign, a.watch, a.clearWatch, a.refresh),
		a.status,
		a.ownerStatus,
		a.requesterStatus,
	)
	a.refreshWidgets()
	return a
}

func (a *taskAssignment) panel() fyne.CanvasObject { return a.section }

func (a *taskAssignment) currentDraft() *taskAssignmentDraft {
	if a.selectedKey == "" {
		return nil
	}
	d := a.drafts[a.selectedKey]
	if d == nil {
		d = &taskAssignmentDraft{}
		a.drafts[a.selectedKey] = d
	}
	return d
}

func (a *taskAssignment) selectTask(project, task string) {
	a.selectedKey = ""
	if project != "" && task != "" {
		a.selectedKey = taskEditorKey(project, task)
		a.currentDraft()
	}
	a.choices = make(map[string]storage.RegisteredTargetIdentity)
	a.target.Options = nil
	a.updating = true
	a.target.SetSelected("")
	a.updating = false
	a.refreshWidgets()
}

func (a *taskAssignment) setTask(project, task string, detail storage.Task) {
	if a.selectedKey != taskEditorKey(project, task) || detail.ID != task || detail.ProjectIdent != project {
		return
	}
	d := a.currentDraft()
	d.task, d.hasTask = detail, true
	d.requester = requesterFromTask(detail)
	a.refreshWidgets()
	if !d.targetsLoaded && !d.loading && !d.inFlight && !d.unknown {
		a.loadTargets(false)
	}
}

func requesterFromTask(task storage.Task) *storage.RegisteredTargetIdentity {
	if len(task.Requester) == 0 || string(task.Requester) == "null" {
		return nil
	}
	var requester *storage.RegisteredTargetIdentity
	if json.Unmarshal(task.Requester, &requester) != nil {
		return nil
	}
	return requester
}

func assignmentActive(d *taskAssignmentDraft) bool {
	return d != nil && d.hasTask && (d.task.Status == "todo" || storage.IsActiveTaskStatus(d.task.Status))
}

func (a *taskAssignment) selectTarget(label string) {
	if a.updating {
		return
	}
	d := a.currentDraft()
	if d == nil || d.inFlight || d.unknown || d.resolving {
		return
	}
	if target, ok := a.choices[label]; ok {
		copy := target
		d.selected = &copy
	} else {
		d.selected = nil
	}
	d.err = ""
	a.refreshWidgets()
}

func registeredTargetLabel(entry storage.RegisteredTarget) string {
	name := entry.Target.Origin.Provider
	if entry.DisplayName != nil && strings.TrimSpace(*entry.DisplayName) != "" {
		name = strings.TrimSpace(*entry.DisplayName)
	}
	parts := []string{name}
	if entry.Hostname != nil && strings.TrimSpace(*entry.Hostname) != "" {
		parts = append(parts, strings.TrimSpace(*entry.Hostname))
	}
	parts = append(parts, entry.Target.Origin.OS, entry.Target.Origin.SessionID, entry.Target.Origin.InstanceID, entry.Target.Generation)
	return strings.Join(parts, " · ")
}

func taskOwnerLabel(task storage.Task, targets []storage.RegisteredTarget, checkedAt, now time.Time) string {
	if task.OwnerOrigin == nil && task.OwnerAgentID == nil {
		return ""
	}
	owner := "Unknown agent"
	if task.OwnerAgentID != nil && strings.TrimSpace(*task.OwnerAgentID) != "" {
		owner = strings.TrimSpace(*task.OwnerAgentID)
	}
	machine := "machine unavailable"
	if !checkedAt.IsZero() && now.Sub(checkedAt) >= 0 && now.Sub(checkedAt) <= time.Minute && task.OwnerOrigin != nil {
		var latest *storage.RegisteredTarget
		for i := range targets {
			entry := &targets[i]
			if entry.Target.Origin.SessionID != task.OwnerOrigin.SessionID || entry.ExpiresAt <= now.UnixMilli() {
				continue
			}
			if latest == nil || entry.SeenAt > latest.SeenAt {
				latest = entry
			}
		}
		if latest != nil {
			if latest.DisplayName != nil && strings.TrimSpace(*latest.DisplayName) != "" {
				owner = strings.TrimSpace(*latest.DisplayName)
			}
			if latest.Hostname != nil && strings.TrimSpace(*latest.Hostname) != "" {
				machine = strings.TrimSpace(*latest.Hostname)
			}
		}
	}
	return fmt.Sprintf("%s · %s (external)", owner, machine)
}

func (a *taskAssignment) refreshWidgets() {
	d := a.currentDraft()
	if d == nil || !d.hasTask {
		a.section.Hide()
		a.target.Disable()
		a.assign.Disable()
		a.watch.Disable()
		a.clearWatch.Hide()
		a.refresh.Disable()
		a.status.SetText("Select a task to set its result recipient or assign it.")
		return
	}
	a.section.Show()
	choices := make(map[string]storage.RegisteredTargetIdentity)
	labels := make([]string, 0, len(d.targets))
	now := time.Now().UnixMilli()
	requiredOS, osErr := taskplatform.RequiredOS(d.task.Labels)
	for _, entry := range d.targets {
		if entry.ExpiresAt <= now || entry.SeenAt <= 0 || entry.ExpiresAt <= entry.SeenAt || osErr != nil || (requiredOS != "" && entry.Target.Origin.OS != requiredOS) {
			continue
		}
		label := registeredTargetLabel(entry)
		choices[label] = entry.Target
		labels = append(labels, label)
	}
	a.choices = choices
	a.updating = true
	a.target.Options = labels
	selectedLabel := ""
	if d.selected != nil {
		for label, target := range choices {
			if target == *d.selected {
				selectedLabel = label
				break
			}
		}
		if selectedLabel == "" {
			d.selected = nil
		}
	}
	a.target.SetSelected(selectedLabel)
	a.updating = false
	a.target.Disable()
	a.assign.Disable()
	a.watch.Disable()
	a.refresh.Disable()
	a.clearWatch.Hide()
	ownerLabel := taskOwnerLabel(d.task, d.targets, d.targetsCheckedAt, time.Now())
	if ownerLabel == "" {
		a.ownerStatus.Hide()
	} else {
		a.ownerStatus.SetText("Task owner: " + ownerLabel)
		a.ownerStatus.Show()
	}
	if d.requester != nil {
		a.requesterStatus.SetText(fmt.Sprintf("Results to: %s · %s · %s", d.requester.Origin.Provider, d.requester.Origin.OS, d.requester.Origin.SessionID))
		a.clearWatch.Show()
	} else {
		a.requesterStatus.SetText("No result recipient is set.")
	}
	switch {
	case d.inFlight:
		a.status.SetText("Saving task routing…")
	case d.resolving:
		a.status.SetText("Refreshing task state…")
		a.refresh.SetText("Refreshing…")
	case d.unknown:
		a.status.SetText("A result is not confirmed. Refresh task state before trying again.")
		a.refresh.SetText("Refresh task state")
		a.refresh.Enable()
	case d.loading:
		a.status.SetText("Loading live agents…")
		a.refresh.SetText("Refresh agents")
		a.refresh.Enable()
	case d.err != "":
		a.status.SetText(d.err)
		a.refresh.SetText("Refresh agents")
		a.refresh.Enable()
	case len(labels) == 0:
		a.status.SetText("No live agents are available for this task and project.")
		a.refresh.SetText("Refresh agents")
		a.refresh.Enable()
	default:
		a.status.SetText("Choose a live agent for this project.")
		a.refresh.SetText("Refresh agents")
		a.refresh.Enable()
	}
	mutationBlocked := d.inFlight || d.unknown || d.resolving
	if !mutationBlocked && !d.loading && osErr == nil && len(labels) > 0 {
		a.target.Enable()
		if d.selected != nil {
			if assignmentActive(d) {
				a.assign.Enable()
			}
			if d.requester == nil || *d.requester != *d.selected {
				a.watch.Enable()
			}
		}
	}
	if mutationBlocked {
		a.assign.Disable()
		a.watch.Disable()
		a.clearWatch.Disable()
		a.target.Disable()
	} else if d.requester != nil {
		a.clearWatch.Enable()
	}
	if !assignmentActive(d) {
		a.assign.Disable()
	}
	if osErr != nil {
		a.status.SetText("Invalid task OS requirement; correct the labels before routing.")
		a.assign.Disable()
		a.watch.Disable()
	}
}

func (a *taskAssignment) loadTargets(reconcile bool) {
	b := a.browser
	b.mu.Lock()
	project, task := b.selectedProject, b.selectedTask
	d := a.drafts[taskEditorKey(project, task)]
	if b.closing || b.ctx == nil || project == "" || task == "" || b.selectedTaskDetail == nil || b.selectedTaskDetail.Task.ID != task || d == nil || a.selectedKey != taskEditorKey(project, task) || d.inFlight || d.resolving {
		b.mu.Unlock()
		return
	}
	d.loading = !reconcile
	d.resolving = reconcile
	d.err = ""
	d.operation++
	op := d.operation
	a.refreshWidgets()
	b.mu.Unlock()
	if !b.launch(func(ctx context.Context) {
		if reconcile {
			detail, err := b.client.TaskDetail(ctx, project, task)
			b.dispatch(func() { a.finishReconcile(project, task, op, detail, err) })
			return
		}
		targets, err := b.client.Targets(ctx, project)
		b.dispatch(func() { a.finishTargets(project, task, op, targets, err) })
	}) {
		a.finishTargets(project, task, op, nil, fmt.Errorf("browser is closed"))
	}
}

func (a *taskAssignment) finishTargets(project, task string, op uint64, targets []storage.RegisteredTarget, err error) {
	b := a.browser
	b.mu.Lock()
	d := a.drafts[taskEditorKey(project, task)]
	valid := !b.closing && d != nil && d.operation == op && d.loading
	if valid {
		d.loading = false
		if err != nil {
			d.targetsCheckedAt = time.Time{}
			d.err = "Live agents unavailable: " + err.Error()
		} else {
			d.targets, d.targetsLoaded, d.err = targets, true, ""
			d.targetsCheckedAt = time.Now()
		}
		if b.selectedProject == project && b.selectedTask == task && a.selectedKey == taskEditorKey(project, task) {
			a.refreshWidgets()
		}
	}
	b.mu.Unlock()
}

func (a *taskAssignment) refreshState() {
	d := a.currentDraft()
	if d == nil || d.inFlight || d.resolving {
		return
	}
	if d.unknown && (d.operationKind == assignmentOperationWatch || d.operationKind == assignmentOperationClearWatch) {
		a.resolveRequester()
		return
	}
	a.loadTargets(d.unknown)
}

func (a *taskAssignment) saveRequester() {
	d := a.currentDraft()
	if d == nil || d.selected == nil || d.requester != nil && *d.requester == *d.selected {
		return
	}
	target := *d.selected
	a.changeRequester(&target)
}

func (a *taskAssignment) clearRequester() { a.changeRequester(nil) }

func (a *taskAssignment) changeRequester(target *storage.RegisteredTargetIdentity) {
	b := a.browser
	b.mu.Lock()
	project, task := b.selectedProject, b.selectedTask
	d := a.drafts[taskEditorKey(project, task)]
	if b.closing || b.ctx == nil || project == "" || task == "" || b.selectedTaskDetail == nil || b.selectedTaskDetail.Task.ID != task || a.selectedKey != taskEditorKey(project, task) || d == nil || !d.hasTask || d.inFlight || d.unknown || d.resolving || d.operationKind != assignmentOperationNone {
		b.mu.Unlock()
		return
	}
	if target == nil {
		if d.requester == nil {
			b.mu.Unlock()
			return
		}
		d.operationKind = assignmentOperationClearWatch
	} else {
		now := time.Now().UnixMilli()
		requiredOS, osErr := taskplatform.RequiredOS(d.task.Labels)
		available := false
		for _, entry := range d.targets {
			if entry.Target == *target && entry.SeenAt > 0 && entry.ExpiresAt > now && entry.ExpiresAt > entry.SeenAt && osErr == nil && (requiredOS == "" || target.Origin.OS == requiredOS) {
				available = true
				break
			}
		}
		if !available {
			d.selected = nil
			d.targetsLoaded = false
			d.err = "Result recipient is unavailable or incompatible. Refresh agents and select again."
			a.refreshWidgets()
			b.mu.Unlock()
			return
		}
		copy := *target
		target = &copy
		d.operationKind = assignmentOperationWatch
	}
	d.inFlight = true
	d.unknown = false
	d.resolving = false
	d.err = ""
	d.watchTarget = target
	d.operation++
	op := d.operation
	kind := d.operationKind
	a.refreshWidgets()
	b.mu.Unlock()
	if !b.launch(func(ctx context.Context) {
		result, err := b.client.SetTaskRequester(ctx, project, task, target, nil)
		b.dispatch(func() { a.finishRequester(project, task, op, kind, target, result, err) })
	}) {
		a.finishRequester(project, task, op, kind, target, nil, fmt.Errorf("browser is closed"))
	}
}

func requesterMatches(task storage.Task, target *storage.RegisteredTargetIdentity) bool {
	actual := requesterFromTask(task)
	if target == nil {
		return actual == nil
	}
	return actual != nil && *actual == *target
}

func (a *taskAssignment) finishRequester(project, task string, op uint64, kind assignmentOperationKind, target *storage.RegisteredTargetIdentity, result *storage.Task, err error) {
	b := a.browser
	b.mu.Lock()
	d := a.drafts[taskEditorKey(project, task)]
	if b.closing || d == nil || !d.inFlight || d.operation != op || d.operationKind != kind {
		b.mu.Unlock()
		return
	}
	d.inFlight = false
	selectedNow := b.selectedProject == project && b.selectedTask == task && a.selectedKey == taskEditorKey(project, task)
	if err != nil {
		if strings.Contains(err.Error(), "requester not confirmed") || strings.Contains(err.Error(), "requester resource not confirmed") {
			d.unknown = true
			d.err = ""
		} else {
			d.unknown = false
			d.operationKind = assignmentOperationNone
			d.watchTarget = nil
			d.err = err.Error()
		}
		if selectedNow {
			a.refreshWidgets()
		}
		b.mu.Unlock()
		return
	}
	if result == nil || result.ID != task || result.ProjectIdent != project || !requesterMatches(*result, target) {
		d.unknown = true
		d.err = "Requester receipt did not confirm the selected result recipient."
		if selectedNow {
			a.refreshWidgets()
		}
		b.mu.Unlock()
		return
	}
	d.task, d.hasTask = *result, true
	d.requester = requesterFromTask(*result)
	d.inFlight, d.unknown, d.resolving = false, false, false
	d.operationKind = assignmentOperationNone
	d.watchTarget = nil
	d.err = "Result recipient saved."
	if target == nil {
		d.err = "Result recipient cleared."
	}
	if selectedNow {
		a.refreshWidgets()
	}
	b.mu.Unlock()
	if selectedNow {
		b.refreshTaskAfterMutation(project, task)
	}
}

func (a *taskAssignment) resolveRequester() {
	b := a.browser
	b.mu.Lock()
	project, task := b.selectedProject, b.selectedTask
	d := a.drafts[taskEditorKey(project, task)]
	if b.closing || b.ctx == nil || project == "" || task == "" || d == nil || !d.unknown || (d.operationKind != assignmentOperationWatch && d.operationKind != assignmentOperationClearWatch) || d.resolving {
		b.mu.Unlock()
		return
	}
	d.resolving = true
	d.err = ""
	d.operation++
	op := d.operation
	kind := d.operationKind
	a.refreshWidgets()
	b.mu.Unlock()
	if !b.launch(func(ctx context.Context) {
		detail, err := b.client.TaskDetail(ctx, project, task)
		b.dispatch(func() { a.finishRequesterReconcile(project, task, op, kind, detail, err) })
	}) {
		a.finishRequesterReconcile(project, task, op, kind, nil, fmt.Errorf("browser is closed"))
	}
}

func (a *taskAssignment) finishRequesterReconcile(project, task string, op uint64, kind assignmentOperationKind, detail *storage.TaskDetail, err error) {
	b := a.browser
	b.mu.Lock()
	d := a.drafts[taskEditorKey(project, task)]
	if b.closing || d == nil || d.operation != op || d.operationKind != kind || !d.resolving {
		b.mu.Unlock()
		return
	}
	d.resolving = false
	if err != nil || detail == nil || detail.Task.ID != task || detail.Task.ProjectIdent != project {
		d.unknown = true
		d.err = "Task refresh failed; requester outcome remains unknown. " + errorText(err)
	} else {
		confirmed := requesterMatches(detail.Task, d.watchTarget)
		d.task, d.hasTask = detail.Task, true
		d.requester = requesterFromTask(detail.Task)
		d.unknown = false
		d.operationKind = assignmentOperationNone
		d.watchTarget = nil
		if confirmed {
			d.err = "Result recipient change confirmed."
		} else {
			d.err = "Task refreshed. The result recipient did not change; inspect the task before saving again."
		}
	}
	selectedNow := b.selectedProject == project && b.selectedTask == task && a.selectedKey == taskEditorKey(project, task)
	if selectedNow {
		a.refreshWidgets()
	}
	b.mu.Unlock()
	if selectedNow && err == nil && detail != nil {
		b.refreshTaskAfterMutation(project, task)
	}
}

func (a *taskAssignment) finishReconcile(project, task string, op uint64, detail *storage.TaskDetail, err error) {
	b := a.browser
	b.mu.Lock()
	d := a.drafts[taskEditorKey(project, task)]
	valid := !b.closing && d != nil && d.operation == op && d.resolving && d.operationKind == assignmentOperationAssign
	if !valid {
		b.mu.Unlock()
		return
	}
	d.resolving = false
	if err != nil || detail == nil || detail.Task.ID != task || detail.Task.ProjectIdent != project {
		d.unknown = true
		d.err = "Task refresh failed; assignment outcome remains unknown. " + errorText(err)
	} else {
		d.task, d.hasTask = detail.Task, true
		d.requester = requesterFromTask(detail.Task)
		d.unknown, d.err = false, ""
		d.operationKind = assignmentOperationNone
		d.targetsLoaded = false
		d.selected = nil
	}
	selectedNow := b.selectedProject == project && b.selectedTask == task && a.selectedKey == taskEditorKey(project, task)
	if selectedNow {
		a.refreshWidgets()
	}
	b.mu.Unlock()
	if selectedNow && err == nil && detail != nil {
		b.refreshTaskAfterMutation(project, task)
		a.loadTargets(false)
	}
}

func (a *taskAssignment) submit() {
	b := a.browser
	b.mu.Lock()
	project, task := b.selectedProject, b.selectedTask
	d := a.drafts[taskEditorKey(project, task)]
	if b.closing || b.ctx == nil || project == "" || task == "" || b.selectedTaskDetail == nil || b.selectedTaskDetail.Task.ID != task || a.selectedKey != taskEditorKey(project, task) || !assignmentActive(d) || d.selected == nil || d.inFlight || d.unknown || d.resolving {
		b.mu.Unlock()
		return
	}
	selected := *d.selected
	now := time.Now().UnixMilli()
	requiredOS, osErr := taskplatform.RequiredOS(d.task.Labels)
	exact := false
	for _, entry := range d.targets {
		if entry.Target == selected && entry.SeenAt > 0 && entry.ExpiresAt > now && entry.ExpiresAt > entry.SeenAt && osErr == nil && (requiredOS == "" || entry.Target.Origin.OS == requiredOS) {
			exact = true
			break
		}
	}
	if !exact {
		d.selected = nil
		d.targetsLoaded = false
		d.err = "That agent is no longer available or eligible. Refresh agents and choose again."
		a.refreshWidgets()
		b.mu.Unlock()
		return
	}
	d.inFlight = true
	d.operationKind = assignmentOperationAssign
	d.err = ""
	d.operation++
	op := d.operation
	a.refreshWidgets()
	b.mu.Unlock()
	if !b.launch(func(ctx context.Context) {
		result, err := b.client.AssignTask(ctx, project, task, selected)
		b.dispatch(func() { a.finishAssignment(project, task, op, selected, result, err) })
	}) {
		a.finishAssignment(project, task, op, selected, nil, fmt.Errorf("browser is closed"))
	}
}

func (a *taskAssignment) finishAssignment(project, task string, op uint64, target storage.RegisteredTargetIdentity, result *storage.Task, err error) {
	b := a.browser
	b.mu.Lock()
	d := a.drafts[taskEditorKey(project, task)]
	if b.closing || d == nil || !d.inFlight || d.operation != op || d.operationKind != assignmentOperationAssign {
		b.mu.Unlock()
		return
	}
	d.inFlight = false
	selectedNow := b.selectedProject == project && b.selectedTask == task && a.selectedKey == taskEditorKey(project, task)
	if err != nil {
		if strings.Contains(err.Error(), "assignment not confirmed") || strings.Contains(err.Error(), "assignment receipt") {
			d.unknown = true
			d.err = ""
		} else {
			d.err = err.Error()
			d.operationKind = assignmentOperationNone
		}
		if selectedNow {
			a.refreshWidgets()
		}
		b.mu.Unlock()
		return
	}
	if result == nil || result.ID != task || result.ProjectIdent != project || result.Status != "todo" || result.OwnerOrigin == nil || *result.OwnerOrigin != target.Origin {
		d.unknown = true
		d.err = "Gateway receipt did not confirm the selected agent."
		if selectedNow {
			a.refreshWidgets()
		}
		b.mu.Unlock()
		return
	}
	d.task, d.hasTask = *result, true
	d.requester = requesterFromTask(*result)
	d.unknown, d.err = false, "Task assigned to "+target.Origin.SessionID
	d.operationKind = assignmentOperationNone
	d.selected, d.targetsLoaded = nil, false
	if selectedNow {
		a.refreshWidgets()
	}
	b.mu.Unlock()
	if selectedNow {
		b.refreshTaskAfterMutation(project, task)
		b.refreshTasksAfterMutation(project)
	}
}
