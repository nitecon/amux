package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
)

func (b *Browser) setMainView(name string) {
	page := b.mainViews[name]
	if page == nil {
		return
	}
	b.mainView = name
	if name != "Checkout" {
		b.selection.MainView = name
		b.persist()
	}
	b.mainContent.Objects = []fyne.CanvasObject{page}
	b.mainContent.Refresh()
	for label, button := range b.mainViewButtons {
		button.Importance = widget.MediumImportance
		if label == name {
			button.Importance = widget.HighImportance
		}
		button.Refresh()
	}
	b.refreshMainView()
}

func (b *Browser) refreshMainView() {
	switch b.mainView {
	case "Board":
		b.taskBoard.refresh()
	case "Conversations":
		b.loadProjectConversations()
	case "Events":
		b.loadEventView()
	case "Alerts":
		b.loadAlertsView()
	case "Git":
		b.refreshWorktreeStatus()
	}
}

func providerConversationLabel(provider string) string {
	switch provider {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "antigravity":
		return "Agy"
	default:
		return provider
	}
}

var defaultChatProviders = [...]string{"claude", "codex", "antigravity"}

func (b *Browser) openProviderConversation(provider string) {
	b.activeConversationProvider = provider
	b.refreshConversationLaunchControls()
	if b.selectedProject == "" {
		return
	}
	if b.projectConversationsLoading {
		b.pendingConversationProvider = provider
		return
	}
	if b.defaultChats == nil {
		b.pendingConversationProvider = provider
		b.loadProjectConversations()
		return
	}
	b.pendingConversationProvider = ""
	if chat, ok := b.defaultChats[provider]; ok {
		b.openStandaloneConversation(chat)
		b.resumeDefaultConversation(chat)
		return
	}
	b.startStandaloneConversation(b.selectedProject, b.selection.CheckoutPath, provider)
}

func (b *Browser) setGatewayReconnecting(reconnecting bool) {
	if b.navigationReconnecting == reconnecting {
		return
	}
	b.navigationReconnecting = reconnecting
	if reconnecting {
		b.headerGateway.SetText("Gateway: reconnecting…\nThis machine: unavailable")
	} else {
		b.headerGateway.SetText("Gateway: connected\nRefreshing presence…")
	}
	b.shell.Refresh()
}

func (b *Browser) applyHeaderPresence(summary storage.PresenceSummary, err error) {
	if b.navigationReconnecting {
		return
	}
	previous := b.headerGateway.Text
	defer func() {
		if b.headerGateway.Text != previous {
			b.shell.Refresh()
		}
	}()
	if err != nil {
		b.headerGateway.SetText("Gateway: unavailable\nThis machine: unavailable")
		return
	}
	platforms := strings.Join(summary.OS, ", ")
	if platforms != "" {
		platforms = " · " + platforms
	}
	machine := "This machine: counts not reported by gateway"
	if summary.ThisMachine != nil {
		machine = fmt.Sprintf("This machine: %d agents · %d busy · %d idle", summary.ThisMachine.Agents, summary.ThisMachine.Active, summary.ThisMachine.Idle)
	}
	b.headerGateway.SetText(fmt.Sprintf("Gateway: connected · %d machines%s · %d agents · %d busy · %d idle\n%s",
		summary.Machines, platforms, summary.Agents, summary.Active, summary.Idle, machine))
}

func (b *Browser) loadProjectConversations() {
	b.projectConversationGeneration++
	generation, project := b.projectConversationGeneration, b.selectedProject
	b.projectConversationItems = nil
	b.conversationHistory = nil
	b.defaultChats = nil
	b.projectConversationsLoading = false
	b.projectConversationList.Refresh()
	if project == "" {
		return
	}
	b.projectConversationsLoading = true
	if !b.launch(func(ctx context.Context) {
		var items []storage.Conversation
		var err error
		if b.localConversationsDirectory != "" {
			items, err = listMarkdownChats(b.localConversationsDirectory, project)
		}
		for after := ""; b.localConversationsDirectory == ""; {
			var page []storage.Conversation
			page, err = b.client.ProjectConversations(ctx, project, after, browserConversationPage)
			items = append(items, page...)
			if err != nil || len(page) < browserConversationPage {
				break
			}
			after = page[len(page)-1].ID
		}
		b.dispatch(func() {
			if project != b.selectedProject || generation != b.projectConversationGeneration {
				return
			}
			b.projectConversationsLoading = false
			if err != nil {
				var response *gatewayclient.HTTPError
				message := "Conversations unavailable: " + err.Error()
				if errors.As(err, &response) && response.Status == http.StatusNotFound {
					message = "This gateway does not expose the Conversations API."
				}
				if b.pendingConversationProvider != "" || b.selection.ProjectConversation == "" {
					b.standaloneChatHost.Objects = []fyne.CanvasObject{widget.NewLabel(message)}
					b.standaloneChatHost.Refresh()
				} else {
					b.newConversationStatus.SetText(message)
				}
				b.pendingConversationProvider = ""
				return
			}
			sort.SliceStable(items, func(i, j int) bool {
				return items[i].UpdatedAt > items[j].UpdatedAt
			})
			b.conversationHistory = items
			b.defaultChats = make(map[string]storage.Conversation)
			for _, chat := range items {
				if chat.TaskID != "" {
					b.projectConversationItems = append(b.projectConversationItems, chat)
					continue
				}
				current := b.defaultChats[chat.Provider]
				if current.ID == "" || chat.CreatedAt > current.CreatedAt {
					b.defaultChats[chat.Provider] = chat
				}
			}
			b.projectConversationList.Refresh()
			if provider := b.pendingConversationProvider; provider != "" {
				b.openProviderConversation(provider)
				return
			}
			if b.selection.ProjectConversation != "" {
				for _, chat := range items {
					if chat.ID == b.selection.ProjectConversation {
						b.openStandaloneConversation(chat)
						if chat.TaskID == "" {
							b.resumeDefaultConversation(chat)
						}
						break
					}
				}
			}
		})
	}) {
		b.projectConversationsLoading = false
	}
}

func (b *Browser) openStandaloneConversation(chat storage.Conversation) {
	if chat.ProjectIdent != b.selectedProject {
		return
	}
	if child := b.chatForConversation(chat); child != nil {
		b.selection.ProjectConversation = chat.ID
		b.persist()
		content := child.Content()
		if chat.TaskID != "" {
			content = container.NewBorder(b.taskThreadHeader(chat), nil, nil, nil, content)
			b.activeConversationProvider = ""
		} else {
			b.activeConversationProvider = chat.Provider
		}
		b.refreshConversationLaunchControls()
		b.standaloneChatHost.Objects = []fyne.CanvasObject{content}
		b.standaloneChatHost.Refresh()
	}
}

func (b *Browser) startStandaloneConversation(project, directory, provider string) {
	b.launchStandaloneConversation(project, directory, provider, "")
}

func (b *Browser) launchStandaloneConversation(project, directory, provider, replace string) {
	if b.localConversationsDirectory != "" {
		b.createLocalConversation(project, directory, provider, replace)
		return
	}
	if b.workerPanel == nil {
		b.newConversationStatus.SetText("A local worker is required to start a default chat.")
		return
	}
	manager, ok := b.workerPanel.manager.(*WorkerManager)
	if !ok {
		return
	}
	command, commandErr := providerLaunchCommand(provider, b.conversationLaunchPreferences(provider))
	if commandErr != nil {
		b.newConversationStatus.SetText(commandErr.Error())
		return
	}
	key := chatKey(project, provider)
	if b.openingDefaults[key] {
		return
	}
	if b.openingDefaults == nil {
		b.openingDefaults = make(map[string]bool)
	}
	b.openingDefaults[key] = true
	if b.resumingConversations == nil {
		b.resumingConversations = make(map[string]context.CancelFunc)
	}
	selection := b.conversationSelection
	b.newConversationStatus.SetText("Starting " + providerConversationLabel(provider) + "…")
	if !b.launchWithTimeout(0, func(ctx context.Context) {
		runContext, stop := context.WithCancel(ctx)
		defer stop()
		var createdID string
		err := manager.RunConversationWithCommand(runContext, project, directory, provider, command, func(chat storage.Conversation) {
			createdID = chat.ID
			b.dispatch(func() {
				b.resumingConversations[chatKey(project, chat.ID)] = stop
				if replace != "" {
					if stopPrevious := b.resumingConversations[chatKey(project, replace)]; stopPrevious != nil {
						stopPrevious()
					}
				}
				delete(b.openingDefaults, key)
				if project != b.selectedProject {
					return
				}
				b.newConversationStatus.SetText("")
				if b.mainView == "Conversations" && selection == b.conversationSelection {
					b.openStandaloneConversation(chat)
				}
				b.loadProjectConversations()
			})
		})
		b.dispatch(func() {
			if createdID != "" {
				delete(b.resumingConversations, chatKey(project, createdID))
			} else {
				delete(b.openingDefaults, key)
			}
			if err != nil && ctx.Err() == nil && b.selectedProject == project && b.activeConversationProvider == provider &&
				(createdID == "" || b.selection.ProjectConversation == createdID) {
				b.newConversationStatus.SetText("Conversation worker stopped: " + err.Error())
			}
		})
	}) {
		delete(b.openingDefaults, key)
		b.newConversationStatus.SetText("The workspace is unavailable.")
	}
}

func (b *Browser) resumeDefaultConversation(chat storage.Conversation) {
	if b.localConversationsDirectory != "" || b.workerPanel == nil || b.ctx == nil {
		return
	}
	manager, ok := b.workerPanel.manager.(*WorkerManager)
	if !ok {
		return
	}
	key := chatKey(chat.ProjectIdent, chat.ID)
	if b.resumingConversations == nil {
		b.resumingConversations = make(map[string]context.CancelFunc)
	}
	if b.resumingConversations[key] != nil {
		return
	}
	runContext, stop := context.WithCancel(b.ctx)
	b.resumingConversations[key] = stop
	if !b.launchWithTimeout(0, func(ctx context.Context) {
		defer stop()
		err := manager.ResumeConversation(runContext, chat)
		b.dispatch(func() {
			delete(b.resumingConversations, key)
			if err != nil && !errors.Is(err, storage.ErrConversationConflict) && ctx.Err() == nil &&
				b.selectedProject == chat.ProjectIdent && b.selection.ProjectConversation == chat.ID {
				b.newConversationStatus.SetText("Conversation worker stopped: " + err.Error())
			}
		})
	}) {
		stop()
		delete(b.resumingConversations, key)
	}
}

func (b *Browser) loadEventView() {
	project := b.selectedProject
	host := b.mainViews["Events"].(*fyne.Container)
	host.Objects = []fyne.CanvasObject{widget.NewLabel("Loading events…")}
	host.Refresh()
	b.launch(func(ctx context.Context) {
		events, err := b.client.TaskEvents(ctx, project)
		b.dispatch(func() {
			if project != b.selectedProject || b.mainView != "Events" {
				return
			}
			rows := container.NewVBox(browserSectionHeading("EVENTS"))
			if err != nil {
				rows.Add(widget.NewLabel("Events unavailable: " + err.Error()))
			} else if len(events) == 0 {
				rows.Add(widget.NewLabel("No recent events."))
			}
			for _, event := range events {
				var task struct{ Title string }
				_ = json.Unmarshal(event.Task, &task)
				rows.Add(widget.NewLabel(event.Kind + " · " + task.Title))
			}
			host.Objects = []fyne.CanvasObject{container.NewVScroll(rows)}
			host.Refresh()
		})
	})
}

// Review notices use the durable task journal so a navigation reconnect or
// periodic refresh can pick up events that the desktop did not see live.
func (b *Browser) refreshReviewNotices(project string) {
	if b.reviewNoticesLoading {
		return
	}
	cursor, initialized := b.reviewNoticeCursors[project]
	var after *int64
	if initialized {
		after = &cursor
	}
	b.reviewNoticesLoading = b.launch(func(ctx context.Context) {
		events, err := b.client.TaskEventsAfter(ctx, project, after)
		b.dispatch(func() {
			b.reviewNoticesLoading = false
			if err != nil || b.closing {
				return
			}
			if b.reviewNoticeCursors == nil {
				b.reviewNoticeCursors = make(map[string]int64)
			}
			for _, event := range events {
				if event.ID <= b.reviewNoticeCursors[project] {
					continue
				}
				b.reviewNoticeCursors[project] = event.ID
				if !initialized || event.Kind != "task_user_review" {
					continue
				}
				var task struct{ Title string }
				if app := fyne.CurrentApp(); app != nil && json.Unmarshal(event.Task, &task) == nil && task.Title != "" {
					app.SendNotification(fyne.NewNotification("Task ready for review", project+" · "+task.Title))
				}
			}
			if len(events) == 0 {
				b.reviewNoticeCursors[project] = cursor
			}
		})
	})
}

func (b *Browser) loadAlertsView() {
	project := b.selectedProject
	b.showCheckoutAlerts(project, b.checkoutItems)
	b.alertItems = nil
	b.alertList.Refresh()
	if project == "" {
		b.alertStatus.SetText("Select a project to view alerts.")
		return
	}
	b.alertStatus.SetText("Loading alerts…")
	b.launch(func(ctx context.Context) {
		checkouts, err := b.client.Checkouts(ctx)
		b.dispatch(func() {
			if err == nil && project == b.selectedProject && b.mainView == "Alerts" {
				b.showCheckoutAlerts(project, checkouts)
			}
		})
	})
	b.launch(func(ctx context.Context) {
		result, err := b.client.ListProjectMessageThreads(ctx, project, gatewayclient.ProjectMessageListQuery{State: "unresolved"})
		b.dispatch(func() {
			if project != b.selectedProject || b.mainView != "Alerts" {
				return
			}
			if err != nil {
				b.alertStatus.SetText("Alerts unavailable: " + err.Error())
				return
			}
			b.alertItems = result.Threads
			b.alertStatus.SetText(fmt.Sprintf("%d unresolved threads shown", len(b.alertItems)))
			b.alertList.Refresh()
		})
	})
}

func (b *Browser) showCheckoutAlerts(project string, checkouts []storage.CheckoutOverview) {
	b.checkoutAlert.Hide()
	for _, checkout := range checkouts {
		if checkout.Ident == project && len(checkout.UnavailableHosts) > 0 {
			b.checkoutAlert.SetText("Checkout reported unavailable on " + strings.Join(checkout.UnavailableHosts, ", ") + ".")
			b.checkoutAlert.Show()
			return
		}
	}
}
