package desktop

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/storage"
)

func (b *Browser) openTaskThread(chat storage.Conversation) {
	if chat.ProjectIdent != b.selectedProject || chat.TaskID == "" {
		return
	}
	b.pendingConversationProvider = ""
	b.conversationSelection++
	b.setMainView("Conversations")
	b.openStandaloneConversation(chat)
}

func (b *Browser) taskThreadHeader(current storage.Conversation) fyne.CanvasObject {
	title := widget.NewLabelWithStyle("Task Thread", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	back := widget.NewButtonWithIcon("Go to task", theme.NavigateBackIcon(), func() {
		b.setMainView("Tasks")
		if b.selectedTask != current.TaskID {
			b.selectTask(current.TaskID)
		}
	})
	controls := toolbarTheme{Theme: fyne.CurrentApp().Settings().Theme()}
	tabs := container.NewHBox()
	seen := make(map[string]bool)
	for _, chat := range append([]storage.Conversation{current}, b.conversationItems...) {
		if chat.TaskID != current.TaskID || chat.ProjectIdent != current.ProjectIdent {
			continue
		}
		agent := chat.Owner.SessionID
		if agent == "" {
			agent = chat.ID
		}
		if seen[agent] {
			continue
		}
		seen[agent] = true
		button := widget.NewButton(providerConversationLabel(chat.Provider)+" · "+shortID(agent), func() {
			b.conversationSelection++
			b.openStandaloneConversation(chat)
		})
		if chat.ID == current.ID {
			button.Importance = widget.HighImportance
		}
		tabs.Add(button)
	}
	header := container.NewVBox(container.NewHBox(title, layout.NewSpacer(), back), container.NewHScroll(tabs))
	return browserPanel(container.NewThemeOverride(header, controls))
}
