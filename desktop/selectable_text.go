package desktop

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

func selectableLabel(text string) *widget.Label {
	label := widget.NewLabel(text)
	label.Selectable = true
	return label
}

func copyTaskIDButton(taskID func() string) *widget.Button {
	return widget.NewButtonWithIcon("Copy task ID", theme.ContentCopyIcon(), func() {
		if app := fyne.CurrentApp(); app != nil {
			if id := taskID(); id != "" {
				app.Clipboard().SetContent(id)
			}
		}
	})
}
