package desktop

import (
	"context"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/internal/clientcontrol"
)

func (b *Browser) confirmDeleteProject(project string) {
	window := fyne.CurrentApp().Driver().CanvasForObject(b.root)
	if window == nil {
		return
	}

	input := widget.NewEntry()
	input.SetPlaceHolder(project)
	message := widget.NewLabel("Permanently delete " + project + " and its tasks, conversations, and other gateway data. Local checkout files will remain. Type the project ID to confirm.")
	message.Wrapping = fyne.TextWrapWord
	var confirmation *widget.PopUp
	remove := widget.NewButtonWithIcon("Delete project", theme.DeleteIcon(), func() {
		if input.Text != project {
			return
		}
		confirmation.Hide()
		b.deleteProject(project, input.Text)
	})
	remove.Importance = widget.DangerImportance
	remove.Disable()
	input.OnChanged = func(text string) {
		if text == project {
			remove.Enable()
		} else {
			remove.Disable()
		}
	}
	cancel := widget.NewButton("Cancel", func() { confirmation.Hide() })
	content := container.NewPadded(container.NewVBox(
		browserSectionHeading("PERMANENTLY DELETE PROJECT"), message, input, container.NewHBox(cancel, remove),
	))
	confirmation = widget.NewModalPopUp(content, window)
	confirmation.Resize(fyne.NewSize(min(480, window.Size().Width), content.MinSize().Height+54))
	confirmation.Show()
	window.Focus(input)
}

func (b *Browser) deleteProject(project, confirmation string) {
	b.browserStatus.SetText("Deleting " + project + "…")
	b.launchWithTimeout(0, func(ctx context.Context) {
		err := b.client.DeleteProject(ctx, project, confirmation)
		if err == nil && b.clientConfigPath != "" {
			_ = b.updateClientCheckout(ctx, clientcontrol.CheckoutChange{Project: project})
		}
		b.dispatch(func() {
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				b.browserStatus.SetText("Delete project: " + err.Error())
				return
			}
			b.browserStatus.SetText("Deleted " + project)
			b.Refresh()
		})
	})
}
