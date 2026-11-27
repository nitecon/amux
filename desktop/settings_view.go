package desktop

import (
	"context"
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

func settingsCard(content fyne.CanvasObject) *fyne.Container {
	return container.NewStack(browserCardSurface(),
		container.New(layout.NewCustomPaddedLayout(12, 12, 14, 14), content))
}

func settingsPage(title string, content fyne.CanvasObject) fyne.CanvasObject {
	heading := canvas.NewText(title, theme.ForegroundColor())
	heading.TextSize = 22
	page := container.New(layout.NewCustomPaddedVBoxLayout(16), heading, content)
	return container.NewVScroll(container.New(layout.NewCustomPaddedLayout(18, 18, 22, 22), page))
}

func (b *Browser) settingsView() fyne.CanvasObject {
	type section struct {
		name    string
		icon    fyne.Resource
		content fyne.CanvasObject
	}
	var sections []section
	if b.gatewaySettings != nil {
		sections = append(sections, section{"Gateway", theme.StorageIcon(), b.gatewaySettings.body})
	}
	b.projectsDirectoryEntry = widget.NewEntry()
	b.projectsDirectoryEntry.SetText(b.projectsDirectory())
	b.projectsDirectoryEntry.SetPlaceHolder("Path to the folder that holds your repositories")
	directoryStatus := widget.NewLabel("")
	b.localCheckoutSummary = widget.NewLabel("")
	b.localCheckoutSummary.Wrapping = fyne.TextWrapWord
	saveDirectory := widget.NewButton("Save projects directory", func() {
		if err := b.saveProjectsDirectory(b.projectsDirectoryEntry.Text); err != nil {
			directoryStatus.SetText(err.Error())
			return
		}
		directoryStatus.SetText("Projects directory saved.")
	})
	browseDirectory := widget.NewButtonWithIcon("Browse…", theme.FolderOpenIcon(), nil)
	browseDirectory.OnTapped = func() {
		initial := b.projectsDirectory()
		browseDirectory.Disable()
		if !b.launchWithTimeout(0, func(ctx context.Context) {
			folder, err := pickCheckoutFolder(ctx, "Choose the folder that holds your repositories", initial)
			b.dispatch(func() {
				if ctx.Err() != nil {
					return
				}
				browseDirectory.Enable()
				if err != nil {
					directoryStatus.SetText(err.Error())
				} else if folder != "" {
					b.projectsDirectoryEntry.SetText(folder)
					directoryStatus.SetText("")
				}
			})
		}) {
			browseDirectory.Enable()
		}
	}
	directoryField := container.NewBorder(nil, nil, nil, browseDirectory, b.projectsDirectoryEntry)
	workspace := container.NewVBox(widget.NewLabel("Projects directory"), directoryField,
		container.NewHBox(saveDirectory, layout.NewSpacer()), directoryStatus, b.localCheckoutSummary, widget.NewSeparator(),
		widget.NewLabel("Workspace"), container.NewBorder(nil, nil, nil, b.workspaceClose, b.workspaceSelect),
		widget.NewLabel("Workspace name"), b.workspaceName,
		container.NewHBox(b.workspaceAdd, b.taskWindowToggle, layout.NewSpacer()))
	sections = append(sections,
		section{"Workspace", theme.FolderIcon(), settingsCard(workspace)},
		section{"Automation", theme.SettingsIcon(), settingsCard(
			container.NewVBox(b.pluginSettings.accordion, b.providerSettings.accordion, container.NewHScroll(b.workSubmissionSettings.accordion)))},
	)
	if b.workerPanel != nil {
		sections = append(sections, section{"Workers", theme.ComputerIcon(), settingsCard(container.NewHScroll(b.workerPanel.accordion))})
	}
	if b.appControls != nil {
		sections = append(sections, section{"Updates", theme.ViewRefreshIcon(), settingsCard(b.appControls)})
	}
	controls := toolbarTheme{Theme: fyne.CurrentApp().Settings().Theme()}
	page := container.NewStack()
	rail := container.New(layout.NewCustomPaddedVBoxLayout(6), browserSectionHeading("SETTINGS"))
	buttons := make([]*widget.Button, 0, len(sections))
	pages := make([]fyne.CanvasObject, 0, len(sections))
	selectPage := func(index int) {
		page.Objects = []fyne.CanvasObject{pages[index]}
		page.Refresh()
		for i, button := range buttons {
			button.Importance = widget.LowImportance
			if i == index {
				button.Importance = widget.HighImportance
			}
			button.Refresh()
		}
	}
	for i, item := range sections {
		pages = append(pages, settingsPage(item.name, item.content))
		button := widget.NewButtonWithIcon(item.name, item.icon, func() { selectPage(i) })
		button.Alignment = widget.ButtonAlignLeading
		buttons = append(buttons, button)
		rail.Add(button)
	}
	selectPage(0)
	width := canvas.NewRectangle(theme.InputBackgroundColor())
	width.SetMinSize(fyne.NewSize(156, 0))
	nav := container.NewStack(width, container.New(layout.NewCustomPaddedLayout(14, 14, 10, 10), rail))
	return browserPanel(container.NewBorder(nil, nil, container.NewThemeOverride(nav, controls), nil, page))
}
