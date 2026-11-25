package desktop

import (
	"context"
	"testing"

	"fyne.io/fyne/v2"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

func TestProjectDeleteDialogRequiresExactName(t *testing.T) {
	fixture := startEditorBrowser(t, withoutComposerNavigationStream)
	b := fixture.browser
	window := fynetest.NewWindow(b.Content())
	defer window.Close()
	window.Resize(fyne.NewSize(1280, 820))

	b.confirmDeleteProject("demo")
	overlay, ok := window.Canvas().Overlays().Top().(fyne.Widget)
	if !ok {
		t.Fatal("delete did not open a modal confirmation")
	}
	objects := fynetest.WidgetRenderer(overlay).Objects()
	popup := objects[len(objects)-1].(*widget.PopUp)
	content := popup.Content.(*fyne.Container).Objects[0].(*fyne.Container)
	entry := content.Objects[2].(*widget.Entry)
	buttons := content.Objects[3].(*fyne.Container)
	remove := buttons.Objects[1].(*widget.Button)
	if !remove.Disabled() || remove.Importance != widget.DangerImportance {
		t.Fatal("delete should start disabled and display as destructive")
	}
	entry.SetText("Demo")
	if !remove.Disabled() {
		t.Fatal("delete accepted a different name")
	}
	entry.SetText("demo")
	if remove.Disabled() {
		t.Fatal("delete stayed disabled for the exact name")
	}
	fynetest.Tap(remove)
	waitBrowser(t, fixture.callbacks, func() bool {
		project, err := fixture.store.Project(context.Background(), "demo")
		return err == nil && project == nil
	})
}
