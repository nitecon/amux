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

// wrappedLabel is a selectable label that wraps, so long text never widens a
// scrolled transcript past its viewport.
func wrappedLabel(text string) *widget.Label {
	label := selectableLabel(text)
	label.Wrapping = fyne.TextWrapWord
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
