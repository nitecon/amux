package desktop

import (
	"sort"
	"strings"

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

func conversationUpdatedAt(c storage.Conversation) int64 {
	if c.UpdatedAt > 0 {
		return c.UpdatedAt
	}
	return c.CreatedAt
}

func sortTaskThreads(runs []storage.Conversation) {
	sort.SliceStable(runs, func(i, j int) bool {
		a, b := runs[i], runs[j]
		aUpdated, bUpdated := conversationUpdatedAt(a), conversationUpdatedAt(b)
		if aUpdated != bUpdated {
			return aUpdated > bUpdated
		}
		aLabel, bLabel := conversationLabel(a), conversationLabel(b)
		if aLower, bLower := strings.ToLower(aLabel), strings.ToLower(bLabel); aLower != bLower {
			return aLower < bLower
		}
		if aLabel != bLabel {
			return aLabel < bLabel
		}
		return a.ID < b.ID
	})
}

func mergeTaskThreadMetadata(dst, src storage.Conversation) storage.Conversation {
	if src.UpdatedAt > dst.UpdatedAt {
		dst.UpdatedAt = src.UpdatedAt
	}
	if dst.CreatedAt == 0 && src.CreatedAt != 0 {
		dst.CreatedAt = src.CreatedAt
	}
	if (dst.FleetHostname == nil || strings.TrimSpace(*dst.FleetHostname) == "") &&
		src.FleetHostname != nil && strings.TrimSpace(*src.FleetHostname) != "" {
		dst.FleetHostname = src.FleetHostname
	}
	if (dst.OwnerHostname == nil || strings.TrimSpace(*dst.OwnerHostname) == "") &&
		src.OwnerHostname != nil && strings.TrimSpace(*src.OwnerHostname) != "" {
		dst.OwnerHostname = src.OwnerHostname
	}
	if dst.TaskTitle == "" && src.TaskTitle != "" {
		dst.TaskTitle = src.TaskTitle
	}
	return dst
}

func (b *Browser) taskThreadRuns(current storage.Conversation) []storage.Conversation {
	byID := make(map[string]storage.Conversation)
	add := func(chat storage.Conversation) {
		if chat.ID == "" || chat.TaskID != current.TaskID || chat.ProjectIdent != current.ProjectIdent {
			return
		}
		if existing, ok := byID[chat.ID]; ok {
			byID[chat.ID] = mergeTaskThreadMetadata(existing, chat)
			return
		}
		byID[chat.ID] = chat
	}
	for _, chat := range b.conversationHistory {
		add(chat)
	}
	for _, chat := range b.conversationItems {
		add(chat)
	}
	for _, chat := range b.taskThreadItems {
		add(chat)
	}
	add(current)
	runs := make([]storage.Conversation, 0, len(byID))
	for _, chat := range byID {
		runs = append(runs, chat)
	}
	sortTaskThreads(runs)
	return runs
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
	runs := b.taskThreadRuns(current)
	b.taskThreadItems = runs
	for _, chat := range runs {
		chat := chat
		button := widget.NewButton(conversationLabel(chat), func() {
			b.conversationSelection++
			b.openStandaloneConversation(chat)
		})
		if chat.ID == current.ID {
			button.Importance = widget.HighImportance
		}
		tabs.Add(button)
	}
	header := container.NewVBox(container.NewHBox(title, layout.NewSpacer(), copyTaskIDButton(func() string { return current.TaskID }), back), container.NewHScroll(tabs))
	return browserPanel(container.NewThemeOverride(header, controls))
}
