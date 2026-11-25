package desktop

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/chat"
	"testing"

	fynetest "fyne.io/fyne/v2/test"
)

func TestCopyTaskIDUsesCurrentFullID(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	id := "01a20ee8-d968-767d-8e91-75a69b4fb0a4"
	button := copyTaskIDButton(func() string { return id })
	button.OnTapped()
	if got := app.Clipboard().Content(); got != id {
		t.Fatalf("clipboard = %q, want %q", got, id)
	}
}

func TestRenderedConversationTextIsSelectable(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	view := &UI{}
	row := view.renderItem(chat.Item{Kind: "message", Role: "assistant", Text: "Copy part of this message"}).(*fyne.Container)
	if !row.Objects[1].(*widget.Label).Selectable {
		t.Fatal("conversation text cannot be selected")
	}
}
