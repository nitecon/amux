package desktop

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/internal/localconversation"
	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
	"github.com/nitecon/amux/worksubmission"
)

const (
	browserRequestTimeout   = 12 * time.Second
	browserConversationPage = 100
)

// BrowserConfig configures the existing-project/task/conversation browser.
type BrowserConfig struct {
	ClientConfigPath            string
	LocalConversationsDirectory string
	AutoTaskAgents              bool
	Client                      *gatewayclient.Client
	Project                     string
	PreferencesPath             string
	WorkPreferencesPath         string
	GatewaySetup                *GatewaySetupConfig
	AppControls                 fyne.CanvasObject
	Dispatch                    func(func())
	WorkerManager               *WorkerManager
	NewWindow                   func(string) fyne.Window
}

type Browser struct {
	clientConfigPath                                         string
	excludedProjects                                         map[string]bool
	client                                                   *gatewayclient.Client
	appControls                                              fyne.CanvasObject
	projectOverride, prefsPath, endpoint, prefKey, clusterID string
	dispatch                                                 func(func())
	root                                                     *container.Split
	shell                                                    *fyne.Container
	mainContent                                              *fyne.Container
	mainViews                                                map[string]fyne.CanvasObject
	mainViewButtons                                          map[string]*widget.Button
	approvalBanner                                           *fyne.Container
	approvalWatchUnavailable                                 bool
	mainView                                                 string
	taskBoard                                                *taskBoard
	taskSubtasks                                             *fyne.Container
	taskParent                                               *fyne.Container
	taskDetailScroll                                         *container.Scroll
	taskPane                                                 fyne.CanvasObject
	taskPaneHost                                             *container.Split
	taskWindow                                               fyne.Window
	taskWindowFactory                                        func(string) fyne.Window
	taskWindowToggle                                         *widget.Button
	projectSearch, taskSearch                                *widget.Entry
	workspaceName                                            *widget.Entry
	workspaceSelect                                          *widget.Select
	workspacePeerSelect                                      *widget.Select
	workspaceAdd                                             *widget.Button
	workspaceClose                                           *widget.Button
	workspacePeerClose                                       *widget.Button
	workspaceOptionIDs                                       map[string]string
	workspacePeerOptionIDs                                   map[string]string
	projects                                                 *widget.List
	tasks                                                    *widget.List
	detailHost                                               *fyne.Container
	emptyDetailPrompt                                        *fyne.Container
	taskHistory                                              *fyne.Container
	taskDetailPage, taskChatPage                             *fyne.Container
	statusSelect                                             *widget.Select
	statusShortcuts                                          map[string]*widget.Button
	taskDetailSplit                                          *container.Split
	taskMeta                                                 *widget.Label
	conversations                                            *widget.List
	projectConversationList                                  *widget.List
	localConversationsDirectory                              string
	automaticWorkerScopes                                    chan []automaticWorkerScope
	localChatSessions                                        map[string]*localChat
	approvalNotices                                          map[approvalKey]approvalNotice
	approvalRows                                             *fyne.Container
	pendingConversationProvider                              string
	activeConversationProvider                               string
	providerTabs                                             map[string]*widget.Button
	conversationAgents                                       *fyne.Container
	conversationOptions                                      []*widget.Button
	newChatButton                                            *widget.Button
	conversationHistory                                      []storage.Conversation
	projectConversationItems                                 []storage.Conversation
	defaultChats                                             map[string]storage.Conversation
	resumingConversations                                    map[string]context.CancelFunc
	openingDefaults                                          map[string]bool
	conversationSelection                                    uint64

	projectConversationsLoading   bool
	projectConversationGeneration uint64
	standaloneChatHost            *fyne.Container

	newConversationStatus *widget.Label

	alertList                                           *widget.List
	alertStatus                                         *widget.Label
	checkoutAlert                                       *widget.Label
	alertItems                                          []storage.ProjectMessageThreadSummary
	headerProject, headerGateway                        *widget.Label
	loadMoreConversations                               *widget.Button
	taskComposer                                        *taskComposer
	taskWorkflow                                        *taskWorkflow
	taskHeaderActions                                   fyne.CanvasObject
	taskAssignment                                      *taskAssignment
	workerPanel                                         *workerPanel
	worktreeStatus                                      *worktreeStatusView
	workspaceFiles                                      *workspaceFilesPane
	newTaskAccordion                                    *widget.Accordion
	pluginSettings                                      *pluginSettingsPanel
	workSubmissionSettings                              *workSubmissionSettingsPanel
	workPreferencesPath                                 string
	gatewaySettings                                     *gatewaySettingsPanel
	projectItems                                        []storage.ProjectOverview
	checkoutItems                                       []storage.CheckoutOverview
	localCheckouts                                      []projectidentity.LocalCheckout
	checkoutScan                                        uint64
	projectsDirectoryEntry                              *widget.Entry
	localCheckoutSummary                                *widget.Label
	checkoutPage                                        *fyne.Container
	workspaceIDs                                        []string
	activeWorkspaceID                                   string
	workspaceBindingGeneration                          map[string]uint64
	taskLists                                           map[string]*browserTaskListState
	updatingWorkspaceSelect                             bool
	updatingPeerWorkspaceSelect                         bool
	updatingNavigationWidgets                           bool
	visibleProjects                                     []storage.ProjectOverview
	localRailProjects                                   map[string]string
	discoveredRailProjects                              map[string]bool
	localRailSelection                                  string
	selectedProjectStatus                               *widget.Label
	taskItems                                           []storage.TaskSummary
	visibleTasks                                        []storage.TaskSummary
	conversationItems                                   []storage.Conversation
	conversationAfter                                   string
	conversationsMore, conversationsLoading             bool
	selectedProject, selectedTask, selectedConversation string
	selectedTaskDetail                                  *storage.TaskDetail
	selectedProjectFavorite                             *widget.Button
	projectLabel, taskTitle, taskSpec, browserStatus    *widget.Label
	taskComments                                        *fyne.Container
	taskEditor                                          *taskEditor
	chatHost                                            *fyne.Container
	chats                                               map[string]*UI
	chatStarted                                         map[string]bool
	prefs                                               NavigationPreferences
	selection                                           NavigationSelection

	mu                            sync.Mutex
	ctx                           context.Context
	cancel                        context.CancelFunc
	closing, started              bool
	wg                            sync.WaitGroup
	navigationInvalidations       chan struct{}
	navigationReconnecting        bool
	reviewNoticeCursors           map[string]int64
	reviewNoticesLoading          bool
	projectGen, taskGen           uint64
	projectLoading, detailLoading bool
}

// browserTaskListState retains only the task list request/snapshot for one
// workspace. It is removed with that workspace and is fenced by both its
// project binding and query authority.
type browserTaskListState struct {
	project           string
	bindingGeneration uint64
	queryGeneration   uint64
	status            string
	allStates         bool
	items             []storage.TaskSummary
	loading           bool
	refreshPending    bool
}

const (
	browserStatusActive     = "active"
	browserStatusUserReview = "user_review"
	browserStatusCompleted  = "completed"
	browserStatusTodo       = "todo"
	browserStatusInProgress = "in_progress"
	browserStatusStalled    = "stalled"
	browserStatusDone       = "done"
	browserStatusCancelled  = "cancelled"
	browserStatusAll        = "all"
)

type browserStatusOption struct {
	value string
	label string
}

var browserStatusOptions = []browserStatusOption{
	{browserStatusActive, "Active"},
	{browserStatusUserReview, "User Review"},
	{browserStatusCompleted, "Completed"},
	{browserStatusTodo, "Queued"},
	{browserStatusInProgress, "In Progress"},
	{browserStatusStalled, "Stalled"},
	{browserStatusDone, "Done"},
	{browserStatusCancelled, "Cancelled"},
	{browserStatusAll, "All"},
}

var browserStatusValues = func() []string {
	values := make([]string, len(browserStatusOptions))
	for i, option := range browserStatusOptions {
		values[i] = option.value
	}
	return values
}()

func browserStatusLabels() []string {
	labels := make([]string, len(browserStatusOptions))
	for i, option := range browserStatusOptions {
		labels[i] = option.label
	}
	return labels
}

func browserStatusValue(option string) string {
	for _, status := range browserStatusOptions {
		if option == status.label || option == status.value {
			return status.value
		}
	}
	return option
}

func browserStatusLabel(status string) string {
	for _, option := range browserStatusOptions {
		if status == option.value {
			return option.label
		}
	}
	return "Active"
}

func normalizeBrowserStatus(status string) string {
	if status == "" || !validBrowserStatus(status) {
		return browserStatusActive
	}
	return status
}

func browserTaskStatuses(status string) []string {
	switch normalizeBrowserStatus(status) {
	case browserStatusActive:
		return []string{browserStatusTodo, browserStatusInProgress, browserStatusStalled}
	case browserStatusUserReview:
		return []string{browserStatusUserReview}
	case browserStatusCompleted:
		return []string{browserStatusDone, browserStatusCancelled}
	case browserStatusAll:
		return []string{browserStatusTodo, browserStatusInProgress, browserStatusStalled, browserStatusUserReview, browserStatusDone, browserStatusCancelled}
	default:
		return []string{normalizeBrowserStatus(status)}
	}
}

func NewBrowser(config BrowserConfig) (*Browser, error) {
	if config.Client == nil {
		return nil, fmt.Errorf("project browser requires a gateway client")
	}
	configuredWorkPreferences := os.Getenv("AMUX_WORK_SUBMISSION_PREFERENCES")
	workPreferencesPath, err := worksubmission.OptionalPreferencesPath(config.WorkPreferencesPath, configuredWorkPreferences)
	if err != nil {
		return nil, fmt.Errorf("resolve work submission preferences: %w", err)
	}
	prefs := emptyNavigationPreferences()
	if config.PreferencesPath != "" {
		var err error
		prefs, err = LoadNavigationPreferences(config.PreferencesPath)
		if err != nil {
			return nil, err
		}
	}
	b := &Browser{clientConfigPath: config.ClientConfigPath, client: config.Client, localConversationsDirectory: config.LocalConversationsDirectory, localChatSessions: make(map[string]*localChat), appControls: config.AppControls, projectOverride: strings.TrimSpace(config.Project), prefsPath: config.PreferencesPath, endpoint: config.Client.Endpoint(), dispatch: config.Dispatch, taskWindowFactory: config.NewWindow, prefs: prefs, chats: map[string]*UI{}, chatStarted: map[string]bool{}, browserStatus: widget.NewLabel("Loading projects…")}
	b.taskComposer = newTaskComposer(b)
	b.worktreeStatus = newWorktreeStatusView(func() { b.refreshWorktreeStatus() })
	if b.dispatch == nil {
		b.dispatch = fyne.Do
	}
	b.prefKey = b.endpoint
	if legacy := prefs.ProjectsDirectories[b.prefKey]; prefs.ProjectsDirectory == "" && legacy != "" {
		b.prefs.ProjectsDirectory = legacy
		b.prefs.ProjectsDirectories = nil
	}

	b.clusterID = config.Client.ClusterID()
	migrateEndpointFavorites(prefs.Favorites, b.endpoint, b.clusterID)
	b.pluginSettings = newPluginSettingsPanel(b)
	b.workPreferencesPath = workPreferencesPath
	b.workSubmissionSettings = newWorkSubmissionSettingsPanel(workPreferencesPath, b)
	if config.GatewaySetup != nil {
		b.gatewaySettings = newGatewaySettingsPanel(*config.GatewaySetup)
	}
	if config.AutoTaskAgents && config.WorkerManager != nil {
		b.automaticWorkerScopes = make(chan []automaticWorkerScope, 1)
	}
	if config.WorkerManager != nil {
		b.workerPanel = newWorkerPanel(b, config.WorkerManager)
	}
	b.workspaceIDs = append([]string(nil), prefs.WorkspaceIDs[b.prefKey]...)
	if len(b.workspaceIDs) == 0 {
		id := uuid.NewString()
		b.workspaceIDs = []string{id}
		if prefs.WorkspaceIDs == nil {
			prefs.WorkspaceIDs = make(map[string][]string)
		}
		prefs.WorkspaceIDs[b.prefKey] = append([]string(nil), b.workspaceIDs...)
		legacy := prefs.Selections[b.prefKey]
		prefs.Selections[workspaceSelectionKey(b.prefKey, id)] = legacy
	}
	b.activeWorkspaceID = prefs.ActiveWorkspace[b.prefKey]
	if !b.hasWorkspace(b.activeWorkspaceID) {
		b.activeWorkspaceID = b.workspaceIDs[0]
	}
	b.workspaceBindingGeneration = make(map[string]uint64, len(b.workspaceIDs))
	b.taskLists = make(map[string]*browserTaskListState, len(b.workspaceIDs))
	for _, id := range b.workspaceIDs {
		b.workspaceBindingGeneration[id] = 1
		b.taskLists[id] = &browserTaskListState{bindingGeneration: 1, status: browserStatusActive}
	}
	b.selection = prefs.Selections[workspaceSelectionKey(b.prefKey, b.activeWorkspaceID)]
	if b.selection == (NavigationSelection{}) {
		b.selection = prefs.Selections[b.prefKey]
	}
	if b.projectOverride != "" {
		if b.selection.Project != "" && b.selection.Project != b.projectOverride {
			b.selection.Task = ""
			b.selection.Conversation = ""
			b.selection.ProjectConversation = ""
			b.selection.CheckoutPath = ""
			b.selection.CheckoutOrigin = nil
		}
		b.selection.Project = b.projectOverride
	}
	if b.workerPanel != nil {
		b.workerPanel.restoreWorkspaceCheckout()
	}
	b.prefs = prefs
	b.selection.Status = normalizeBrowserStatus(b.selection.Status)
	b.buildUI()
	b.persist()
	return b, nil
}

func (b *Browser) Content() fyne.CanvasObject { return b.shell }

func (b *Browser) toggleTaskWindow() {
	b.mu.Lock()
	closing := b.closing
	b.mu.Unlock()
	if closing {
		return
	}
	if b.taskWindow != nil {
		b.dockTaskWindow()
		return
	}
	if b.taskWindowFactory == nil || b.taskPaneHost == nil || b.taskPane == nil {
		return
	}
	window := b.taskWindowFactory("Amux · Tasks")
	if window == nil {
		return
	}
	returnButton := widget.NewButton("Show tasks here", b.dockTaskWindow)
	b.taskPaneHost.Trailing = container.NewCenter(container.NewVBox(widget.NewLabel("Tasks are open in a separate window"), returnButton))
	b.taskPaneHost.Refresh()
	b.taskWindow = window
	b.taskWindowToggle.SetText("Return tasks to main window")
	window.Resize(fyne.NewSize(800, 900))
	window.SetContent(b.taskPane)
	window.SetOnClosed(func() { b.taskWindowClosed(window) })
	window.Show()
}

func (b *Browser) taskWindowClosed(window fyne.Window) {
	b.mu.Lock()
	closing := b.closing
	b.mu.Unlock()
	if closing || b.taskWindow != window {
		return
	}
	b.taskWindow = nil
	b.taskPaneHost.Trailing = b.taskPane
	b.taskPaneHost.Refresh()
	b.taskWindowToggle.SetText("Open tasks separately")
}

func (b *Browser) dockTaskWindow() {
	window := b.taskWindow
	if window == nil {
		return
	}
	b.mu.Lock()
	closing := b.closing
	b.mu.Unlock()
	if closing {
		b.closeTaskWindow()
		return
	}
	window.SetContent(container.NewMax())
	window.Close()
}

func (b *Browser) closeTaskWindow() {
	window := b.taskWindow
	if window == nil {
		return
	}
	b.taskWindow = nil
	window.SetContent(container.NewMax())
	window.SetOnClosed(nil)
	window.Close()
}

func browserSectionHeading(label string) *canvas.Text {
	heading := canvas.NewText(label, theme.PlaceHolderColor())
	heading.TextSize = 11
	return heading
}

func browserPanel(content fyne.CanvasObject) *fyne.Container {
	background := canvas.NewRectangle(theme.InputBackgroundColor())
	background.StrokeColor = theme.SeparatorColor()
	background.StrokeWidth = 1
	return container.NewStack(background, container.NewPadded(content))
}

func browserCardSurface() *canvas.Rectangle {
	background := canvas.NewRectangle(theme.ButtonColor())
	background.CornerRadius = 7
	background.StrokeColor = theme.SeparatorColor()
	background.StrokeWidth = 1
	return background
}

func taskCardTitle() *fyne.Container {
	title := widget.NewLabel("")
	title.Wrapping = fyne.TextWrapWord
	title.Truncation = fyne.TextTruncateEllipsis
	space := canvas.NewRectangle(color.Transparent)
	lineHeight := fyne.MeasureText("M", theme.TextSize(), fyne.TextStyle{}).Height
	space.SetMinSize(fyne.NewSize(0, 2*lineHeight+2*theme.InnerPadding()))
	return container.NewStack(title, space)
}

func browserListCard(labels ...fyne.CanvasObject) *fyne.Container {
	content := container.NewVBox()
	for _, label := range labels {
		content.Add(label)
	}
	return container.NewStack(browserCardSurface(), container.NewPadded(content))
}

func updateBrowserListCard(obj fyne.CanvasObject, selected bool) *fyne.Container {
	card := obj.(*fyne.Container)
	background := card.Objects[0].(*canvas.Rectangle)
	background.FillColor = theme.ButtonColor()
	background.StrokeColor = theme.SeparatorColor()
	if selected {
		background.FillColor = theme.SelectionColor()
		background.StrokeColor = theme.PrimaryColor()
	}
	background.Refresh()
	return card
}

func browserCardObject(card *fyne.Container, index int) fyne.CanvasObject {
	content := card.Objects[1].(*fyne.Container).Objects[0].(*fyne.Container)
	return content.Objects[index]
}

func (b *Browser) buildUI() {
	b.projectSearch = widget.NewEntry()
	b.projectSearch.SetPlaceHolder("Search projects")
	b.taskSearch = widget.NewEntry()
	b.taskSearch.SetPlaceHolder("Filter by title, ID or label")
	b.projectLabel = widget.NewLabel("Select a project")
	b.projectLabel.TextStyle = fyne.TextStyle{Bold: true}
	b.taskTitle = selectableLabel("Select a task")
	b.taskTitle.SizeName = theme.SizeNameSubHeadingText
	b.taskMeta = selectableLabel("")
	b.taskMeta.Wrapping = fyne.TextWrapWord
	b.taskMeta.Hide()
	b.taskTitle.Wrapping = fyne.TextWrapWord
	b.taskSpec = selectableLabel("Task details will appear here.")
	b.taskSpec.Wrapping = fyne.TextWrapWord
	b.taskComments = container.NewVBox()
	b.taskEditor = newTaskEditor(b)
	b.taskWorkflow = newTaskWorkflow(b)
	b.taskAssignment = newTaskAssignment(b)
	b.newTaskAccordion = b.taskComposer.buildNewTaskAccordion()
	commentComposer := b.taskComposer.buildCommentComposer()
	b.browserStatus = widget.NewLabel("Ready")
	b.workspaceSelect = widget.NewSelect(nil, func(label string) {
		if b.updatingWorkspaceSelect {
			return
		}
		if id := b.workspaceOptionIDs[label]; id != "" {
			b.selectWorkspace(id)
		}
	})
	b.workspacePeerSelect = widget.NewSelect(nil, func(label string) {
		if b.updatingPeerWorkspaceSelect {
			return
		}
		id := b.workspacePeerOptionIDs[label]
		if id != "" && id != b.activeWorkspaceID {
			b.workspacePeerClose.Enable()
		} else {
			b.workspacePeerClose.Disable()
		}
	})
	b.workspacePeerSelect.PlaceHolder = "Choose inactive workspace"
	b.workspaceAdd = widget.NewButton("+ New", b.addWorkspace)
	b.workspaceClose = widget.NewButtonWithIcon("", theme.CancelIcon(), b.closeActiveWorkspace)
	b.workspaceAdd.Importance = widget.LowImportance
	b.workspaceClose.Importance = widget.LowImportance
	b.workspacePeerClose = widget.NewButton("Close peer", b.closeSelectedPeerWorkspace)
	b.workspacePeerClose.Disable()
	b.workspaceName = widget.NewEntry()
	b.workspaceName.SetPlaceHolder("Workspace name (optional)")
	b.workspaceName.SetText(b.selection.WorkspaceName)
	b.workspaceName.OnChanged = func(value string) {
		if b.updatingNavigationWidgets {
			return
		}
		b.selection.WorkspaceName = strings.TrimSpace(value)
		b.refreshWorkspaceSelector()
		b.applyProjectFilter()
		b.persist()
	}
	b.selectedProjectFavorite = widget.NewButton("Favorite", b.toggleFavorite)
	b.statusSelect = widget.NewSelect(browserStatusLabels(), nil)
	b.statusSelect.SetSelected(browserStatusLabel(b.selection.Status))
	b.statusSelect.OnChanged = b.changeStatus
	refresh := widget.NewButton("Refresh", b.Refresh)
	b.projects = widget.NewList(func() int { return len(b.visibleProjects) }, func() fyne.CanvasObject {
		name := widget.NewLabel("")
		name.Truncation = fyne.TextTruncateEllipsis
		summary := canvas.NewText("", theme.PlaceHolderColor())
		summary.TextSize = 10
		dot := canvas.NewCircle(theme.PlaceHolderColor())
		dotBox := container.NewCenter(container.NewGridWrap(fyne.NewSquareSize(7), dot))
		star := widget.NewButton("☆", nil)
		star.Importance = widget.LowImportance
		caption := container.NewBorder(nil, nil, container.NewGridWrap(fyne.NewSize(0, 0)), nil, summary)
		labels := container.New(layout.NewCustomPaddedVBoxLayout(0), name, caption)
		content := container.NewBorder(nil, nil, dotBox, container.NewCenter(star), labels)
		heading := browserSectionHeading("")
		headingBlock := container.NewStack(canvas.NewRectangle(theme.InputBackgroundColor()), heading)
		headingBlock.Hide()
		// Keep the native overlay scrollbar outside the card and star hit area.
		scrollGutter := container.NewGridWrap(fyne.NewSize(2, 0))
		return newProjectMenuRow(container.NewBorder(headingBlock, nil, nil, scrollGutter, browserListCard(content)))
	}, func(i widget.ListItemID, obj fyne.CanvasObject) {
		if i < 0 || i >= len(b.visibleProjects) {
			return
		}
		p := b.visibleProjects[i]
		row := obj.(*projectMenuRow)
		row.onMenu = func(event *fyne.PointEvent) { b.showProjectMenu(row, event, p) }
		row.onSelect = func() { b.projects.Select(i) }
		item := row.content
		headingBlock := item.Objects[1].(*fyne.Container)
		heading := headingBlock.Objects[1].(*canvas.Text)
		heading.Text = b.projectGroupAt(i)
		if heading.Text == "" {
			headingBlock.Hide()
		} else {
			headingBlock.Show()
			heading.Refresh()
		}
		card := updateBrowserListCard(item.Objects[0], p.Ident == b.selectedRailProject())
		content := browserCardObject(card, 0).(*fyne.Container)
		labels := content.Objects[0].(*fyne.Container)
		directory := b.localRailProjects[p.Ident]
		name := p.Ident
		if directory != "" {
			name = filepath.Base(directory)
		}
		labels.Objects[0].(*widget.Label).SetText(name)
		summary := labels.Objects[1].(*fyne.Container).Objects[0].(*canvas.Text)
		summary.Text = fmt.Sprintf("%d active agents", p.Agents)
		if p.PendingTasks != nil {
			summary.Text = fmt.Sprintf("%d queued · %d agents", *p.PendingTasks, p.Agents)
		}
		if directory != "" {
			summary.Text = "Local checkout"
		} else if b.projectCheckout(p).Directory != "" {
			summary.Text = "Local · " + summary.Text
		} else if b.discoveredRailProjects[p.Ident] {
			summary.Text = "Local checkout found · " + summary.Text
		}
		summary.Refresh()
		dotBox := content.Objects[1].(*fyne.Container)
		dot := dotBox.Objects[0].(*fyne.Container).Objects[0].(*canvas.Circle)
		dot.FillColor = theme.PlaceHolderColor()
		if p.Agents > 0 {
			dot.FillColor = theme.SuccessColor()
		}
		dot.Refresh()
		star := content.Objects[2].(*fyne.Container).Objects[0].(*widget.Button)
		symbol := "☆"
		star.Importance = widget.LowImportance
		if b.railProjectFavorite(p.Ident) {
			symbol = "★"
		}
		star.SetText(symbol)
		star.Refresh()
		star.OnTapped = func() { b.toggleFavoriteFor(p.Ident) }
	})
	b.projects.HideSeparators = true
	b.projects.OnSelected = func(id widget.ListItemID) {
		if id < 0 || id >= len(b.visibleProjects) {
			return
		}
		project := b.visibleProjects[id].Ident
		b.selectRailProject(project)
		b.projects.Refresh()
	}
	b.tasks = widget.NewList(func() int { return len(b.visibleTasks) }, func() fyne.CanvasObject {
		title := taskCardTitle()
		status := canvas.NewText("Status", theme.ForegroundColor())
		status.TextSize = 10
		badgeBackground := canvas.NewRectangle(theme.InputBackgroundColor())
		badgeBackground.CornerRadius = 4
		badgeBackground.StrokeWidth = 1
		badge := container.NewStack(badgeBackground, container.New(layout.NewCustomPaddedLayout(2, 2, 5, 5), status))
		id := canvas.NewText("#00000000", theme.PrimaryColor())
		id.TextSize = 10
		id.TextStyle.Monospace = true
		return container.NewBorder(nil, nil, canvas.NewRectangle(theme.InputBackgroundColor()), nil, browserListCard(container.NewBorder(nil, nil, id, badge), title))
	}, func(i widget.ListItemID, obj fyne.CanvasObject) {
		if i < 0 || i >= len(b.visibleTasks) {
			return
		}
		t := b.visibleTasks[i]
		row := obj.(*fyne.Container)
		card := updateBrowserListCard(row.Objects[0], t.ID == b.selectedTask)
		indent := row.Objects[1].(*canvas.Rectangle)
		indent.SetMinSize(fyne.NewSize(12, 0))
		if t.Parent != nil {
			indent.Show()
		} else {
			indent.Hide()
		}
		row.Refresh()
		header := browserCardObject(card, 0).(*fyne.Container)
		id := header.Objects[0].(*canvas.Text)
		badge := header.Objects[1].(*fyne.Container)
		status := badge.Objects[1].(*fyne.Container).Objects[0].(*canvas.Text)
		title := browserCardObject(card, 1).(*fyne.Container).Objects[0].(*widget.Label)
		id.Text = "#" + shortID(t.ID)
		id.Refresh()
		title.SetText(t.Title)
		status.Text = browserStatusLabel(t.Status)
		if t.Status == "" {
			status.Text = "Parent"
		}
		status.Color = taskStatusColor(t.Status)
		tint := color.NRGBAModel.Convert(status.Color).(color.NRGBA)
		tint.A = 100
		badgeBackground := badge.Objects[0].(*canvas.Rectangle)
		badgeBackground.StrokeColor = tint
		badgeBackground.Refresh()
		if t.Parent != nil {
			status.Text += " · Subtask"
		}
		status.Refresh()
		header.Refresh()
	})
	b.tasks.HideSeparators = true
	b.tasks.OnSelected = func(id widget.ListItemID) {
		if id < 0 || id >= len(b.visibleTasks) {
			return
		}
		task := b.visibleTasks[id].ID
		if task != b.selectedTask {
			b.selectTask(task)
		} else if b.taskChatPage.Visible() {
			b.backToTaskDetail()
		}
		b.tasks.Refresh()
	}
	b.conversations = widget.NewList(func() int { return len(b.conversationItems) }, func() fyne.CanvasObject { return widget.NewLabel("template") }, func(i widget.ListItemID, obj fyne.CanvasObject) {
		if i < 0 || i >= len(b.conversationItems) {
			return
		}
		c := b.conversationItems[i]
		obj.(*widget.Label).SetText(conversationLabel(c))
	})
	b.loadMoreConversations = widget.NewButton("Load more conversations", b.loadMore)
	b.loadMoreConversations.Hide()
	b.conversations.OnSelected = func(id widget.ListItemID) {
		if id < 0 || id >= len(b.conversationItems) {
			return
		}
		b.conversations.UnselectAll()
		b.openTaskThread(b.conversationItems[id])
	}
	b.projectSearch.OnChanged = func(value string) {
		if b.updatingNavigationWidgets {
			return
		}
		b.selection.ProjectFilter = value
		b.applyProjectFilter()
		b.persist()
	}
	b.projectConversationList = widget.NewList(func() int { return len(b.conversationHistory) }, func() fyne.CanvasObject {
		title := widget.NewLabel("Conversation")
		title.Truncation = fyne.TextTruncateEllipsis
		return title
	}, func(i widget.ListItemID, obj fyne.CanvasObject) {
		if i >= len(b.conversationHistory) {
			return
		}
		chat := b.conversationHistory[i]
		title := chat.TaskTitle
		if title == "" {
			title = providerConversationLabel(chat.Provider)
			if chat.TaskID != "" {
				title += " · Task"
			}
		}
		obj.(*widget.Label).SetText(title + " · " + time.UnixMilli(chat.CreatedAt).Format("Jan 2, 15:04"))
	})
	b.projectConversationList.OnSelected = func(i widget.ListItemID) {
		if i < 0 || i >= len(b.conversationHistory) {
			return
		}
		b.projectConversationList.UnselectAll()
		b.conversationSelection++
		chat := b.conversationHistory[i]
		b.openStandaloneConversation(chat)
		if chat.TaskID == "" {
			b.resumeDefaultConversation(chat)
		}
	}
	b.newConversationStatus = widget.NewLabel("")
	b.newConversationStatus.Wrapping = fyne.TextWrapWord
	b.standaloneChatHost = container.NewMax(widget.NewLabel("Select a conversation or start a new one."))
	b.alertList = widget.NewList(func() int { return len(b.alertItems) }, func() fyne.CanvasObject { return widget.NewLabel("Alert") }, func(i widget.ListItemID, obj fyne.CanvasObject) {
		if i >= 0 && i < len(b.alertItems) {
			item := b.alertItems[i]
			title := item.Preview
			if item.Subject != nil && strings.TrimSpace(*item.Subject) != "" {
				title = *item.Subject
			}
			obj.(*widget.Label).SetText(title)
		}
	})
	b.checkoutAlert = widget.NewLabel("")
	b.checkoutAlert.Wrapping = fyne.TextWrapWord
	b.checkoutAlert.Hide()
	b.alertStatus = widget.NewLabel("Select a project to view alerts.")
	b.headerProject, b.headerGateway = widget.NewLabel("Gateway project: —"), widget.NewLabel("Gateway: checking…\nThis machine: unavailable")
	b.taskSearch.OnChanged = func(value string) {
		if b.updatingNavigationWidgets {
			return
		}
		b.changeTaskSearch(value)
	}
	workspaceStyle := fyne.CurrentApp().Settings().Theme()
	controls := toolbarTheme{Theme: workspaceStyle}
	cardLists := cardListTheme{Theme: workspaceStyle}
	projectHeading := browserSectionHeading("PROJECTS")
	b.selectedProjectFavorite.Importance = widget.LowImportance
	projectHeader := container.NewVBox(projectHeading, b.projectSearch)
	b.workspaceFiles = newWorkspaceFilesPane(b.dispatch)
	b.taskWindowToggle = widget.NewButton("Open tasks separately", b.toggleTaskWindow)
	railWidth := canvas.NewRectangle(theme.InputBackgroundColor())
	railWidth.SetMinSize(fyne.NewSize(208, 0))
	projectPane := container.NewStack(railWidth, browserPanel(container.NewBorder(
		projectHeader, nil, nil, nil, container.NewThemeOverride(b.projects, cardLists),
	)))
	tasksHeading := browserSectionHeading("TASKS")
	var newTaskButton *widget.Button
	newTaskButton = widget.NewButton("New Task", func() {
		if b.newTaskAccordion.Items[0].Open {
			b.newTaskAccordion.Close(0)
			b.newTaskAccordion.Hide()
			newTaskButton.SetText("New Task")
		} else {
			b.newTaskAccordion.Open(0)
			b.newTaskAccordion.Show()
			newTaskButton.SetText("Close")
		}
	})
	b.taskComposer.releasePrepare.OnTapped = func() {
		b.taskComposer.prepareReleaseTask()
		b.newTaskAccordion.Open(0)
		b.newTaskAccordion.Show()
		newTaskButton.SetText("Close")
	}
	b.taskComposer.onCreated = func() {
		b.newTaskAccordion.Close(0)
		b.newTaskAccordion.Hide()
		newTaskButton.SetText("New Task")
	}
	b.taskComposer.submitAnother.OnTapped = func() {
		b.taskComposer.newTaskRequest()
		b.newTaskAccordion.Open(0)
		b.newTaskAccordion.Show()
		newTaskButton.SetText("Close")
	}
	newTaskButton.Importance = widget.HighImportance
	b.taskComposer.releasePrepare.Importance = widget.LowImportance
	b.statusShortcuts = make(map[string]*widget.Button)
	filters := container.NewGridWithColumns(3)
	for _, status := range []string{browserStatusActive, browserStatusUserReview, browserStatusCompleted} {
		label := browserStatusLabel(status)
		button := widget.NewButton(label, func() { b.statusSelect.SetSelected(label) })
		b.statusShortcuts[status] = button
		filters.Add(button)
	}
	b.refreshStatusShortcuts()
	var moreFilters *widget.Button
	moreFilters = widget.NewButton("⋯", func() {
		canvas := fyne.CurrentApp().Driver().CanvasForObject(moreFilters)
		if canvas == nil {
			return
		}
		menu := fyne.NewMenu("")
		for _, label := range browserStatusLabels() {
			item := fyne.NewMenuItem(label, func() { b.statusSelect.SetSelected(label) })
			item.Checked = b.statusSelect.Selected == label
			menu.Items = append(menu.Items, item)
		}
		position := fyne.CurrentApp().Driver().AbsolutePositionForObject(moreFilters)
		widget.ShowPopUpMenuAtPosition(menu, canvas, position.AddXY(0, moreFilters.Size().Height))
	})
	taskToolbar := container.NewVBox(
		b.taskSearch,
		container.NewThemeOverride(container.NewBorder(nil, nil, nil, moreFilters, filters), controls),
	)
	b.newTaskAccordion.Hide()
	taskHeader := container.NewVBox(tasksHeading, taskToolbar)
	emptyDetailLabel := widget.NewLabel("Choose a task from the queue.")
	emptyDetailLabel.Alignment = fyne.TextAlignCenter
	b.emptyDetailPrompt = container.NewCenter(emptyDetailLabel)
	taskPane := browserPanel(container.NewBorder(taskHeader, container.NewVBox(b.taskComposer.createdNotice, b.newTaskAccordion), nil, nil, container.NewThemeOverride(b.tasks, cardLists)))
	b.taskSubtasks = container.NewVBox()
	b.taskParent = container.NewVBox()
	settings := widget.NewAccordion(widget.NewAccordionItem("Task settings", container.NewVBox(b.taskAssignment.panel(), widget.NewSeparator(), b.taskEditor.form)))
	copyTask := copyTaskIDButton(func() string { return b.selectedTask })
	b.taskHeaderActions = container.NewThemeOverride(container.NewBorder(nil, nil, nil, copyTask, b.taskWorkflow.actions()), controls)
	detailsBody := container.NewVBox(container.NewThemeOverride(b.taskWorkflow.panel(), controls), b.taskParent, b.taskSubtasks, widget.NewSeparator(), browserSectionHeading("SPECIFICATION"), b.taskSpec, settings, widget.NewSeparator(), browserSectionHeading("COMMENTS"), b.taskComments)
	b.taskDetailScroll = container.NewVScroll(detailsBody)
	b.chatHost = container.NewMax(widget.NewLabel("Select an existing conversation to open its live chat."))
	conversationHeading := browserSectionHeading("CONVERSATIONS")
	conversationPanel := container.NewBorder(conversationHeading, b.loadMoreConversations, nil, nil, b.conversations)
	b.taskDetailSplit = container.NewVSplit(b.taskDetailScroll, conversationPanel)
	b.taskDetailSplit.Offset = .81
	conversationPanel.Hide()
	b.taskHistory = container.NewBorder(nil,
		container.NewVBox(widget.NewSeparator(), container.NewThemeOverride(commentComposer, controls)),
		nil, nil, b.taskDetailSplit,
	)
	detailContent := container.NewMax(b.taskHistory, b.emptyDetailPrompt)
	b.detailHost = detailContent
	b.showTaskHistory(false)
	detailHeading := browserSectionHeading("TASK DETAIL")
	refresh.Importance = widget.LowImportance
	b.taskDetailPage = container.NewPadded(container.NewBorder(container.NewVBox(container.NewBorder(nil, nil, detailHeading, container.NewThemeOverride(refresh, controls), b.projectLabel), b.taskTitle, b.taskMeta, b.taskHeaderActions), container.NewVBox(widget.NewSeparator(), b.browserStatus), nil, nil, detailContent))
	back := widget.NewButton("Back to task details", b.backToTaskDetail)
	b.taskChatPage = container.NewBorder(container.NewThemeOverride(container.NewHBox(back, copyTaskIDButton(func() string { return b.selectedTask })), controls), nil, nil, nil, b.chatHost)
	b.taskChatPage.Hide()
	right := container.NewStack(b.taskDetailPage, b.taskChatPage)
	b.taskPane = right
	b.taskPaneHost = container.NewHSplit(taskPane, right)
	b.taskPaneHost.Offset = .28
	conversationView := b.buildConversationView(controls)
	b.mainViews = map[string]fyne.CanvasObject{
		"Tasks": container.NewBorder(
			browserPanel(container.NewThemeOverride(container.NewHBox(newTaskButton, b.taskComposer.releasePrepare), controls)), nil, nil, nil, b.taskPaneHost,
		),
		"Conversations": conversationView,
		"Files":         container.NewHScroll(b.workspaceFiles.Content()),
		"Git":           container.NewBorder(nil, nil, nil, nil, container.NewVBox(browserSectionHeading("GIT"), b.worktreeStatus.accordion)),
	}
	b.taskBoard = newTaskBoard(b)
	b.mainViews["Board"] = b.taskBoard.content
	b.mainViews["Alerts"] = container.NewBorder(container.NewVBox(browserSectionHeading("ALERTS"), b.checkoutAlert, b.alertStatus), nil, nil, nil, b.alertList)
	b.mainViews["Settings"] = b.settingsView()
	b.checkoutPage = container.NewStack()
	b.mainViews["Checkout"] = b.checkoutPage
	b.mainViews["Events"] = container.NewStack()
	b.mainContent = container.NewStack(b.mainViews["Tasks"])
	b.mainViewButtons = make(map[string]*widget.Button)
	mainNav, actions := container.NewHBox(), container.NewHBox()
	for _, name := range []string{"Tasks", "Board", "Conversations", "Files", "Git", "Events", "Alerts", "Settings"} {
		viewName := name
		button := widget.NewButton(viewName, func() { b.setMainView(viewName) })
		b.mainViewButtons[viewName] = button
		switch name {
		case "Events", "Alerts", "Settings":
			actions.Add(button)
		default:
			mainNav.Add(button)
		}
	}
	brand := widget.NewLabelWithStyle("amux", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	b.headerGateway.SizeName = theme.SizeNameCaptionText
	connection := container.NewStack(browserCardSurface(), b.headerGateway)
	header := container.NewBorder(nil, nil, container.NewHBox(brand, b.headerProject), connection)
	toolbar := container.NewThemeOverride(container.NewBorder(nil, nil, mainNav, actions), controls)
	mainPanel := container.NewBorder(toolbar, nil, nil, nil, b.mainContent)
	b.root = container.NewHSplit(projectPane, mainPanel)
	b.root.Offset = .19
	b.selectedProjectStatus = widget.NewLabel("")
	b.selectedProjectStatus.Truncation = fyne.TextTruncateEllipsis
	b.selectedProjectStatus.Importance = widget.LowImportance
	statusBar := container.NewVBox(widget.NewSeparator(), b.selectedProjectStatus)
	b.approvalBanner = container.NewVBox()
	b.approvalBanner.Hide()
	b.shell = container.NewBorder(container.NewVBox(header, widget.NewSeparator(), b.approvalBanner), statusBar, nil, nil, b.root)
	b.updateSelectedProjectStatus()
	b.pluginSettings.setRoot(b.root)
	b.workSubmissionSettings.setRoot(b.root)
	b.refreshWorkspaceSelector()
	if b.workerPanel != nil {
		b.workerPanel.setRoot(b.root)
	}
	initialView := b.selection.MainView
	if initialView == "" {
		initialView = "Tasks"
	}
	b.setMainView(initialView)
}

func (b *Browser) Start(parent context.Context) {
	b.mu.Lock()
	if b.started || b.closing {
		b.mu.Unlock()
		return
	}
	b.started = true
	b.ctx, b.cancel = context.WithCancel(parent)
	ctx := b.ctx
	b.navigationInvalidations = make(chan struct{}, 1)
	b.wg.Add(2)
	b.mu.Unlock()
	b.dispatch(func() {
		b.pluginSettings.reload(false)
		b.scanProjectCheckouts()
		b.syncWorkspaceFiles()
		b.refreshWorktreeStatus()
		b.refreshOnUI()
	})
	if b.workerPanel != nil {
		if manager, ok := b.workerPanel.manager.(*WorkerManager); ok {
			manager.open = b.watchApprovals(manager.open, nil)
		}
		startErr := b.workerPanel.manager.Start(ctx)
		b.dispatch(func() { b.workerPanel.lifetimeStarted(ctx, startErr) })
	}
	if b.automaticWorkerScopes != nil {
		manager := b.workerPanel.manager.(*WorkerManager)
		b.launchWithTimeout(0, func(ctx context.Context) {
			manager.runAutomaticWorkers(ctx, b.automaticWorkerScopes, func(status string) {
				b.dispatch(func() { b.workerPanel.automaticStatus.SetText(status) })
			}, providerAvailable)
		})
	}
	if b.clientConfigPath != "" {
		b.launchWithTimeout(0, b.watchManagedApprovals)
	}
	go b.runNavigationRefreshes(ctx)
	go func() {
		defer b.wg.Done()
		defer close(b.navigationInvalidations)
		backoff := 250 * time.Millisecond
		polling := false
		invalidate := func() {
			select {
			case b.navigationInvalidations <- struct{}{}:
			default:
			}
		}
		for {
			ready := false
			err := b.client.WatchNavigation(ctx, func() error {
				if !ready {
					ready = true
					backoff = 250 * time.Millisecond
					b.dispatch(func() { b.setGatewayReconnecting(false) })
				}
				invalidate()
				return nil
			})
			if ctx.Err() != nil {
				return
			}
			var response *gatewayclient.HTTPError
			if errors.As(err, &response) && response.Status == http.StatusNotFound {
				// Rust gateways offer authenticated REST refreshes without this stream.
				b.dispatch(func() { b.setGatewayReconnecting(false) })
				delay := 10 * time.Second
				if !polling {
					delay = browserRefreshCoalesceWindow
				}
				polling = true
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
					invalidate()
				}
				continue
			}
			polling = false
			if !gatewayclient.Retryable(err) {
				b.dispatch(func() { b.showNavigationError(err) })
				return
			}
			b.dispatch(func() { b.setGatewayReconnecting(true) })
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if backoff < 5*time.Second {
				backoff *= 2
				if backoff > 5*time.Second {
					backoff = 5 * time.Second
				}
			}
		}
	}()
}

const browserRefreshCoalesceWindow = time.Second

func (b *Browser) runNavigationRefreshes(ctx context.Context) {
	defer b.wg.Done()
	var lastRefresh time.Time
	var timer *time.Timer
	var timerC <-chan time.Time
	pending := false
	dispatchRefresh := func() {
		lastRefresh = time.Now()
		b.dispatch(b.refreshOnUI)
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-b.navigationInvalidations:
			if !ok {
				return
			}
			if lastRefresh.IsZero() || time.Since(lastRefresh) >= browserRefreshCoalesceWindow {
				dispatchRefresh()
				continue
			}
			pending = true
			if timer == nil {
				timer = time.NewTimer(time.Until(lastRefresh.Add(browserRefreshCoalesceWindow)))
				timerC = timer.C
			}
		case <-timerC:
			timerC = nil
			timer = nil
			if pending {
				pending = false
				dispatchRefresh()
			}
		}
	}
}

func (b *Browser) setNavigationStatus(status string) {
	b.mu.Lock()
	closed := b.closing
	b.mu.Unlock()
	if !closed {
		b.browserStatus.SetText(status)
	}
}

func (b *Browser) showNavigationError(err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	b.setNavigationStatus("Live updates stopped: " + err.Error() + ". Use Refresh to reload.")
}

func (b *Browser) Close() {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return
	}
	b.closing = true
	cancel := b.cancel
	if b.workerPanel != nil {
		b.wg.Add(1)
	}
	children := make([]*UI, 0, len(b.chats))
	for _, child := range b.chats {
		children = append(children, child)
	}
	b.mu.Unlock()
	b.dispatch(b.closeTaskWindow)
	if b.workerPanel != nil {
		b.dispatch(b.workerPanel.close)
	}
	if b.worktreeStatus != nil {
		b.dispatch(b.worktreeStatus.close)
	}
	if b.workspaceFiles != nil {
		b.dispatch(b.workspaceFiles.Close)
	}
	if b.pluginSettings != nil {
		b.dispatch(b.pluginSettings.close)
	}
	if b.workSubmissionSettings != nil {
		b.dispatch(b.workSubmissionSettings.close)
	}
	if cancel != nil {
		cancel()
	}
	if b.workerPanel != nil {
		go func() {
			defer b.wg.Done()
			_ = b.workerPanel.manager.Close()
		}()
	}
	for _, child := range children {
		child.Close()
	}
}

func (b *Browser) Wait() {
	b.wg.Wait()
	if b.workerPanel != nil {
		_ = b.workerPanel.manager.Wait()
	}
	b.mu.Lock()
	children := make([]*UI, 0, len(b.chats))
	for _, child := range b.chats {
		children = append(children, child)
	}
	b.mu.Unlock()
	for _, child := range children {
		child.Wait()
	}
}

func (b *Browser) launch(run func(context.Context)) bool {
	return b.launchWithTimeout(browserRequestTimeout, run)
}

func (b *Browser) launchWithTimeout(timeout time.Duration, run func(context.Context)) bool {
	b.mu.Lock()
	if b.closing || b.ctx == nil {
		b.mu.Unlock()
		return false
	}
	ctx := b.ctx
	b.wg.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		run(ctx)
	}()
	return true
}

func (b *Browser) Refresh() {
	b.dispatch(func() {
		b.pluginSettings.reload(true)
		if b.clientConfigPath != "" {
			b.scanProjectCheckouts()
		}
		b.refreshOnUI()
	})
}

func (b *Browser) refreshOnUI() {
	b.mu.Lock()
	if b.closing || b.ctx == nil || b.projectLoading {
		b.mu.Unlock()
		return
	}
	b.projectLoading = true
	b.mu.Unlock()
	bindings := b.machineCheckoutBindings()
	b.launch(func(ctx context.Context) {
		hostname, hostErr := os.Hostname()
		var checkouts []storage.CheckoutOverview
		var checkoutErr error
		if hostErr == nil {
			var report []storage.CheckoutReportItem
			report, checkoutErr = machineCheckoutReport(ctx, bindings)
			if checkoutErr == nil {
				checkouts, checkoutErr = b.client.SyncCheckouts(ctx, hostname, report)
			}
		} else {
			checkoutErr = hostErr
		}
		items, err := b.client.Projects(ctx)
		summary, presenceErr := b.client.PresenceSummary(ctx, hostname)
		b.dispatch(func() {
			b.applyHeaderPresence(summary, presenceErr)
			if checkoutErr == nil {
				b.checkoutItems = checkouts
				if b.mainView == "Alerts" {
					b.showCheckoutAlerts(b.selectedProject, checkouts)
				}
			} else {
				b.checkoutItems = nil
			}
			b.applyProjects(items, err)
			if checkoutErr != nil && b.workerPanel != nil && b.selection.CheckoutPath != "" && !localCheckoutAvailable(b.selection.CheckoutPath) && err == nil {
				b.workerPanel.discoveryStatus.SetText("Checkout is missing; machine-reported recovery sources are unavailable.")
			}
		})
	})
	b.mu.Lock()
	hasProject := b.selectedProject != ""
	b.mu.Unlock()
	if hasProject {
		b.refreshTasks()
		if b.mainView == "Conversations" {
			b.loadConversationAgents(b.selectedProject, b.projectConversationGeneration)
		}
	}
}

type workspaceCheckoutBinding struct {
	project, directory string
}

func (b *Browser) machineCheckoutBindings() []workspaceCheckoutBinding {
	bindings := make([]workspaceCheckoutBinding, 0, len(b.workspaceIDs))
	for _, id := range b.workspaceIDs {
		selection := b.prefs.Selections[workspaceSelectionKey(b.prefKey, id)]
		if id == b.activeWorkspaceID {
			selection = b.selection
		}
		if selection.Project == "" || selection.CheckoutPath == "" {
			continue
		}
		bindings = append(bindings, workspaceCheckoutBinding{project: selection.Project, directory: selection.CheckoutPath})
	}
	return bindings
}

func machineCheckoutReport(ctx context.Context, bindings []workspaceCheckoutBinding) ([]storage.CheckoutReportItem, error) {
	report := make([]storage.CheckoutReportItem, 0, len(bindings))
	for _, binding := range bindings {
		if !localCheckoutAvailable(binding.directory) {
			continue
		}
		origin, err := projectidentity.CloneOrigin(ctx, binding.directory)
		if err != nil {
			return nil, err
		}
		report = append(report, storage.CheckoutReportItem{Project: binding.project, Origin: origin})
	}
	return report, nil
}

func (b *Browser) applyProjects(items []storage.ProjectOverview, err error) {
	b.mu.Lock()
	b.projectLoading = false
	closed := b.closing
	b.mu.Unlock()
	if closed {
		return
	}
	if err != nil {
		b.browserStatus.SetText("Projects unavailable: " + err.Error())
		return
	}
	b.projectItems = items
	b.updateAutomaticWorkerScopes()
	if b.reconcileWorkspaceProjectFavorites() {
		b.persist()
	}
	if b.workerPanel != nil {
		b.workerPanel.refreshRecoverySources()
	}
	b.applyProjectFilter()
	b.updateSelectedProjectStatus()
	if b.localRailSelection != "" {
		return
	}
	if len(items) == 0 {
		b.selectProject("")
		b.browserStatus.SetText("No active projects are available.")
		return
	}
	if b.selectedProject != "" && !projectExists(items, b.selectedProject) {
		b.selectProject("")
	}
	if b.selectedProject == "" {
		choice := b.selection.Project
		if choice == "" {
			choice = items[0].Ident
		}
		if projectExists(items, choice) {
			b.selectProject(choice)
		} else {
			b.browserStatus.SetText("The saved project is no longer available. Select a project.")
		}
	}
	if b.selectedProject != "" {
		b.refreshTasks()
		b.refreshReviewNotices(b.selectedProject)
		if b.mainView == "Board" {
			b.taskBoard.refresh()
		}
	}
}

func (b *Browser) selectProject(project string) {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return
	}
	b.localRailSelection = ""
	if b.selection.Project != project {
		b.selection.ProjectConversation = ""
	} else if b.selection.MainView == "Conversations" && b.selection.ProjectConversation == "" {
		b.selection.ProjectConversation = b.selection.Conversation
	}
	preserveSaved := b.selectedProject == "" && project != "" && b.selection.Project == project
	checkoutBindingChanged := b.selection.Project != "" && b.selection.Project != project && b.selection.CheckoutPath != ""
	if b.selection.Project != "" && b.selection.Project != project {
		b.selection.CheckoutPath = ""
		b.selection.CheckoutOrigin = nil
		if b.workerPanel != nil {
			b.workerPanel.restoreWorkspaceCheckout()
		}
	}
	desiredTask, desiredConversation := b.selection.Task, b.selection.Conversation
	b.projectGen++
	b.taskGen++
	b.selectedProject = project
	b.selectedTask = ""
	b.selectedConversation = ""
	b.selectedTaskDetail = nil
	b.conversationItems = nil
	b.conversationAfter = ""
	b.conversationsMore = false
	b.conversationsLoading = false
	b.taskItems = nil
	b.visibleTasks = nil
	b.detailLoading = false
	b.mu.Unlock()
	if b.activeWorkspaceID != "" && b.selection.Project != project {
		b.workspaceBindingGeneration[b.activeWorkspaceID]++
		if state := b.taskLists[b.activeWorkspaceID]; state != nil {
			state.bindingGeneration = b.workspaceBindingGeneration[b.activeWorkspaceID]
			state.queryGeneration++
			state.project = project
			state.status = b.selection.Status
			state.items = nil
			state.loading = false
			state.refreshPending = false
		}
	}
	b.taskComposer.selectProject(project)
	b.taskWorkflow.selectTask(project, "")
	b.taskEditor.selectTask(project, "")
	b.selection.Project = project
	if b.selection.CheckoutPath == "" {
		b.restoreProjectCheckout(project)
	}
	if b.reconcileWorkspaceProjectFavorites() {
		b.persist()
		b.applyProjectFilter()
	}
	if b.workerPanel != nil {
		b.workerPanel.refreshRecoverySources()
	}
	if preserveSaved {
		b.selection.Task = desiredTask
		b.selection.Conversation = desiredConversation
	} else {
		b.selection.Task = ""
		b.selection.Conversation = ""
	}
	b.updateSelectedProjectStatus()
	b.projectLabel.SetText(project)
	if project == "" {
		b.headerProject.SetText("Gateway project: —")
	} else {
		b.headerProject.SetText("Gateway project: " + project)
	}
	b.standaloneChatHost.Objects = []fyne.CanvasObject{widget.NewLabel("Select a conversation or start a new one.")}
	b.standaloneChatHost.Refresh()
	b.newConversationStatus.SetText("")
	b.conversationSelection++
	b.activeConversationProvider = ""
	b.pendingConversationProvider = ""
	b.refreshConversationLaunchControls()
	b.refreshMainView()
	b.selectedProjectFavorite.SetText(b.favoriteText(project))
	b.syncProjectSelection()
	b.taskTitle.SetText("Select a task")
	b.taskMeta.Hide()
	b.taskSpec.SetText("Choose a task to view its specification and comments.")
	b.taskComments.RemoveAll()
	b.showTaskHistory(false)
	b.tasks.UnselectAll()
	b.conversations.UnselectAll()
	b.loadMoreConversations.Enable()
	b.tasks.Refresh()
	b.conversations.Refresh()
	b.syncConversationSelection()
	b.loadMoreConversations.Hide()
	b.showEmptyChat("Select an existing conversation to open its live chat.")
	b.persist()
	b.syncWorkspaceFiles()
	b.refreshWorktreeStatus()
	b.refreshWorkspaceSelector()
	if checkoutBindingChanged && b.client != nil && b.pluginSettings != nil {
		b.Refresh()
	}
	if project != "" {
		if state := b.currentTaskListState(project); state != nil && state.items != nil {
			b.taskItems = append([]storage.TaskSummary(nil), state.items...)
			b.applyTaskFilter()
		}
		b.refreshTasks()
	}
}

func (b *Browser) hasWorkspace(id string) bool {
	for _, candidate := range b.workspaceIDs {
		if candidate == id {
			return true
		}
	}
	return false
}

func (b *Browser) refreshWorkspaceSelector() {
	if b.workspaceSelect == nil {
		return
	}
	options := make([]string, 0, len(b.workspaceIDs))
	b.workspaceOptionIDs = make(map[string]string, len(b.workspaceIDs))
	selected := ""
	selectedPeerID := ""
	if b.workspacePeerSelect != nil {
		selectedPeerID = b.workspacePeerOptionIDs[b.workspacePeerSelect.Selected]
	}
	for i, id := range b.workspaceIDs {
		selection := b.prefs.Selections[workspaceSelectionKey(b.prefKey, id)]
		if id == b.activeWorkspaceID {
			selection = b.selection
		}
		labelName := strings.TrimSpace(selection.WorkspaceName)
		if labelName == "" {
			labelName = selection.Project
			if labelName == "" {
				labelName = "Choose project"
			}
		}
		label := fmt.Sprintf("Workspace %d · %s", i+1, labelName)
		options = append(options, label)
		b.workspaceOptionIDs[label] = id
		if id == b.activeWorkspaceID {
			selected = label
		}
	}
	b.updatingWorkspaceSelect = true
	b.workspaceSelect.Options = options
	if selected != "" {
		b.workspaceSelect.SetSelected(selected)
	}
	b.updatingWorkspaceSelect = false
	if b.workspaceClose != nil {
		b.workspaceClose.Enable()
	}
	if b.workspacePeerSelect != nil {
		peerOptions := make([]string, 0, len(b.workspaceIDs))
		b.workspacePeerOptionIDs = make(map[string]string)
		selectedPeer := ""
		for i, id := range b.workspaceIDs {
			if id == b.activeWorkspaceID {
				continue
			}
			selection := b.prefs.Selections[workspaceSelectionKey(b.prefKey, id)]
			labelName := strings.TrimSpace(selection.WorkspaceName)
			if labelName == "" {
				labelName = selection.Project
				if labelName == "" {
					labelName = "Choose project"
				}
			}
			label := fmt.Sprintf("Workspace %d · %s", i+1, labelName)
			peerOptions = append(peerOptions, label)
			b.workspacePeerOptionIDs[label] = id
			if id == selectedPeerID {
				selectedPeer = label
			}
		}
		b.updatingPeerWorkspaceSelect = true
		b.workspacePeerSelect.Options = peerOptions
		b.workspacePeerSelect.SetSelected(selectedPeer)
		b.updatingPeerWorkspaceSelect = false
		if b.workspacePeerClose != nil {
			if selectedPeer != "" {
				b.workspacePeerClose.Enable()
			} else {
				b.workspacePeerClose.Disable()
			}
		}
	}
}

func (b *Browser) selectWorkspace(id string) {
	if !b.hasWorkspace(id) || id == b.activeWorkspaceID {
		return
	}
	b.persist()
	b.activeWorkspaceID = id
	b.selection = b.prefs.Selections[workspaceSelectionKey(b.prefKey, id)]
	b.refreshConversationLaunchControls()
	if b.workerPanel != nil {
		b.workerPanel.restoreWorkspaceCheckout()
	}
	b.selection.Status = normalizeBrowserStatus(b.selection.Status)
	project := b.selection.Project
	b.updatingNavigationWidgets = true
	b.projectSearch.SetText(b.selection.ProjectFilter)
	b.taskSearch.SetText(b.selection.TaskFilter)
	b.workspaceName.SetText(b.selection.WorkspaceName)
	b.statusSelect.SetSelected(browserStatusLabel(b.selection.Status))
	b.updatingNavigationWidgets = false
	b.refreshStatusShortcuts()
	b.applyProjectFilter()
	b.applyTaskFilter()
	b.mu.Lock()
	b.selectedProject = ""
	b.mu.Unlock()
	b.refreshWorkspaceSelector()
	b.selectProject(project)
	if b.selection.MainView != "" {
		b.setMainView(b.selection.MainView)
	}
}

func (b *Browser) addWorkspace() {
	b.persist()
	id := uuid.NewString()
	b.workspaceIDs = append(b.workspaceIDs, id)
	if b.prefs.WorkspaceIDs == nil {
		b.prefs.WorkspaceIDs = make(map[string][]string)
	}
	b.prefs.WorkspaceIDs[b.prefKey] = append([]string(nil), b.workspaceIDs...)
	if b.workspaceBindingGeneration == nil {
		b.workspaceBindingGeneration = make(map[string]uint64)
	}
	b.workspaceBindingGeneration[id] = 1
	if b.prefs.Selections == nil {
		b.prefs.Selections = make(map[string]NavigationSelection)
	}
	b.prefs.Selections[workspaceSelectionKey(b.prefKey, id)] = NavigationSelection{Status: browserStatusActive}
	b.selectWorkspace(id)
}

func (b *Browser) bindWorkspaceCheckout(workspaceID string, bindingRevision uint64, project, directory string) bool {
	return b.bindWorkspaceCheckoutWithOrigin(workspaceID, bindingRevision, project, directory, nil)
}

func (b *Browser) bindWorkspaceCheckoutWithOrigin(workspaceID string, bindingRevision uint64, project, directory string, origin *string) bool {
	if workspaceID == "" || !b.hasWorkspace(workspaceID) || b.activeWorkspaceID != workspaceID ||
		b.workspaceBindingGeneration[workspaceID] != bindingRevision || b.selection.Project != project {
		return false
	}
	b.selection.CheckoutPath = directory
	b.updateSelectedProjectStatus()
	if origin == nil {
		b.selection.CheckoutOrigin = nil
	} else {
		value := *origin
		b.selection.CheckoutOrigin = &value
	}
	if b.prefs.ProjectCheckouts == nil {
		b.prefs.ProjectCheckouts = make(map[string]localProjectBinding)
	}
	b.prefs.ProjectCheckouts[FavoriteKey(b.prefKey, project)] = localProjectBinding{directory, b.selection.CheckoutOrigin}
	b.persist()
	b.syncWorkspaceFiles()
	b.refreshWorktreeStatus()
	if b.reconcileWorkspaceProjectFavorites() {
		b.persist()
		b.applyProjectFilter()
	}
	if b.selectedProjectFavorite != nil {
		b.selectedProjectFavorite.SetText(b.favoriteText(b.selectedProject))
	}
	return true
}

func (b *Browser) closeActiveWorkspace() {
	b.closeWorkspace(b.activeWorkspaceID)
}

func (b *Browser) closeSelectedPeerWorkspace() {
	id := b.workspacePeerOptionIDs[b.workspacePeerSelect.Selected]
	if id == "" || id == b.activeWorkspaceID {
		return
	}
	b.closeWorkspace(id)
}

func (b *Browser) closeWorkspace(id string) {
	if id == "" || !b.hasWorkspace(id) {
		return
	}
	active := id == b.activeWorkspaceID
	if b.taskComposer.hasUnresolvedReceipt(id) {
		b.browserStatus.SetText("This workspace has a pending or unconfirmed task receipt. Inspect it before closing.")
		return
	}
	b.persist()
	closedIndex := 0
	for i, candidate := range b.workspaceIDs {
		if candidate == id {
			closedIndex = i
			break
		}
	}
	remaining := make([]string, 0, len(b.workspaceIDs)-1)
	for _, candidate := range b.workspaceIDs {
		if candidate != id {
			remaining = append(remaining, candidate)
		}
	}
	delete(b.prefs.Selections, workspaceSelectionKey(b.prefKey, id))
	delete(b.workspaceBindingGeneration, id)
	delete(b.taskLists, id)
	b.taskComposer.removeWorkspaceDrafts(id)
	if len(remaining) == 0 {
		remaining = []string{uuid.NewString()}
		b.workspaceBindingGeneration[remaining[0]] = 1
		b.taskLists[remaining[0]] = &browserTaskListState{bindingGeneration: 1, status: browserStatusActive}
		b.prefs.Selections[workspaceSelectionKey(b.prefKey, remaining[0])] = NavigationSelection{Status: browserStatusActive}
		closedIndex = 0
	}
	b.workspaceIDs = remaining
	if closedIndex >= len(remaining) {
		closedIndex = len(remaining) - 1
	}
	if b.prefs.WorkspaceIDs == nil {
		b.prefs.WorkspaceIDs = make(map[string][]string)
	}
	b.prefs.WorkspaceIDs[b.prefKey] = append([]string(nil), remaining...)
	if !active {
		b.refreshWorkspaceSelector()
		b.persist()
		b.browserStatus.SetText("Peer workspace closed.")
		return
	}
	id = remaining[closedIndex]
	b.activeWorkspaceID = ""
	b.refreshWorkspaceSelector()
	b.selectWorkspace(id)
	b.browserStatus.SetText("Workspace closed.")
	if b.client != nil && b.pluginSettings != nil {
		b.Refresh()
	}
}

func (b *Browser) refreshTasks() {
	b.refreshWorkspaceTasks(b.activeWorkspaceID, b.selectedProject)
}

func (b *Browser) currentTaskListState(project string) *browserTaskListState {
	state := b.taskLists[b.activeWorkspaceID]
	allStates := taskSearchIncludesAllStates(b.selection.Status, b.selection.TaskFilter)
	if state == nil || state.project != project || state.bindingGeneration != b.workspaceBindingGeneration[b.activeWorkspaceID] || state.status != b.selection.Status || state.allStates != allStates {
		return nil
	}
	return state
}

func (b *Browser) refreshWorkspaceTasks(workspaceID, project string) {
	if workspaceID == "" || project == "" || !b.hasWorkspace(workspaceID) {
		return
	}
	state := b.taskLists[workspaceID]
	if state == nil {
		state = &browserTaskListState{bindingGeneration: b.workspaceBindingGeneration[workspaceID], status: browserStatusActive}
		b.taskLists[workspaceID] = state
	}
	selection := b.prefs.Selections[workspaceSelectionKey(b.prefKey, workspaceID)]
	if workspaceID == b.activeWorkspaceID {
		selection = b.selection
	}
	status := normalizeBrowserStatus(selection.Status)
	allStates := taskSearchIncludesAllStates(status, selection.TaskFilter)
	if state.project != project || state.bindingGeneration != b.workspaceBindingGeneration[workspaceID] || state.status != status || state.allStates != allStates {
		state.queryGeneration++
		state.project = project
		state.bindingGeneration = b.workspaceBindingGeneration[workspaceID]
		state.status = status
		state.allStates = allStates
		state.items = nil
		state.loading = false
		state.refreshPending = false
	}
	if state.loading {
		state.refreshPending = true
		return
	}
	state.loading = true
	state.refreshPending = false
	queryGeneration, bindingGeneration := state.queryGeneration, state.bindingGeneration
	statuses := browserTaskStatuses(status)
	if allStates {
		statuses = browserTaskStatuses(browserStatusAll)
	}
	started := b.launch(func(ctx context.Context) {
		items, err := b.client.Tasks(ctx, project, statuses)
		if err == nil {
			items = loadTaskParents(ctx, project, items, b.client.TaskDetail)
		}
		b.dispatch(func() {
			b.applyWorkspaceTasks(workspaceID, project, bindingGeneration, queryGeneration, status, allStates, items, err)
		})
	})
	if !started {
		state.loading = false
	}
}

func (b *Browser) applyWorkspaceTasks(workspaceID, project string, bindingGeneration, queryGeneration uint64, status string, allStates bool, items []storage.TaskSummary, err error) {
	if b.closing || !b.hasWorkspace(workspaceID) {
		return
	}
	state := b.taskLists[workspaceID]
	if state == nil || state.project != project || state.bindingGeneration != bindingGeneration || state.queryGeneration != queryGeneration || state.status != status || state.allStates != allStates || b.workspaceBindingGeneration[workspaceID] != bindingGeneration {
		return
	}
	state.loading = false
	pendingRefresh := state.refreshPending
	state.refreshPending = false
	if err == nil {
		state.items = append([]storage.TaskSummary(nil), items...)
	}
	if pendingRefresh {
		b.refreshWorkspaceTasks(workspaceID, project)
		return
	}
	if workspaceID != b.activeWorkspaceID || project != b.selectedProject || status != b.selection.Status || allStates != taskSearchIncludesAllStates(b.selection.Status, b.selection.TaskFilter) {
		return
	}
	if err != nil {
		b.browserStatus.SetText("Tasks unavailable: " + err.Error())
		return
	}
	b.applyTaskItems(items)
}

func (b *Browser) applyTaskItems(items []storage.TaskSummary) {
	b.taskItems = items
	b.applyTaskFilter()
	if len(items) == 0 {
		b.selectTask("")
		b.browserStatus.SetText("No tasks match this status.")
		return
	}
	if b.selectedTask != "" && !taskExists(items, b.selectedTask) {
		b.selectTask("")
	}
	if b.selectedTask == "" {
		if b.selection.Task != "" && taskExists(items, b.selection.Task) {
			b.selectTask(b.selection.Task)
		} else if b.selection.Task != "" {
			b.selection.Task = ""
			b.selection.Conversation = ""
			b.persist()
			b.taskTitle.SetText("Saved task is no longer available")
			b.taskMeta.Hide()
			b.taskSpec.SetText("Select a task to view its details and conversations.")
			b.browserStatus.SetText("The previously selected task is no longer available.")
		} else {
			b.browserStatus.SetText("Select a task to view its details and conversations.")
		}
	} else {
		b.refreshTaskDetail()
	}
}

func (b *Browser) showTaskHistory(selected bool) {
	if b.detailHost == nil {
		return
	}
	if selected {
		b.emptyDetailPrompt.Hide()
		b.taskHistory.Show()
		b.taskHeaderActions.Show()
	} else {
		b.taskHistory.Hide()
		b.taskHeaderActions.Hide()
		b.emptyDetailPrompt.Show()
	}
	b.detailHost.Refresh()
}

func (b *Browser) selectTask(task string) {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return
	}
	preserveSaved := task != "" && b.selection.Task == task
	desiredConversation := b.selection.Conversation
	b.taskGen++
	project := b.selectedProject
	b.selectedTask = task
	b.selectedConversation = ""
	b.selectedTaskDetail = nil
	b.conversationItems = nil
	b.conversationAfter = ""
	b.conversationsMore = false
	b.conversationsLoading = false
	b.detailLoading = false
	b.showTaskHistory(task != "")
	b.mu.Unlock()
	b.taskComposer.selectTask(project, task)
	b.taskWorkflow.selectTask(project, task)
	b.taskAssignment.selectTask(project, task)
	b.taskEditor.selectTask(project, task)
	b.selection.Task = task
	if preserveSaved {
		b.selection.Conversation = desiredConversation
	} else {
		b.selection.Conversation = ""
	}
	b.conversations.Refresh()
	b.syncConversationSelection()
	b.conversations.UnselectAll()
	b.loadMoreConversations.Enable()
	b.loadMoreConversations.Hide()
	b.taskTitle.SetText("Loading task…")
	b.taskMeta.Hide()
	b.taskSpec.SetText("")
	b.taskComments.RemoveAll()
	b.taskParent.RemoveAll()
	b.taskSubtasks.RemoveAll()
	b.taskDetailScroll.ScrollToTop()
	b.showEmptyChat("Select an existing conversation to open its live chat.")
	b.persist()
	if task == "" || project == "" {
		b.taskTitle.SetText("Select a task")
		b.taskMeta.Hide()
		b.taskSpec.SetText("No task is selected.")
		return
	}
	b.refreshTaskDetail()
}

func (b *Browser) refreshTaskDetail() {
	b.mu.Lock()
	project, task, generation := b.selectedProject, b.selectedTask, b.taskGen
	conversationWindow := len(b.conversationItems)
	if conversationWindow < browserConversationPage {
		conversationWindow = browserConversationPage
	}
	if b.closing || b.ctx == nil || project == "" || task == "" || b.detailLoading {
		b.mu.Unlock()
		return
	}
	b.detailLoading = true
	b.mu.Unlock()
	b.launch(func(ctx context.Context) {
		detail, detailErr := b.client.TaskDetail(ctx, project, task)
		conversations, after, more, convErr := loadConversationWindow(ctx, b.client, project, task, conversationWindow)
		if detailErr == nil && convErr != nil {
			var response *gatewayclient.HTTPError
			if errors.As(convErr, &response) && response.Status == 404 && b.localConversationsDirectory != "" {
				conversations, detailErr = listLocalTaskThreads(b.localConversationsDirectory, project, task)
			} else {
				detailErr = convErr
			}
		}
		var children []storage.Task
		var subtaskErr error
		if detailErr == nil {
			children, subtaskErr = b.client.TaskSubtasks(ctx, project, task)
		}
		b.dispatch(func() {
			b.applyTaskDetail(project, task, generation, detail, conversations, after, more, detailErr)
			if !b.closing && generation == b.taskGen && project == b.selectedProject && task == b.selectedTask {
				b.showTaskSubtasks(children, subtaskErr)
			}
		})
	})
}

func (b *Browser) refreshTaskAfterMutation(project, task string) {
	b.mu.Lock()
	if b.closing || b.selectedProject != project || b.selectedTask != task {
		b.mu.Unlock()
		return
	}
	b.taskGen++
	b.detailLoading = false
	b.conversationsLoading = false
	b.mu.Unlock()
	b.refreshTaskDetail()
}

func (b *Browser) refreshTasksAfterMutation(project string) {
	b.mu.Lock()
	if b.closing || b.selectedProject != project {
		b.mu.Unlock()
		return
	}
	// Fence any list snapshot that began before the mutation acknowledgment.
	b.projectGen++
	if state := b.currentTaskListState(project); state != nil {
		state.queryGeneration++
		state.loading = false
		state.refreshPending = false
	}
	b.mu.Unlock()
	b.refreshTasks()
}

func loadConversationWindow(ctx context.Context, client *gatewayclient.Client, project, task string, target int) ([]storage.Conversation, string, bool, error) {
	if target < browserConversationPage {
		target = browserConversationPage
	}
	items := make([]storage.Conversation, 0, target)
	after := ""
	more := false
	for len(items) < target {
		limit := min(browserConversationPage, target-len(items))
		page, err := client.TaskConversations(ctx, project, task, after, limit)
		if err != nil {
			return nil, "", false, err
		}
		items = append(items, page...)
		if len(page) > 0 {
			after = page[len(page)-1].ID
		}
		if len(page) < limit {
			more = false
			break
		}
		more = true
		if len(page) == 0 {
			break
		}
	}
	return items, after, more, nil
}

func (b *Browser) applyTaskDetail(project, task string, generation uint64, detail *storage.TaskDetail, conversations []storage.Conversation, after string, more bool, err error) {
	b.mu.Lock()
	valid := !b.closing && generation == b.taskGen && project == b.selectedProject && task == b.selectedTask
	if valid {
		b.detailLoading = false
	}
	b.mu.Unlock()
	if !valid {
		return
	}
	if err != nil {
		b.taskTitle.SetText("Task details unavailable")
		b.taskSpec.SetText(err.Error())
		b.browserStatus.SetText("Task detail request failed.")
		return
	}
	if detail == nil || detail.Task.ID != task {
		b.taskTitle.SetText("Task is unavailable")
		b.taskSpec.SetText("The selected task no longer exists.")
		return
	}
	b.selectedTaskDetail = detail
	b.taskEditor.setAuthoritative(project, task, detail.Task)
	b.taskWorkflow.setTask(project, task, &detail.Task)
	b.taskAssignment.setTask(project, task, detail.Task)
	b.conversationItems = conversations
	if len(b.conversationItems) > 0 {
		b.conversationAfter = b.conversationItems[len(b.conversationItems)-1].ID
	} else {
		b.conversationAfter = after
	}
	b.conversationsMore = more || len(conversations) == browserConversationPage
	b.conversationsLoading = false
	if len(conversations) > 0 || b.conversationsMore {
		b.taskDetailSplit.Trailing.Show()
	} else {
		b.taskDetailSplit.Trailing.Hide()
	}
	b.taskDetailSplit.Refresh()
	meta := browserStatusLabel(detail.Task.Status)
	if detail.Task.TaskType != "" {
		meta += " · " + detail.Task.TaskType
	}
	b.showTaskParent(project, task, detail.Task.Parent)
	b.taskTitle.SetText(detail.Task.Title)
	b.taskMeta.SetText(meta)
	b.taskMeta.Show()
	b.browserStatus.SetText("Ready")
	spec := "No specification was provided."
	if detail.Task.Details != nil && strings.TrimSpace(*detail.Task.Details) != "" {
		spec = strings.TrimSpace(*detail.Task.Details)
	}
	b.taskSpec.SetText(spec)
	b.showTaskComments(detail.Comments)
	b.taskDetailScroll.Refresh()
	b.conversations.Refresh()
	b.syncConversationSelection()
	if b.conversationsMore {
		b.loadMoreConversations.Show()
	} else {
		b.loadMoreConversations.Hide()
	}
	if len(b.conversationItems) == 0 && !b.conversationsMore {
		b.browserStatus.SetText("This task has no existing conversations.")
		b.selectedConversation = ""
		b.selection.Conversation = ""
		b.showEmptyChat("No conversations exist for this task yet.")
		b.persist()
		return
	}
	if b.selection.Conversation != "" {
		for _, c := range b.conversationItems {
			if c.ID == b.selection.Conversation && c.TaskID == task && c.ProjectIdent == project {
				b.openConversation(c)
				return
			}
		}
	}
	if b.conversationsMore {
		if !b.showCachedChat(project, b.selection.Conversation) {
			b.selectedConversation = ""
			b.showEmptyChat("Load more conversations to locate the previously selected session.")
		}
	} else {
		b.selectedConversation = ""
		b.selection.Conversation = ""
		b.showEmptyChat("Select an existing conversation to open its live chat.")
		b.persist()
	}
}

func mergeConversations(first, previous []storage.Conversation) []storage.Conversation {
	seen := make(map[string]bool, len(first)+len(previous))
	merged := make([]storage.Conversation, 0, len(first)+len(previous))
	for _, item := range first {
		if !seen[item.ID] {
			seen[item.ID] = true
			merged = append(merged, item)
		}
	}
	for _, item := range previous {
		if !seen[item.ID] {
			seen[item.ID] = true
			merged = append(merged, item)
		}
	}
	return merged
}

func (b *Browser) loadMore() {
	b.mu.Lock()
	project, task, after, generation := b.selectedProject, b.selectedTask, b.conversationAfter, b.taskGen
	if b.closing || b.ctx == nil || project == "" || task == "" || !b.conversationsMore || b.conversationsLoading {
		b.mu.Unlock()
		return
	}
	b.conversationsLoading = true
	b.mu.Unlock()
	b.loadMoreConversations.Disable()
	b.launch(func(ctx context.Context) {
		items, err := b.client.TaskConversations(ctx, project, task, after, browserConversationPage)
		b.dispatch(func() { b.applyMoreConversations(project, task, generation, items, err) })
	})
}

func (b *Browser) applyMoreConversations(project, task string, generation uint64, items []storage.Conversation, err error) {
	b.mu.Lock()
	valid := !b.closing && generation == b.taskGen && project == b.selectedProject && task == b.selectedTask
	if valid {
		b.conversationsLoading = false
	}
	b.mu.Unlock()
	if !valid {
		return
	}
	b.loadMoreConversations.Enable()
	if err != nil {
		b.browserStatus.SetText("More conversations unavailable: " + err.Error())
		return
	}
	b.conversationItems = mergeConversations(b.conversationItems, items)
	if len(items) > 0 {
		b.conversationAfter = items[len(items)-1].ID
	}
	b.conversationsMore = len(items) == browserConversationPage
	if b.conversationsMore {
		b.loadMoreConversations.Show()
	} else {
		b.loadMoreConversations.Hide()
	}
	b.conversations.Refresh()
	b.syncConversationSelection()
	if b.selection.Conversation != "" {
		for _, item := range items {
			if item.ID == b.selection.Conversation && item.ProjectIdent == project && item.TaskID == task {
				b.openConversation(item)
				break
			}
		}
		found := false
		for _, item := range b.conversationItems {
			if item.ID == b.selection.Conversation && item.ProjectIdent == project && item.TaskID == task {
				found = true
				break
			}
		}
		if !found && !b.conversationsMore {
			b.selectedConversation = ""
			b.selection.Conversation = ""
			b.persist()
			b.showEmptyChat("The previously selected conversation is no longer available.")
		}
	}
}

func (b *Browser) openConversation(c storage.Conversation) {
	if c.ProjectIdent != b.selectedProject || c.TaskID != b.selectedTask {
		return
	}
	b.selectedConversation = c.ID
	b.selection.Conversation = c.ID
	b.persist()
	child := b.chatForConversation(c)
	if child != nil {
		b.chatHost.Objects = []fyne.CanvasObject{child.Content()}
		b.chatHost.Refresh()
		b.showTaskChat()
		b.syncConversationSelection()
	}
}

func (b *Browser) chatForConversation(c storage.Conversation) *UI {
	key := chatKey(c.ProjectIdent, c.ID)
	b.mu.Lock()
	child := b.chats[key]
	b.mu.Unlock()
	if child == nil {
		title := c.Provider + " · " + time.UnixMilli(c.CreatedAt).Format("Jan 2, 2006") + " · " + shortID(c.ID)
		if b.selectedTaskDetail != nil && b.selectedTaskDetail.Task.ID == c.TaskID && b.selectedTaskDetail.Task.Title != "" {
			title = b.selectedTaskDetail.Task.Title + " · " + title
		}
		var transport conversationClient = b.client
		readOnly := b.localConversationsDirectory != "" && c.TaskID != "" && c.ID == legacyTaskConversationID(c.TaskID, c.Owner.SessionID)
		if readOnly {
			dir, err := taskTranscriptDirectory(b.localConversationsDirectory, c.ProjectIdent, c.TaskID)
			if err != nil || !canonicalTaskUUID(c.Owner.SessionID) {
				b.browserStatus.SetText("Task transcript has an invalid task or agent identity.")
				return nil
			}
			transport = localTaskThread{info: c, path: filepath.Join(dir, c.Owner.SessionID+".md")}
		}
		fleet := c.FleetHostname != nil
		if !fleet && b.clientConfigPath != "" && c.TaskID == "" {
			transport = localconversation.Remote{ConfigPath: b.clientConfigPath}
		} else if !fleet && b.localConversationsDirectory != "" && c.TaskID == "" {
			local, err := openMarkdownChat(b.localConversationsDirectory, c, b.watchApprovals(worker.OpenWithToolApprovals(worker.OpenNative, b.workPreferencesPath), &c))
			if err != nil {
				b.showLocalChatOpenError("Conversation could not be opened: " + err.Error())
				return nil
			}
			b.localChatSessions[key] = local
			transport = local
		}
		// Local chats and this host's workers share the approval preferences.
		approvalsPath := ""
		localChat := !fleet && c.TaskID == "" && (b.clientConfigPath != "" || b.localConversationsDirectory != "")
		host, hostErr := os.Hostname()
		localWorker := fleet && hostErr == nil && strings.EqualFold(strings.TrimSpace(host), strings.TrimSpace(*c.FleetHostname))
		if !readOnly && (localChat || localWorker) {
			approvalsPath = b.workPreferencesPath
		}
		approvalDirectory := c.WorkingDirectory
		if approvalDirectory == "" {
			for _, project := range b.projectItems {
				if project.Ident == c.ProjectIdent {
					approvalDirectory = b.projectCheckout(project).Directory
					break
				}
			}
		}
		var err error
		child, err = New(Config{Client: transport, Project: c.ProjectIdent, ConversationID: c.ID, TaskTitle: title, Dispatch: b.dispatch, ReadOnly: readOnly, ToolApprovalsPath: approvalsPath, ToolApprovalDirectory: approvalDirectory})
		if err != nil {
			b.browserStatus.SetText("Conversation could not be opened: " + err.Error())
			return nil
		}
		b.mu.Lock()
		if b.closing {
			b.mu.Unlock()
			child.Close()
			return nil
		}
		if existing := b.chats[key]; existing != nil {
			child.Close()
			child = existing
		} else {
			b.chats[key] = child
			b.chatStarted[key] = false
		}
		b.mu.Unlock()
	}
	b.mu.Lock()
	if !b.chatStarted[key] {
		b.chatStarted[key] = true
		ctx := b.ctx
		closed := b.closing
		b.mu.Unlock()
		if !closed && ctx != nil {
			child.Start(ctx)
		}
	} else {
		b.mu.Unlock()
	}
	return child
}

func (b *Browser) showCachedChat(project, conversation string) bool {
	if conversation == "" {
		return false
	}
	b.mu.Lock()
	child := b.chats[chatKey(project, conversation)]
	b.mu.Unlock()
	if child == nil {
		return false
	}
	b.selectedConversation = conversation
	b.chatHost.Objects = []fyne.CanvasObject{child.Content()}
	b.chatHost.Refresh()
	b.showTaskChat()
	return true
}

func (b *Browser) refreshStatusShortcuts() {
	for status, button := range b.statusShortcuts {
		button.Importance = widget.MediumImportance
		if status == b.selection.Status {
			button.Importance = widget.HighImportance
		}
		button.Refresh()
	}
}

func (b *Browser) changeStatus(status string) {
	status = browserStatusValue(status)
	if b.updatingNavigationWidgets || !validBrowserStatus(status) {
		return
	}
	b.selection.Status = status
	b.refreshStatusShortcuts()
	if state := b.taskLists[b.activeWorkspaceID]; state != nil {
		state.queryGeneration++
		state.status = status
		state.items = nil
		state.loading = false
		state.refreshPending = false
	}
	b.mu.Lock()
	b.projectGen++
	b.taskGen++
	b.selectedTask = ""
	b.selectedConversation = ""
	b.selectedTaskDetail = nil
	b.showTaskHistory(false)
	b.conversationItems = nil
	b.taskItems = nil
	b.visibleTasks = nil
	b.detailLoading = false
	b.conversationsLoading = false
	b.mu.Unlock()
	b.taskEditor.selectTask(b.selectedProject, "")
	b.selection.Task = ""
	b.selection.Conversation = ""
	b.persist()
	b.taskTitle.SetText("Select a task")
	b.taskMeta.Hide()
	b.taskSpec.SetText("Choose a task to view its specification and comments.")
	b.taskComments.RemoveAll()
	b.conversations.Refresh()
	b.conversations.UnselectAll()
	b.loadMoreConversations.Enable()
	b.loadMoreConversations.Hide()
	b.showEmptyChat("Select an existing conversation to open its live chat.")
	b.refreshTasks()
}

func (b *Browser) changeTaskSearch(query string) {
	previousAllStates := taskSearchIncludesAllStates(b.selection.Status, b.selection.TaskFilter)
	b.selection.TaskFilter = query
	allStates := taskSearchIncludesAllStates(b.selection.Status, query)
	if previousAllStates != allStates {
		b.taskItems = nil
		b.visibleTasks = nil
		b.applyTaskFilter()
		b.refreshTasks()
	} else {
		b.applyTaskFilter()
	}
	b.persist()
}

func (b *Browser) applyProjectFilter() {
	b.visibleProjects = b.filteredProjectRail()
	rowHeight := b.projects.CreateItem().MinSize().Height
	for i := range b.visibleProjects {
		height := rowHeight
		if b.projectGroupAt(i) != "" {
			height += browserSectionHeading("FAVORITES").MinSize().Height + theme.Padding()
		}
		b.projects.SetItemHeight(i, height)
	}
	b.projects.Refresh()
	b.syncProjectSelection()
}
func (b *Browser) applyTaskFilter() {
	b.visibleTasks = includeTaskParents(FilterTasks(b.taskItems, b.selection.TaskFilter), b.taskItems)
	sortVisibleTasks(b.visibleTasks)
	b.visibleTasks = groupTaskRows(b.visibleTasks)
	b.tasks.Refresh()
	b.syncTaskSelection()
}
func (b *Browser) toggleFavorite() {
	b.toggleFavoriteFor(b.selectedProject)
}

func (b *Browser) toggleFavoriteFor(project string) {
	if directory := b.localRailProjects[project]; directory != "" {
		toggleProjectFavorite(b.prefs.Favorites, projectFavoriteKeys("", directory, ""))
		b.applyProjectFilter()
		b.persist()
		return
	}
	keys := projectFavoriteKeys(b.endpoint, "", project, b.clusterID)
	if project == b.selectedProject {
		keys = b.activeProjectFavoriteKeys(project)
	}
	if len(keys) == 0 {
		return
	}
	toggleProjectFavorite(b.prefs.Favorites, keys)
	b.updateAutomaticWorkerScopes()
	b.selectedProjectFavorite.SetText(b.favoriteText(b.selectedProject))
	b.applyProjectFilter()
	b.persist()
}
func (b *Browser) favoriteText(project string) string {
	if b.railProjectFavorite(project) {
		return "Unfavorite"
	}
	return "Favorite"
}

func (b *Browser) syncProjectSelection() {
	for i, item := range b.visibleProjects {
		if item.Ident == b.selectedRailProject() {
			b.projects.Select(i)
			return
		}
	}
	b.projects.UnselectAll()
}

func (b *Browser) syncTaskSelection() {
	if b.taskChatPage != nil && b.taskChatPage.Visible() {
		return
	}
	for i, item := range b.visibleTasks {
		if item.ID == b.selectedTask {
			b.tasks.Select(i)
			return
		}
	}
	b.tasks.UnselectAll()
}

func (b *Browser) syncConversationSelection() {
	for i, item := range b.conversationItems {
		if item.ID == b.selectedConversation {
			b.conversations.Select(i)
			return
		}
	}
	b.conversations.UnselectAll()
}
func (b *Browser) persist() {
	if b.prefs.Selections == nil {
		b.prefs.Selections = make(map[string]NavigationSelection)
	}
	if b.activeWorkspaceID != "" {
		b.prefs.Selections[workspaceSelectionKey(b.prefKey, b.activeWorkspaceID)] = b.selection
		if b.prefs.WorkspaceIDs == nil {
			b.prefs.WorkspaceIDs = make(map[string][]string)
		}
		b.prefs.WorkspaceIDs[b.prefKey] = append([]string(nil), b.workspaceIDs...)
		if b.prefs.ActiveWorkspace == nil {
			b.prefs.ActiveWorkspace = make(map[string]string)
		}
		b.prefs.ActiveWorkspace[b.prefKey] = b.activeWorkspaceID
	} else if len(b.workspaceIDs) == 0 {
		// Keep the legacy selection key for Browser helpers without a workspace.
		b.prefs.Selections[b.prefKey] = b.selection
	}
	if b.prefsPath != "" {
		if err := SaveNavigationPreferences(b.prefsPath, b.prefs); err != nil {
			b.browserStatus.SetText("Preferences could not be saved: " + err.Error())
		}
	}
}
func (b *Browser) showEmptyChat(message string) {
	if b.taskChatPage != nil {
		b.taskChatPage.Hide()
		b.taskDetailPage.Show()
	}
	b.chatHost.Objects = []fyne.CanvasObject{widget.NewLabel(message)}
	b.chatHost.Refresh()
}

func (b *Browser) showTaskChat() {
	b.taskDetailPage.Hide()
	b.taskChatPage.Show()
	// Fyne does not dispatch OnSelected when an already-selected row is tapped.
	// The card still shows the active task from selectedTask.
	b.tasks.UnselectAll()
}

func (b *Browser) backToTaskDetail() {
	b.selectedConversation = ""
	b.selection.Conversation = ""
	b.conversations.UnselectAll()
	b.showEmptyChat("Select an existing conversation to open its live chat.")
	b.syncTaskSelection()
	b.persist()
}
func validBrowserStatus(status string) bool {
	for _, value := range browserStatusValues {
		if status == value {
			return true
		}
	}
	return false
}
func projectExists(items []storage.ProjectOverview, ident string) bool {
	for _, item := range items {
		if item.Ident == ident {
			return true
		}
	}
	return false
}
func taskExists(items []storage.TaskSummary, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}
func chatKey(project, conversation string) string { return project + "\x00" + conversation }

// conversationLabel reads "host - Name (Provider) @ date". The host comes from
// the fleet record or the owner's reported presence and is omitted when unknown.
func conversationLabel(c storage.Conversation) string {
	date := time.UnixMilli(c.CreatedAt).Format("Jan 2, 2006 15:04")
	agent := worker.AgentName(c.Owner.SessionID) + " (" + providerConversationLabel(c.Provider) + ")"
	if host := conversationHostname(c); host != "" {
		agent = host + " - " + agent
	}
	return agent + " @ " + date
}

func conversationHostname(c storage.Conversation) string {
	for _, host := range []*string{c.FleetHostname, c.OwnerHostname} {
		if host != nil && strings.TrimSpace(*host) != "" {
			return strings.TrimSpace(*host)
		}
	}
	return ""
}
