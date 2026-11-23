package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	b.selection.MainView = name
	b.persist()
	b.updateProjectRail()
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

func (b *Browser) toggleConversationProjects() {
	b.conversationProjectsExpanded = !b.conversationProjectsExpanded
	b.updateProjectRail()
}

func (b *Browser) updateProjectRail() {
	if b.mainView == "Conversations" && !b.conversationProjectsExpanded {
		b.root.Leading = b.compactProjectRail
		b.root.SetOffset(0)
	} else {
		b.root.Leading = b.projectRail
		b.root.SetOffset(.16)
	}
	if b.mainView == "Conversations" {
		b.collapseConversationProjects.Show()
	} else {
		b.collapseConversationProjects.Hide()
	}
	b.root.Refresh()
}

func providerConversationLabel(provider string) string {
	switch provider {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "antigravity":
		return "agy"
	default:
		return provider
	}
}

var defaultChatProviders = [...]string{"claude", "codex", "antigravity"}

func (b *Browser) openProviderConversation(provider string) {
	if b.projectConversationsLoading || b.selectedProject == "" || b.defaultChats == nil {
		return
	}
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
	machine := "This machine: unavailable"
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
		after := ""
		for {
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
				b.newConversationStatus.SetText("Conversations unavailable: " + err.Error())
				return
			}
			sort.SliceStable(items, func(i, j int) bool {
				return items[i].UpdatedAt > items[j].UpdatedAt
			})
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
			if b.selection.Conversation != "" {
				for _, chat := range items {
					if chat.ID == b.selection.Conversation {
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
		b.selectedConversation = chat.ID
		b.selection.Conversation = chat.ID
		b.persist()
		content := child.Content()
		if chat.TaskID != "" {
			link := widget.NewButton("Go to task", func() {
				b.setMainView("Tasks")
				b.selectTask(chat.TaskID)
			})
			content = container.NewBorder(container.NewHBox(link), nil, nil, nil, content)
		} else if b.workerPanel != nil {
			clear := widget.NewButton("Clear", func() {
				b.conversationSelection++
				b.startStandaloneConversation(chat.ProjectIdent, b.selection.CheckoutPath, chat.Provider)
			})
			content = container.NewBorder(container.NewHBox(clear), nil, nil, nil, content)
		}
		b.standaloneChatHost.Objects = []fyne.CanvasObject{content}
		b.standaloneChatHost.Refresh()
	}
}

func (b *Browser) startStandaloneConversation(project, directory, provider string) {
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
	selection := b.conversationSelection
	b.newConversationStatus.SetText("Starting " + providerConversationLabel(provider) + "…")
	if !b.launchWithTimeout(0, func(ctx context.Context) {
		err := manager.RunConversationWithCommand(ctx, project, directory, provider, command, func(chat storage.Conversation) {
			b.dispatch(func() {
				delete(b.openingDefaults, key)
				if project != b.selectedProject {
					return
				}
				b.newConversationStatus.SetText("")
				b.loadProjectConversations()
				if b.mainView == "Conversations" && selection == b.conversationSelection {
					b.openStandaloneConversation(chat)
				}
			})
		})
		b.dispatch(func() {
			if err != nil {
				delete(b.openingDefaults, key)
				if ctx.Err() == nil && b.selectedProject == project {
					b.newConversationStatus.SetText("Conversation worker stopped: " + err.Error())
				}
			}
		})
	}) {
		delete(b.openingDefaults, key)
		b.newConversationStatus.SetText("The workspace is unavailable.")
	}
}

func (b *Browser) resumeDefaultConversation(chat storage.Conversation) {
	if b.workerPanel == nil {
		return
	}
	manager, ok := b.workerPanel.manager.(*WorkerManager)
	if !ok {
		return
	}
	b.launchWithTimeout(0, func(ctx context.Context) {
		err := manager.ResumeConversation(ctx, chat)
		if err != nil && !errors.Is(err, storage.ErrConversationConflict) && ctx.Err() == nil {
			b.dispatch(func() {
				if b.selectedProject == chat.ProjectIdent {
					b.newConversationStatus.SetText("Conversation worker stopped: " + err.Error())
				}
			})
		}
	})
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
