package desktop

import (
	"context"
	"fmt"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/internal/localconversation"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

type approvalNotice struct {
	run     storage.WorkerRun
	title   string
	local   *storage.Conversation
	options []conversation.PermissionOption
	resolve func(context.Context, string, string) error
}

type approvalKey struct {
	project, conversation, instance, request string
}

func approvalNotification(project, provider, title string) *fyne.Notification {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Agent requests approval"
	}
	return fyne.NewNotification("Approval needed: "+title, project+" · "+providerConversationLabel(provider)+" · Open Events to review")
}

func (b *Browser) watchApprovals(open worker.Open, local *storage.Conversation) worker.Open {
	if open == nil {
		open = worker.OpenNative
	}
	return func(ctx context.Context, run storage.WorkerRun, sink conversation.Sink) (worker.Session, error) {
		key := approvalKey{project: run.Project, conversation: run.ConversationID, instance: run.Owner.InstanceID}
		active := &approvalSession{}
		ready := make(chan struct{})
		report := func(event conversation.Event) {
			request := key
			request.request = event.RequestID
			b.dispatch(func() {
				if b.closing {
					return
				}
				switch event.Kind {
				case "permission":
					if request.request == "" {
						return
					}
					if _, exists := b.approvalNotices[request]; exists {
						return
					}
					title := strings.TrimSpace(event.Title)
					if title == "" {
						title = "Agent requests approval"
					}
					if b.approvalNotices == nil {
						b.approvalNotices = make(map[approvalKey]approvalNotice)
					}
					notice := approvalNotice{run: run, title: title, local: local}
					if run.LocalTaskID != "" {
						notice.options = event.Options
						notice.resolve = func(ctx context.Context, requestID, optionID string) error {
							select {
							case <-ready:
								if active.Session != nil {
									return active.ResolvePermission(ctx, requestID, optionID)
								}
								return context.Canceled
							case <-ctx.Done():
								return ctx.Err()
							}
						}
					}
					b.approvalNotices[request] = notice
					if app := fyne.CurrentApp(); app != nil {
						app.SendNotification(approvalNotification(run.Project, run.Owner.Provider, title))
					}
				case "permission_resolved":
					delete(b.approvalNotices, request)
				case "turn_completed", "closed":
					for pending := range b.approvalNotices {
						if pending.project == key.project && pending.conversation == key.conversation && pending.instance == key.instance {
							delete(b.approvalNotices, pending)
						}
					}
				}
				b.refreshApprovalNotices()
			})
		}
		session, err := open(ctx, run, func(eventCtx context.Context, event conversation.Event) error {
			if err := sink(eventCtx, event); err != nil {
				return err
			}
			switch event.Kind {
			case "permission", "permission_resolved", "turn_completed":
				report(event)
			}
			return nil
		})
		if err != nil {
			close(ready)
			report(conversation.Event{Kind: "closed"})
			return nil, err
		}
		active.Session, active.report = session, report
		close(ready)
		return active, nil
	}
}

type approvalSession struct {
	worker.Session
	report func(conversation.Event)
}

func (s *approvalSession) ResolvePermission(ctx context.Context, requestID, optionID string) error {
	if err := s.Session.ResolvePermission(ctx, requestID, optionID); err != nil {
		return err
	}
	s.report(conversation.Event{Kind: "permission_resolved", RequestID: requestID})
	return nil
}

func (s *approvalSession) Prompt(ctx context.Context, text string) (string, error) {
	defer s.report(conversation.Event{Kind: "turn_completed"})
	return s.Session.Prompt(ctx, text)
}

func (s *approvalSession) Close() error {
	defer s.report(conversation.Event{Kind: "closed"})
	return s.Session.Close()
}

func (b *Browser) refreshApprovalNotices() {
	if b.mainViewButtons["Events"] != nil {
		label := "Events"
		if len(b.approvalNotices) > 0 {
			count := len(b.approvalNotices)
			label = fmt.Sprintf("Events · %d approval", count)
			if count != 1 {
				label += "s"
			}
			b.mainViewButtons["Events"].Importance = widget.HighImportance
		} else {
			b.mainViewButtons["Events"].Importance = widget.MediumImportance
		}
		b.mainViewButtons["Events"].SetText(label)
	}
	if b.approvalBanner != nil {
		b.approvalBanner.RemoveAll()
		if len(b.approvalNotices) == 0 {
			b.approvalBanner.Hide()
		} else {
			count := len(b.approvalNotices)
			var first approvalNotice
			for _, notice := range b.approvalNotices {
				first = notice
				break
			}
			title := strings.TrimSpace(first.title)
			if title == "" {
				title = "Agent requests approval"
			}
			message := fmt.Sprintf("%d approval", count)
			if count != 1 {
				message += "s"
			}
			message += " need your review · " + first.run.Project + " · " + title
			label := widget.NewLabel(message)
			label.Wrapping = fyne.TextWrapWord
			var action *widget.Button
			if count == 1 {
				action = widget.NewButton("Open conversation", func() { b.openApprovalConversation(first) })
			} else {
				action = widget.NewButton("Review approvals", func() { b.setMainView("Events") })
			}
			b.approvalBanner.Add(container.NewBorder(nil, nil, nil, action, label))
			b.approvalBanner.Show()
		}
		if b.approvalWatchUnavailable {
			warning := widget.NewLabel("Approval notifications unavailable. Reconnecting to the local client…")
			warning.Wrapping = fyne.TextWrapWord
			warning.Importance = widget.WarningImportance
			b.approvalBanner.Add(warning)
			b.approvalBanner.Show()
		}
	}
	if b.approvalRows == nil {
		return
	}
	b.approvalRows.RemoveAll()
	if len(b.approvalNotices) == 0 {
		return
	}
	b.approvalRows.Add(browserSectionHeading("NEEDS APPROVAL"))
	for key, notice := range b.approvalNotices {
		label := widget.NewLabel(notice.run.Project + " · " + providerConversationLabel(notice.run.Owner.Provider) + " · " + notice.title)
		label.Wrapping = fyne.TextWrapWord
		open := widget.NewButton("Open conversation", func() { b.openApprovalConversation(notice) })
		actions := container.NewHBox(open)
		for _, option := range notice.options {
			actions.Add(widget.NewButton(option.Name, func() {
				b.launch(func(ctx context.Context) {
					if err := notice.resolve(ctx, key.request, option.ID); err != nil {
						b.dispatch(func() { b.browserStatus.SetText("Approval response failed: " + err.Error()) })
					}
				})
			}))
		}
		b.approvalRows.Add(container.NewBorder(nil, nil, nil, actions, label))
	}
}

func (b *Browser) openApprovalConversation(notice approvalNotice) {
	open := func(chat storage.Conversation) {
		if b.selectedProject != chat.ProjectIdent {
			b.selectProject(chat.ProjectIdent)
		}
		if chat.TaskID != "" {
			b.selectTask(chat.TaskID)
			b.openTaskThread(chat)
			return
		}
		b.setMainView("Conversations")
		b.openStandaloneConversation(chat)
	}
	if notice.local != nil {
		open(*notice.local)
		return
	}
	b.launch(func(ctx context.Context) {
		var chat *storage.Conversation
		var err error
		if notice.run.LocalTaskID != "" {
			items, readErr := listLocalTaskThreads(b.localConversationsDirectory, notice.run.Project, notice.run.LocalTaskID)
			err = readErr
			for _, item := range items {
				if item.ID == notice.run.ConversationID {
					chat = &item
					break
				}
			}
		} else {
			chat, err = b.client.Conversation(ctx, notice.run.Project, notice.run.ConversationID)
		}
		b.dispatch(func() {
			if b.closing {
				return
			}
			if err != nil || chat == nil {
				b.browserStatus.SetText("Approval conversation is unavailable.")
				return
			}
			open(*chat)
		})
	})
}

func (b *Browser) watchManagedApprovals(ctx context.Context) {
	remote := localconversation.Remote{ConfigPath: b.clientConfigPath}
	for ctx.Err() == nil {
		err := remote.WatchApprovals(ctx, func(approvals []localconversation.Approval) error {
			b.dispatch(func() {
				if b.closing {
					return
				}
				b.approvalWatchUnavailable = false
				if b.approvalNotices == nil {
					b.approvalNotices = make(map[approvalKey]approvalNotice)
				}
				unseen := make(map[approvalKey]bool)
				for _, approval := range approvals {
					info, event := approval.Conversation, approval.Event
					key := approvalKey{info.ProjectIdent, info.ID, "local-client", event.Event.RequestID}
					unseen[key] = true
					if _, ok := b.approvalNotices[key]; ok {
						continue
					}
					title := strings.TrimSpace(event.Event.Title)
					if title == "" {
						title = "Agent requests approval"
					}
					notice := approvalNotice{
						run:   storage.WorkerRun{Project: info.ProjectIdent, ConversationID: info.ID, Owner: storage.AgentOrigin{Provider: info.Provider}},
						local: &info, title: title, options: event.Event.Options,
						resolve: func(ctx context.Context, _, option string) error {
							return remote.Enqueue(ctx, info.ProjectIdent, info.ID, uuid.NewString(), conversation.Command{
								Kind: "permission", PermissionEventID: event.ID, OptionID: option,
							})
						},
					}
					if approval.Task {
						if info.TaskID == "" {
							notice.local = nil
						}
						notice.resolve = func(ctx context.Context, _, option string) error {
							return remote.ResolveTaskApproval(ctx, event.ID, option)
						}
					}
					b.approvalNotices[key] = notice
					if app := fyne.CurrentApp(); app != nil {
						app.SendNotification(approvalNotification(info.ProjectIdent, info.Provider, title))
					}
				}
				for key := range b.approvalNotices {
					if key.instance == "local-client" && !unseen[key] {
						delete(b.approvalNotices, key)
					}
				}
				b.refreshApprovalNotices()
			})
			return ctx.Err()
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			b.dispatch(func() {
				if !b.closing {
					b.approvalWatchUnavailable = true
					b.refreshApprovalNotices()
				}
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
