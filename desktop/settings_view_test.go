package desktop

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

func openBrowserSettingsPage(t *testing.T, b *Browser, name string) {
	t.Helper()
	b.setMainView("Settings")
	button := findSettingsButton(b.mainViews["Settings"], name)
	if button == nil {
		t.Fatalf("missing Settings navigation button %q", name)
	}
	fynetest.Tap(button)
}

func findSettingsButton(object fyne.CanvasObject, name string) *widget.Button {
	switch object := object.(type) {
	case *widget.Button:
		if object.Text == name {
			return object
		}
	case *fyne.Container:
		for _, child := range object.Objects {
			if button := findSettingsButton(child, name); button != nil {
				return button
			}
		}
	case *container.ThemeOverride:
		return findSettingsButton(object.Content, name)
	}
	return nil
}
