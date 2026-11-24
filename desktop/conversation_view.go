package desktop

import (
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

func (b *Browser) buildConversationView(controls fyne.Theme) fyne.CanvasObject {
	tabs := container.NewHBox()
	b.providerTabs = make(map[string]*widget.Button, len(defaultChatProviders))
	for _, provider := range defaultChatProviders {
		button := widget.NewButton(providerConversationLabel(provider), func() {
			if provider == b.activeConversationProvider && b.openingDefaults[chatKey(b.selectedProject, provider)] {
				return
			}
			b.conversationSelection++
			if provider != b.activeConversationProvider {
				b.standaloneChatHost.Objects = []fyne.CanvasObject{widget.NewLabel("Opening " + providerConversationLabel(provider) + "…")}
				b.standaloneChatHost.Refresh()
				b.newConversationStatus.SetText("")
			}
			b.openProviderConversation(provider)
		})
		b.providerTabs[provider] = button
		tabs.Add(button)
	}
	historyWidth := container.NewGridWrap(fyne.NewSize(230, 0))
	history := browserPanel(container.NewBorder(browserSectionHeading("CHAT HISTORY"), nil, nil, nil, b.projectConversationList))
	historyPane := container.NewStack(historyWidth, history)
	historyPane.Hide()
	tabs.Add(widget.NewButtonWithIcon("History", theme.HistoryIcon(), func() {
		if historyPane.Visible() {
			historyPane.Hide()
		} else {
			historyPane.Show()
		}
	}))
	options := container.NewHBox()
	for _, label := range []string{"Model", "Thinking", "Persona"} {
		var button *widget.Button
		button = widget.NewButton(label, func() { b.editConversationOption(button, label) })
		button.Importance = widget.LowImportance
		b.conversationOptions = append(b.conversationOptions, button)
		options.Add(button)
	}
	b.newChatButton = widget.NewButtonWithIcon("New Chat", theme.ContentAddIcon(), func() {
		if b.openingDefaults[chatKey(b.selectedProject, b.activeConversationProvider)] {
			return
		}
		b.conversationSelection++
		var replace string
		for _, chat := range b.conversationHistory {
			if chat.ID == b.selection.ProjectConversation && chat.TaskID == "" && chat.Provider == b.activeConversationProvider {
				replace = chat.ID
				break
			}
		}
		b.launchStandaloneConversation(b.selectedProject, b.selection.CheckoutPath, b.activeConversationProvider, replace)
	})
	options.Add(b.newChatButton)
	toolbar := browserPanel(container.NewThemeOverride(container.NewHBox(tabs, layout.NewSpacer(), options), controls))
	b.refreshConversationLaunchControls()
	return container.NewBorder(toolbar, b.newConversationStatus, historyPane, nil, b.standaloneChatHost)
}

func (b *Browser) refreshConversationLaunchControls() {
	if len(b.conversationOptions) == 0 {
		return
	}
	provider := b.activeConversationProvider
	prefs := b.conversationLaunchPreferences(provider)
	for name, button := range b.providerTabs {
		button.Importance = widget.MediumImportance
		if name == provider {
			button.Importance = widget.HighImportance
		}
		if b.selectedProject == "" {
			button.Disable()
		} else {
			button.Enable()
		}
		button.Refresh()
	}
	b.conversationOptions[0].SetText("Model: " + conversationOptionValue(prefs.Model, "Default"))
	b.conversationOptions[1].SetText("Thinking: " + conversationOptionValue(prefs.Effort, "Default"))
	b.conversationOptions[2].SetText(conversationOptionValue(prefs.Persona, "No persona"))
	for _, button := range b.conversationOptions {
		if provider == "" {
			button.Hide()
		} else {
			button.Show()
		}
	}
	if provider == "" || (b.workerPanel == nil && b.localConversationsDirectory == "") {
		b.newChatButton.Hide()
	} else {
		b.newChatButton.Show()
	}
}

func conversationOptionValue(value, fallback string) string {
	if value == "" {
		return fallback
	}
	runes := []rune(value)
	if len(runes) > 20 {
		return string(runes[:19]) + "…"
	}
	return value
}

func (b *Browser) editConversationOption(anchor *widget.Button, name string) {
	provider, workspace := b.activeConversationProvider, b.activeWorkspaceID
	driver := fyne.CurrentApp().Driver()
	canvas := driver.CanvasForObject(anchor)
	if provider == "" || canvas == nil {
		return
	}
	prefs := b.conversationLaunchPreferences(provider)
	title := widget.NewLabelWithStyle(name+" · "+providerConversationLabel(provider), fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	var editor fyne.CanvasObject
	var read func() string
	switch name {
	case "Thinking":
		levels := []string{"Default", "low", "medium", "high", "xhigh"}
		if provider == "claude" {
			levels = []string{"Default", "low", "medium", "high", "xhigh", "max"}
		}
		selectLevel := widget.NewSelect(levels, nil)
		selectLevel.SetSelected(conversationOptionValue(prefs.Effort, "Default"))
		editor, read = selectLevel, func() string {
			if selectLevel.Selected == "Default" {
				return ""
			}
			return selectLevel.Selected
		}
	default:
		entry := widget.NewEntry()
		entry.SetPlaceHolder("Provider default")
		if name == "Model" {
			entry.SetText(prefs.Model)
		} else {
			entry.SetText(prefs.Persona)
		}
		editor, read = entry, func() string { return strings.TrimSpace(entry.Text) }
	}
	var popup *widget.PopUp
	closeButton := widget.NewButton("Close", func() { popup.Hide() })
	body := container.NewVBox(title)
	if provider == "antigravity" || (name == "Persona" && provider == "codex") {
		body.Add(widget.NewLabel("This provider does not expose this setting."))
		body.Add(closeButton)
	} else {
		notice := widget.NewLabel("Applies to the next new chat.")
		errText := widget.NewLabel("")
		errText.Wrapping = fyne.TextWrapWord
		save := widget.NewButton("Save", func() {
			if b.activeWorkspaceID != workspace || b.activeConversationProvider != provider {
				popup.Hide()
				return
			}
			switch name {
			case "Model":
				prefs.Model = read()
			case "Thinking":
				prefs.Effort = read()
			case "Persona":
				prefs.Persona = read()
			}
			if _, err := providerLaunchCommand(provider, prefs); err != nil {
				errText.SetText(err.Error())
				return
			}
			b.saveConversationLaunchPreferences(provider, prefs)
			b.refreshConversationLaunchControls()
			popup.Hide()
		})
		save.Importance = widget.HighImportance
		body.Add(editor)
		body.Add(notice)
		body.Add(errText)
		body.Add(container.NewHBox(save, closeButton))
	}
	popup = widget.NewPopUp(settingsCard(body), canvas)
	pos := driver.AbsolutePositionForObject(anchor)
	size := popup.MinSize()
	pos.X = max(0, min(pos.X, canvas.Size().Width-size.Width))
	pos.Y += anchor.Size().Height
	popup.ShowAtPosition(pos)
}
