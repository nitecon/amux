package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
)

const releaseTaskTitle = "Prepare and publish a new release"
const releaseTaskSpecification = `Prepare and publish a new release for this project using its documented release process.

Honor the additional notes below, including any changes or tickets that must land before releasing.

Verify that all CI for the recent changes and final release commit is green; wait for pending checks and resolve failures before tagging. Update all required versions consistently, including manifests, lockfiles, containers and packager metadata where applicable.

Once verified, cut the new release, publish it with an accurate changelog, and verify that release builds and publishing complete successfully with the expected artifacts. Keep this task in progress until verification is complete.

Additional notes / directives (optional):
`
const releaseTaskStatus = "Review the release request and add any notes before submitting."

type taskCreateState uint8

const (
	taskCreateReady taskCreateState = iota
	taskCreatePending
	taskCreateAccepted
	taskCreateUnknown
)

type taskCreateDraft struct {
	title, specification, goal string
	status                     string
	createdTaskID              string
	state                      taskCreateState
	inFlight                   bool
	revision                   uint64
	submittedRevision          uint64
	operationGeneration        uint64
	routing                    gatewayclient.TaskRouting
	targets                    []storage.RegisteredTarget
	targetsLoaded              bool
	targetsLoading             bool
	targetsCheckedAt           time.Time
	targetsStatus              string
	targetsReadGeneration      uint64
}

type taskCommentDraft struct {
	content             string
	status              string
	inFlight            bool
	refreshing          bool
	unknown             bool
	revision            uint64
	submittedRevision   uint64
	operationGeneration uint64
	refreshGeneration   uint64
}

type taskComposer struct {
	browser *Browser

	createTitle, createSpec, createGoal *widget.Entry
	createSubmit                        *widget.Button
	releasePrepare                      *widget.Button
	createStatus                        *widget.Label
	createdLabel                        *widget.Label
	createdNotice                       *fyne.Container
	submitAnother                       *widget.Button
	onCreated                           func()
	routingSelect                       *widget.Select
	routingRefresh                      *widget.Button
	routingStatus                       *widget.Label
	commentEntry                        *widget.Entry
	commentSubmit                       *widget.Button
	commentRefresh                      *widget.Button
	commentStatus                       *widget.Label

	createDrafts      map[string]*taskCreateDraft
	workspaceReceipts map[string]string
	commentDrafts     map[string]*taskCommentDraft
	activeProject     string
	activeTask        string
	updating          bool
	nextOperation     uint64
	routingChoices    map[string]gatewayclient.TaskRouting
}

func newTaskComposer(browser *Browser) *taskComposer {
	return &taskComposer{
		browser: browser, createDrafts: make(map[string]*taskCreateDraft), workspaceReceipts: make(map[string]string),
		commentDrafts:  make(map[string]*taskCommentDraft),
		routingChoices: make(map[string]gatewayclient.TaskRouting),
	}
}

func (c *taskComposer) buildNewTaskAccordion() *widget.Accordion {
	c.createTitle = widget.NewEntry()
	c.createTitle.SetPlaceHolder("Task title")
	c.createSpec = widget.NewMultiLineEntry()
	c.createSpec.SetPlaceHolder("Specification")
	c.createSpec.Wrapping = fyne.TextWrapWord
	c.createGoal = widget.NewEntry()
	c.createGoal.SetPlaceHolder("Optional goal")
	c.createStatus = widget.NewLabel("")
	c.createStatus.Wrapping = fyne.TextWrapWord
	c.routingSelect = widget.NewSelect(nil, c.selectRouting)
	c.routingSelect.PlaceHolder = "Automatic project routing"
	c.routingRefresh = widget.NewButton("Refresh recipients", c.refreshRoutingTargets)
	c.routingStatus = widget.NewLabel("")
	c.routingStatus.Wrapping = fyne.TextWrapWord
	c.createSubmit = widget.NewButton("Create task", c.handleCreateButton)
	c.releasePrepare = widget.NewButton("New Release", c.prepareReleaseTask)
	c.createdLabel = widget.NewLabel("")
	c.createdLabel.Wrapping = fyne.TextWrapWord
	c.submitAnother = widget.NewButton("Submit a new task", c.newTaskRequest)
	c.createdNotice = container.NewVBox(c.createdLabel, c.submitAnother)
	c.createdNotice.Hide()
	c.createTitle.OnChanged = c.captureCreateTitle
	for _, entry := range []*widget.Entry{c.createSpec, c.createGoal} {
		entry.OnChanged = func(string) { c.captureCreateDraft() }
	}
	form := container.NewVBox(
		c.createTitle, c.createSpec, c.createGoal,
		c.routingSelect, container.NewHBox(c.routingRefresh), c.routingStatus,
		c.createSubmit, c.createStatus,
	)
	accordion := widget.NewAccordion(widget.NewAccordionItem("New task", form))
	accordion.MultiOpen = false
	c.refreshWidgets()
	return accordion
}

func (c *taskComposer) buildCommentComposer() fyne.CanvasObject {
	c.commentEntry = widget.NewMultiLineEntry()
	c.commentEntry.SetPlaceHolder("Write a task comment")
	c.commentEntry.Wrapping = fyne.TextWrapWord
	c.commentEntry.SetMinRowsVisible(2)
	c.commentStatus = widget.NewLabel("")
	c.commentStatus.Wrapping = fyne.TextWrapWord
	c.commentSubmit = widget.NewButton("Post comment", c.submitComment)
	c.commentSubmit.Importance = widget.HighImportance
	c.commentRefresh = widget.NewButton("Refresh task before retrying", c.refreshCommentOutcome)
	c.commentRefresh.Hide()
	c.commentEntry.OnChanged = func(string) { c.captureCommentDraft() }
	c.refreshWidgets()
	return container.NewVBox(container.NewBorder(nil, nil, nil, container.NewVBox(c.commentSubmit, c.commentRefresh), c.commentEntry), c.commentStatus)
}

func (c *taskComposer) createDraft(project string) *taskCreateDraft {
	return c.createDraftFor(c.workspaceID(), project)
}

func (c *taskComposer) createDraftFor(workspaceID, project string) *taskCreateDraft {
	key := workspaceID + "\x00" + project
	draft := c.createDrafts[key]
	if draft == nil {
		draft = &taskCreateDraft{routing: gatewayclient.TaskRouting{Mode: gatewayclient.TaskRoutingAuto}}
		c.createDrafts[key] = draft
	}
	return draft
}

func (c *taskComposer) workspaceID() string {
	if c.browser.activeWorkspaceID != "" {
		return c.browser.activeWorkspaceID
	}
	return "legacy:" + c.browser.prefKey
}

func (c *taskComposer) receiptDraft(workspaceID string) *taskCreateDraft {
	key := c.workspaceReceipts[workspaceID]
	if key == "" {
		return nil
	}
	return c.createDrafts[key]
}

func (c *taskComposer) hasUnresolvedReceipt(workspaceID string) bool {
	draft := c.receiptDraft(workspaceID)
	return draft != nil && (draft.inFlight || draft.state == taskCreatePending || draft.state == taskCreateUnknown)
}

func (c *taskComposer) removeWorkspaceDrafts(workspaceID string) {
	prefix := workspaceID + "\x00"
	for key := range c.createDrafts {
		if strings.HasPrefix(key, prefix) {
			delete(c.createDrafts, key)
		}
	}
	delete(c.workspaceReceipts, workspaceID)
}

func (c *taskComposer) commentDraft(project, task string) *taskCommentDraft {
	key := taskEditorKey(project, task)
	draft := c.commentDrafts[key]
	if draft == nil {
		draft = &taskCommentDraft{}
		c.commentDrafts[key] = draft
	}
	return draft
}

func (c *taskComposer) selectProject(project string) {
	c.activeProject, c.activeTask = project, ""
	draft := c.createDraft(project)
	c.updating = true
	c.createTitle.SetText(draft.title)
	c.createSpec.SetText(draft.specification)
	c.createGoal.SetText(draft.goal)
	c.updating = false
	c.refreshWidgets()
	if project != "" {
		if !draft.targetsLoading && !c.routingFresh(draft) {
			c.loadRoutingTargets()
		}
	} else if c.routingSelect != nil {
		c.routingSelect.Options = []string{"Automatic project routing"}
		c.routingSelect.SetSelected("Automatic project routing")
		c.routingSelect.Disable()
		c.routingRefresh.Disable()
		c.routingStatus.SetText("")
	}
}

func (c *taskComposer) selectRouting(label string) {
	if c.updating {
		return
	}
	draft := c.createDraft(c.activeProject)
	if draft.inFlight || draft.state != taskCreateReady {
		c.refreshWidgets()
		return
	}
	routing, ok := c.routingChoices[label]
	if !ok {
		return
	}
	draft.routing = cloneTaskRouting(routing)
	draft.revision++
	draft.status = ""
	c.refreshWidgets()
}

func cloneTaskRouting(routing gatewayclient.TaskRouting) gatewayclient.TaskRouting {
	if routing.Target != nil {
		target := *routing.Target
		routing.Target = &target
	}
	return routing
}

func taskRoutingLabel(draft *taskCreateDraft) string {
	routing := draft.routing
	switch routing.Mode {
	case gatewayclient.TaskRoutingUnassigned:
		return "Queue unassigned"
	case gatewayclient.TaskRoutingBroadcast:
		return "Broadcast to workspace agents"
	case gatewayclient.TaskRoutingSession:
		if routing.Target != nil {
			for _, entry := range draft.targets {
				if entry.Target == *routing.Target {
					return registeredTargetLabel(entry)
				}
			}
			return "Unavailable execution · " + routing.Target.Origin.Provider + " · " + routing.Target.Origin.SessionID + " · " + routing.Target.Generation
		}
		return "Unavailable execution"
	default:
		return "Automatic project routing"
	}
}

func (c *taskComposer) refreshRoutingTargets() {
	c.loadRoutingTargets()
}

func (c *taskComposer) loadRoutingTargets() {
	b := c.browser
	project := c.activeProject
	if project == "" {
		return
	}
	b.mu.Lock()
	workspaceID := c.workspaceID()
	bindingGeneration := b.workspaceBindingGeneration[workspaceID]
	if b.closing || b.ctx == nil || b.selectedProject != project {
		b.mu.Unlock()
		return
	}
	var remote *string
	for _, item := range b.projectItems {
		if item.Ident == project && item.ArchivedAt == nil {
			remote = item.CanonicalRemote
			break
		}
	}
	draft := c.createDraftFor(workspaceID, project)
	draft.targetsLoading = true
	draft.targetsStatus = "Loading live recipients…"
	draft.targetsReadGeneration++
	readGeneration := draft.targetsReadGeneration
	b.mu.Unlock()
	c.refreshRoutingWidgets()
	if !b.launch(func(ctx context.Context) {
		targets, err := b.client.Targets(ctx, project)
		b.dispatch(func() {
			c.finishRoutingTargets(workspaceID, project, bindingGeneration, readGeneration, remote, targets, err)
		})
	}) {
		c.finishRoutingTargets(workspaceID, project, bindingGeneration, readGeneration, remote, nil, fmt.Errorf("browser is closed"))
	}
}

func (c *taskComposer) finishRoutingTargets(workspaceID, project string, bindingGeneration, readGeneration uint64, remote *string, targets []storage.RegisteredTarget, err error) {
	b := c.browser
	b.mu.Lock()
	closed := b.closing
	activeSelection := !closed && b.activeWorkspaceID == workspaceID && b.selectedProject == project
	workspaceCurrent := !closed && b.hasWorkspace(workspaceID) && b.workspaceBindingGeneration[workspaceID] == bindingGeneration && b.prefs.Selections[workspaceSelectionKey(b.prefKey, workspaceID)].Project == project
	currentRemote := false
	for _, item := range b.projectItems {
		if item.Ident == project && item.ArchivedAt == nil && sameRemote(item.CanonicalRemote, remote) {
			currentRemote = true
			break
		}
	}
	b.mu.Unlock()
	if closed || !workspaceCurrent {
		return
	}
	draft := c.createDraftFor(workspaceID, project)
	if draft.targetsReadGeneration != readGeneration || !draft.targetsLoading {
		return
	}
	draft.targetsLoading = false
	if !currentRemote {
		draft.targetsStatus = "Project changed; refresh recipients before choosing a route."
		draft.targetsLoaded = false
	} else if err != nil {
		draft.targetsStatus = "Recipients unavailable; automatic routing is still available. " + err.Error()
		draft.targetsLoaded = false
	} else {
		draft.targets, draft.targetsLoaded = targets, true
		draft.targetsCheckedAt = time.Now()
		draft.targetsStatus = fmt.Sprintf("%d live recipients for this project.", len(targets))
	}
	if activeSelection && c.activeProject == project {
		c.refreshRoutingWidgets()
	}
}

func sameRemote(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

const taskRoutingFreshness = time.Minute

func (c *taskComposer) routingFresh(draft *taskCreateDraft) bool {
	return draft != nil && draft.targetsLoaded && time.Since(draft.targetsCheckedAt) < taskRoutingFreshness
}

func (c *taskComposer) routingAvailable(draft *taskCreateDraft, now time.Time) bool {
	if draft.routing.Mode == "" || draft.routing.Mode == gatewayclient.TaskRoutingAuto {
		return true
	}
	if draft.targetsLoading || !c.routingFresh(draft) {
		return false
	}
	if draft.routing.Mode != gatewayclient.TaskRoutingSession || draft.routing.Target == nil {
		return draft.routing.Mode == gatewayclient.TaskRoutingUnassigned || draft.routing.Mode == gatewayclient.TaskRoutingBroadcast
	}
	for _, entry := range draft.targets {
		if entry.Target == *draft.routing.Target && entry.SeenAt > 0 && entry.ExpiresAt > now.UnixMilli() && entry.ExpiresAt > entry.SeenAt {
			return true
		}
	}
	return false
}

func (c *taskComposer) refreshRoutingWidgets() {
	if c.routingSelect == nil || c.activeProject == "" {
		return
	}
	draft := c.createDraft(c.activeProject)
	c.routingChoices = make(map[string]gatewayclient.TaskRouting)
	options := []string{}
	auto := gatewayclient.TaskRouting{Mode: gatewayclient.TaskRoutingAuto}
	options = append(options, addTaskRoutingChoice(c.routingChoices, "Automatic project routing", auto))
	if c.routingFresh(draft) {
		options = append(options,
			addTaskRoutingChoice(c.routingChoices, "Queue unassigned", gatewayclient.TaskRouting{Mode: gatewayclient.TaskRoutingUnassigned}),
			addTaskRoutingChoice(c.routingChoices, "Broadcast to workspace agents", gatewayclient.TaskRouting{Mode: gatewayclient.TaskRoutingBroadcast}),
		)
		now := time.Now().UnixMilli()
		for _, entry := range draft.targets {
			if entry.SeenAt <= 0 || entry.ExpiresAt <= now || entry.ExpiresAt <= entry.SeenAt {
				continue
			}
			routing := gatewayclient.TaskRouting{Mode: gatewayclient.TaskRoutingSession, Target: &entry.Target}
			label := registeredTargetLabel(entry)
			options = append(options, addTaskRoutingChoice(c.routingChoices, label, routing))
		}
	}
	selected := taskRoutingLabel(draft)
	if _, found := c.routingChoices[selected]; !found {
		options = append(options, addTaskRoutingChoice(c.routingChoices, selected, draft.routing))
	}
	c.updating = true
	c.routingSelect.Options = options
	c.routingSelect.SetSelected(selected)
	c.updating = false
	c.routingSelect.Disable()
	c.routingRefresh.Disable()
	if !draft.inFlight && draft.state == taskCreateReady {
		c.routingRefresh.Enable()
		c.routingSelect.Enable()
	}
	status := draft.targetsStatus
	if draft.routing.Mode == gatewayclient.TaskRoutingSession && !c.routingAvailable(draft, time.Now()) {
		status = "Selected execution is no longer live; refresh recipients or choose another route."
	} else if draft.routing.Mode != gatewayclient.TaskRoutingAuto && !c.routingFresh(draft) {
		status = "Refresh recipients before submitting an explicit route."
	}
	c.routingStatus.SetText(status)
	if c.createSubmit != nil {
		receipt := c.receiptDraft(c.workspaceID())
		if receipt == nil || receipt.state == taskCreateReady {
			if draft.inFlight || (draft.state == taskCreateReady && !c.routingAvailable(draft, time.Now())) {
				c.createSubmit.Disable()
			}
		}
	}
}

func addTaskRoutingChoice(choices map[string]gatewayclient.TaskRouting, label string, routing gatewayclient.TaskRouting) string {
	choices[label] = cloneTaskRouting(routing)
	return label
}

func (c *taskComposer) selectTask(project, task string) {
	c.activeProject, c.activeTask = project, task
	draft := c.commentDraft(project, task)
	c.updating = true
	c.commentEntry.SetText(draft.content)
	c.updating = false
	c.refreshWidgets()
}

func (c *taskComposer) captureCreateDraft() {
	if c.updating {
		return
	}
	draft := c.createDraft(c.activeProject)
	title, specification, goal := c.createTitle.Text, c.createSpec.Text, c.createGoal.Text
	if draft.title != title || draft.specification != specification || draft.goal != goal {
		draft.title, draft.specification, draft.goal = title, specification, goal
		draft.revision++
		if draft.state == taskCreateReady {
			draft.status = ""
		}
	}
	c.refreshWidgets()
}

func (c *taskComposer) captureCreateTitle(title string) {
	if c.updating {
		return
	}
	if len(title) > 1024 {
		draft := c.createDraft(c.activeProject)
		c.updating = true
		c.createTitle.SetText(draft.title)
		c.updating = false
		draft.status = "Task title is too long"
		c.refreshWidgets()
		return
	}
	c.captureCreateDraft()
}

func (c *taskComposer) captureCommentDraft() {
	if c.updating {
		return
	}
	draft := c.commentDraft(c.activeProject, c.activeTask)
	if draft.content != c.commentEntry.Text {
		draft.content = c.commentEntry.Text
		draft.revision++
		if !draft.unknown && !draft.inFlight {
			draft.status = ""
		}
	}
	c.refreshWidgets()
}

// refreshWidgets updates enablement and operation feedback without replacing
// text currently being edited.
func (c *taskComposer) refreshWidgets() {
	if c.createTitle == nil || c.commentEntry == nil {
		return
	}
	create := c.createDraft(c.activeProject)
	workspaceID := c.workspaceID()
	receipt := c.receiptDraft(workspaceID)
	if receipt != nil && receipt.state == taskCreateReady {
		receipt = nil
	}
	shown := create
	if receipt != nil {
		shown = receipt
	}
	if c.activeProject != "" && shown.state == taskCreateAccepted {
		c.createdLabel.SetText(shown.status)
		c.createdNotice.Show()
	} else {
		c.createdNotice.Hide()
	}
	if c.activeProject == "" {
		c.createTitle.Disable()
		c.createSpec.Disable()
		c.createGoal.Disable()
		c.createSubmit.Disable()
		if receipt != nil {
			c.createStatus.SetText(receipt.status)
		} else {
			c.createStatus.SetText("")
		}
	} else {
		c.createTitle.Enable()
		c.createSpec.Enable()
		c.createGoal.Enable()
		c.createStatus.SetText(shown.status)
		switch shown.state {
		case taskCreatePending:
			c.createSubmit.SetText("Creating…")
			c.createSubmit.Disable()
		case taskCreateAccepted:
			c.createSubmit.SetText("Submit a new task")
			c.createSubmit.Enable()
		case taskCreateUnknown:
			c.createSubmit.SetText("New task request")
			c.createSubmit.Enable()
		default:
			c.createSubmit.SetText("Create task")
			if create.inFlight || validateCreateDraft(create) != "" {
				c.createSubmit.Disable()
			} else {
				c.createSubmit.Enable()
			}
		}
	}

	comment := c.commentDraft(c.activeProject, c.activeTask)
	if c.activeProject == "" || c.activeTask == "" {
		c.commentEntry.Disable()
		c.commentSubmit.Disable()
		c.commentRefresh.Hide()
		c.commentStatus.SetText("")
		c.commentStatus.Hide()
	} else {
		c.commentEntry.Enable()
		c.commentStatus.SetText(comment.status)
		if comment.status == "" {
			c.commentStatus.Hide()
		} else {
			c.commentStatus.Show()
		}
		c.commentRefresh.Show()
		if !comment.unknown {
			c.commentRefresh.Hide()
		}
		if comment.inFlight || comment.refreshing || comment.unknown || validateCommentDraft(comment.content) != "" {
			c.commentSubmit.Disable()
		} else {
			c.commentSubmit.Enable()
		}
		if comment.refreshing {
			c.commentRefresh.SetText("Refreshing task…")
			c.commentRefresh.Disable()
		} else {
			c.commentRefresh.SetText("Refresh task before retrying")
			c.commentRefresh.Enable()
		}
	}
	if c.releasePrepare != nil {
		c.releasePrepare.Disable()
		if c.canPrepareRelease() {
			c.releasePrepare.Enable()
		}
	}
	c.refreshRoutingWidgets()
}

func (c *taskComposer) canPrepareRelease() bool {
	project := c.activeProject
	if project == "" {
		return false
	}
	workspaceID := c.workspaceID()
	if c.receiptDraft(workspaceID) != nil {
		return false
	}
	draft := c.createDraftFor(workspaceID, project)
	return !draft.inFlight && draft.state == taskCreateReady && draft.title == "" && draft.specification == "" && draft.goal == ""
}

func (c *taskComposer) prepareReleaseTask() {
	if !c.canPrepareRelease() {
		return
	}
	draft := c.createDraft(c.activeProject)
	draft.title = releaseTaskTitle
	draft.specification = releaseTaskSpecification
	draft.revision++
	draft.status = releaseTaskStatus
	c.updating = true
	c.createTitle.SetText(draft.title)
	c.createSpec.SetText(draft.specification)
	c.updating = false
	c.refreshWidgets()
}

func (c *taskComposer) handleCreateButton() {
	if receipt := c.receiptDraft(c.workspaceID()); receipt != nil && receipt.state != taskCreateReady {
		if receipt.state == taskCreatePending || receipt.inFlight {
			return
		}
		c.newTaskRequest()
		return
	}
	draft := c.createDraft(c.activeProject)
	if draft.state == taskCreateAccepted || draft.state == taskCreateUnknown {
		c.newTaskRequest()
		return
	}
	c.submitCreate()
}

func (c *taskComposer) newTaskRequest() {
	project := c.activeProject
	workspaceID := c.workspaceID()
	receiptKey := c.workspaceReceipts[workspaceID]
	draft := c.createDraft(project)
	if receiptKey != "" {
		draft = c.createDrafts[receiptKey]
	}
	if draft == nil {
		return
	}
	if draft.inFlight || draft.state == taskCreatePending {
		return
	}
	if draft.revision == draft.submittedRevision {
		draft.title, draft.specification, draft.goal = "", "", ""
		draft.revision++
		c.updating = true
		c.createTitle.SetText("")
		c.createSpec.SetText("")
		c.createGoal.SetText("")
		c.updating = false
	}
	draft.state = taskCreateReady
	draft.status = ""
	draft.createdTaskID = ""
	delete(c.workspaceReceipts, workspaceID)
	if project == "" {
		c.refreshWidgets()
		return
	}
	if receiptKey != "" && receiptKey != workspaceID+"\x00"+project {
		current := c.createDraft(project)
		c.updating = true
		c.createTitle.SetText(current.title)
		c.createSpec.SetText(current.specification)
		c.createGoal.SetText(current.goal)
		c.updating = false
	}
	c.refreshWidgets()
}

func (c *taskComposer) submitCreate() {
	b := c.browser
	project := c.activeProject
	if project == "" {
		return
	}
	workspaceID := c.workspaceID()
	if receipt := c.receiptDraft(workspaceID); receipt != nil && receipt.state != taskCreateReady {
		return
	}
	draft := c.createDraft(project)
	if draft.inFlight || draft.state != taskCreateReady {
		return
	}
	if message := validateCreateDraft(draft); message != "" {
		draft.status = "Not submitted: " + message
		c.refreshWidgets()
		return
	}
	routing := cloneTaskRouting(draft.routing)
	if routing.Mode == "" {
		routing.Mode = gatewayclient.TaskRoutingAuto
	}
	if !c.routingAvailable(draft, time.Now()) {
		draft.status = "Not submitted: refresh recipients or choose an available route."
		c.refreshWidgets()
		return
	}
	input := gatewayclient.TaskCreateInput{Title: strings.TrimSpace(draft.title)}
	if specification := strings.TrimSpace(draft.specification); specification != "" {
		input.Specification = &specification
	}
	if goal := strings.TrimSpace(draft.goal); goal != "" {
		input.Goal, _ = json.Marshal(goal)
	}
	b.mu.Lock()
	if b.closing || b.ctx == nil || b.selectedProject != project {
		b.mu.Unlock()
		return
	}
	var remote *string
	projectFound := false
	for _, item := range b.projectItems {
		if item.Ident == project && item.ArchivedAt == nil {
			projectFound = true
			if item.CanonicalRemote != nil {
				remoteValue := *item.CanonicalRemote
				remote = &remoteValue
			}
			break
		}
	}
	b.mu.Unlock()
	if !projectFound {
		draft.status = "Not submitted: the active project binding is unavailable; refresh projects first."
		c.refreshWidgets()
		return
	}
	draft.inFlight = true
	draft.state = taskCreatePending
	draft.status = "Creating task…"
	draft.submittedRevision = draft.revision
	c.nextOperation++
	draft.operationGeneration = c.nextOperation
	operation := draft.operationGeneration
	c.workspaceReceipts[workspaceID] = workspaceID + "\x00" + project
	c.refreshWidgets()
	started := b.launch(func(ctx context.Context) {
		task, err := b.client.CreateRoutedTask(ctx, project, input, remote, routing)
		b.dispatch(func() { c.finishCreate(workspaceID, project, operation, task, err) })
	})
	if !started {
		draft.inFlight = false
		draft.state = taskCreateReady
		draft.status = ""
		delete(c.workspaceReceipts, workspaceID)
		c.refreshWidgets()
	}
}

func (c *taskComposer) finishCreate(workspaceID, project string, operation uint64, task *storage.Task, err error) {
	b := c.browser
	b.mu.Lock()
	closed := b.closing
	currentProject := !closed && b.activeWorkspaceID == workspaceID && b.selectedProject == project
	workspaceExists := b.hasWorkspace(workspaceID)
	b.mu.Unlock()
	if closed || !workspaceExists || c.workspaceReceipts[workspaceID] != workspaceID+"\x00"+project {
		return
	}
	draft := c.createDraftFor(workspaceID, project)
	if draft.operationGeneration != operation || draft.state != taskCreatePending || !draft.inFlight {
		return
	}
	draft.inFlight = false
	if err == nil && (task == nil || task.ProjectIdent != project || strings.TrimSpace(task.ID) == "") {
		err = fmt.Errorf("gateway returned an invalid created task")
	}
	if err != nil {
		draft.status = "Create failed: " + err.Error()
		if postOutcomeUnknown(err) {
			draft.state = taskCreateUnknown
			draft.status = "Outcome unknown. This request was not retried. Start a new task request to continue."
		} else {
			draft.state = taskCreateReady
			delete(c.workspaceReceipts, workspaceID)
		}
	} else {
		draft.state = taskCreateAccepted
		draft.createdTaskID = task.ID
		draft.status = "Task " + task.ID + " submitted."
		// Clear the submitted inputs so they cannot be resent; edits made while
		// the request was pending are newer than the submission and are kept.
		if draft.revision == draft.submittedRevision {
			draft.title, draft.specification, draft.goal = "", "", ""
			draft.revision++
			draft.submittedRevision = draft.revision
			if currentProject {
				c.updating = true
				c.createTitle.SetText("")
				c.createSpec.SetText("")
				c.createGoal.SetText("")
				c.updating = false
			}
		}
	}
	c.refreshWidgets()
	if err == nil && currentProject {
		if c.onCreated != nil {
			c.onCreated()
		}
		b.refreshTasksAfterMutation(project)
	}
}

func (c *taskComposer) submitComment() {
	b := c.browser
	project, task := c.activeProject, c.activeTask
	if project == "" || task == "" {
		return
	}
	draft := c.commentDraft(project, task)
	if draft.inFlight || draft.refreshing || draft.unknown {
		return
	}
	if message := validateCommentDraft(draft.content); message != "" {
		draft.status = "Not submitted: " + message
		c.refreshWidgets()
		return
	}
	b.mu.Lock()
	projectGeneration, taskGeneration := b.projectGen, b.taskGen
	if b.closing || b.ctx == nil || b.selectedProject != project || b.selectedTask != task {
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	content := draft.content
	draft.inFlight = true
	draft.status = "Posting comment…"
	draft.submittedRevision = draft.revision
	c.nextOperation++
	draft.operationGeneration = c.nextOperation
	operation, revision := draft.operationGeneration, draft.revision
	c.refreshWidgets()
	started := b.launch(func(ctx context.Context) {
		comment, err := b.client.AddTaskComment(ctx, project, task, content)
		b.dispatch(func() {
			c.finishComment(project, task, projectGeneration, taskGeneration, operation, revision, comment, err)
		})
	})
	if !started {
		draft.inFlight = false
		draft.status = ""
		c.refreshWidgets()
	}
}

func (c *taskComposer) finishComment(project, task string, projectGeneration, taskGeneration, operation, revision uint64, comment *storage.TaskComment, err error) {
	b := c.browser
	b.mu.Lock()
	closed := b.closing
	currentNavigation := !closed && b.selectedProject == project && b.selectedTask == task && b.projectGen == projectGeneration && b.taskGen == taskGeneration
	b.mu.Unlock()
	if closed {
		return
	}
	draft := c.commentDraft(project, task)
	if draft.operationGeneration != operation || !draft.inFlight {
		return
	}
	draft.inFlight = false
	if err == nil && (comment == nil || comment.TaskID != task || strings.TrimSpace(comment.ID) == "") {
		err = fmt.Errorf("gateway returned an invalid task comment")
	}
	if err != nil {
		draft.status = "Post failed: " + err.Error()
		if postOutcomeUnknown(err) {
			draft.unknown = true
			draft.status = "Outcome unknown. Refresh this task before retrying; your draft is retained."
		}
	} else {
		draft.unknown = false
		draft.status = "Comment posted."
		if draft.revision == revision {
			draft.content = ""
			draft.revision++
		}
		if currentNavigation {
			c.appendComment(comment)
			b.refreshTaskAfterMutation(project, task)
			b.refreshTasksAfterMutation(project)
		}
	}
	if c.activeProject == project && c.activeTask == task {
		c.updating = true
		c.commentEntry.SetText(draft.content)
		c.updating = false
	}
	c.refreshWidgets()
}

func (c *taskComposer) refreshCommentOutcome() {
	b := c.browser
	project, task := c.activeProject, c.activeTask
	if project == "" || task == "" {
		return
	}
	draft := c.commentDraft(project, task)
	if !draft.unknown || draft.inFlight || draft.refreshing {
		return
	}
	b.mu.Lock()
	projectGeneration := b.projectGen
	if b.closing || b.ctx == nil || b.selectedProject != project || b.selectedTask != task {
		b.mu.Unlock()
		return
	}
	b.taskGen++
	b.detailLoading = false
	b.conversationsLoading = false
	taskGeneration := b.taskGen
	conversationItems := append([]storage.Conversation(nil), b.conversationItems...)
	conversationAfter, conversationsMore := b.conversationAfter, b.conversationsMore
	b.mu.Unlock()
	draft.refreshing = true
	c.nextOperation++
	draft.refreshGeneration = c.nextOperation
	generation := draft.refreshGeneration
	draft.status = "Refreshing task…"
	c.refreshWidgets()
	started := b.launch(func(ctx context.Context) {
		detail, err := b.client.TaskDetail(ctx, project, task)
		b.dispatch(func() {
			c.finishCommentRefresh(project, task, projectGeneration, taskGeneration, generation, detail, conversationItems, conversationAfter, conversationsMore, err)
		})
	})
	if !started {
		draft.refreshing = false
		draft.status = "Outcome remains unknown; refresh did not start."
		c.refreshWidgets()
	}
}

func (c *taskComposer) finishCommentRefresh(project, task string, projectGeneration, taskGeneration, generation uint64, detail *storage.TaskDetail, conversations []storage.Conversation, after string, more bool, err error) {
	b := c.browser
	b.mu.Lock()
	closed := b.closing
	currentNavigation := !closed && b.selectedProject == project && b.selectedTask == task && b.projectGen == projectGeneration && b.taskGen == taskGeneration
	b.mu.Unlock()
	if closed {
		return
	}
	draft := c.commentDraft(project, task)
	if draft.refreshGeneration != generation || !draft.refreshing {
		return
	}
	draft.refreshing = false
	if err == nil && (detail == nil || detail.Task.ID != task || detail.Task.ProjectIdent != project) {
		err = fmt.Errorf("gateway returned mismatched task details")
	}
	if err != nil {
		draft.status = "Refresh failed; outcome remains unknown: " + err.Error()
	} else {
		draft.unknown = false
		draft.status = "Task refreshed. Review its comments before retrying."
		if currentNavigation {
			b.applyTaskDetail(project, task, taskGeneration, detail, conversations, after, more, nil)
		}
	}
	if c.activeProject == project && c.activeTask == task {
		c.updating = true
		c.commentEntry.SetText(draft.content)
		c.updating = false
	}
	c.refreshWidgets()
}

func (c *taskComposer) appendComment(comment *storage.TaskComment) {
	if c.browser.selectedTaskDetail == nil || c.browser.selectedTaskDetail.Task.ID != comment.TaskID {
		return
	}
	for _, existing := range c.browser.selectedTaskDetail.Comments {
		if existing.ID == comment.ID {
			return
		}
	}
	b := c.browser
	b.selectedTaskDetail.Comments = append(b.selectedTaskDetail.Comments, *comment)
	b.selectedTaskDetail.CommentCount++
	b.showTaskComments(b.selectedTaskDetail.Comments)
}

func (b *Browser) showTaskComments(comments []storage.TaskComment) {
	b.taskComments.RemoveAll()
	if len(comments) == 0 {
		b.taskComments.Add(widget.NewLabel("No comments yet."))
		return
	}
	for _, comment := range comments {
		author := selectableLabel(comment.Author)
		author.TextStyle = fyne.TextStyle{Bold: true}
		author.Truncation = fyne.TextTruncateEllipsis
		date := selectableLabel(time.UnixMilli(comment.CreatedAt).Format("Jan 2, 2006 · 15:04"))
		date.SizeName = theme.SizeNameCaptionText
		header := container.NewBorder(nil, nil, nil, container.NewCenter(date), author)
		body := selectableLabel(comment.Content)
		body.Wrapping = fyne.TextWrapWord
		b.taskComments.Add(browserListCard(header, body))
	}
}

func validateCreateDraft(draft *taskCreateDraft) string {
	title := strings.TrimSpace(draft.title)
	if title == "" {
		return "enter a task title"
	}
	if len(title) > 1024 {
		return "task title must be at most 1024 bytes"
	}
	if len(strings.TrimSpace(draft.specification)) > 65536 {
		return "task context must be at most 65536 bytes"
	}
	if len(strings.TrimSpace(draft.goal)) > 128 {
		return "goal must be at most 128 bytes"
	}
	return ""
}

func validateCommentDraft(content string) string {
	if strings.TrimSpace(content) == "" {
		return "enter a comment"
	}
	if len(content) > 65536 {
		return "comment must be at most 65536 bytes"
	}
	return ""
}

func postOutcomeUnknown(err error) bool {
	if err == nil {
		return false
	}
	var response *gatewayclient.HTTPError
	if errors.As(err, &response) {
		return response.Status >= 500
	}
	return true // transport failures and malformed successful responses are ambiguous.
}
