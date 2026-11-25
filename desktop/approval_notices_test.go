package desktop

import (
	"context"
	"testing"

	"fyne.io/fyne/v2/container"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

func TestProviderApprovalSurfacesInEventsBeforeOpeningChat(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	b := &Browser{
		dispatch:        func(fn func()) { fn() },
		mainViewButtons: map[string]*widget.Button{"Events": widget.NewButton("Events", nil)},
		approvalRows:    container.NewVBox(),
	}
	ctx := context.Background()
	var emit conversation.Sink
	open := b.watchApprovals(func(_ context.Context, _ storage.WorkerRun, sink conversation.Sink) (worker.Session, error) {
		emit = sink
		return &markdownFixtureSession{done: make(chan struct{})}, nil
	}, nil)
	run := storage.WorkerRun{Project: "demo", ConversationID: "conversation", Owner: storage.AgentOrigin{InstanceID: "instance", Provider: "antigravity"}}
	session, err := open(ctx, run, func(context.Context, conversation.Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := emit(ctx, conversation.Event{Kind: "permission", RequestID: "approval", Title: "Run hostname"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(b.approvalNotices) != 1 || len(b.approvalRows.Objects) != 2 || b.mainViewButtons["Events"].Text != "Events •" {
		t.Fatal("pending approval was not shown exactly once in Events")
	}
	if err := session.ResolvePermission(ctx, "approval", "allow"); err != nil {
		t.Fatal(err)
	}
	if len(b.approvalNotices) != 0 || len(b.approvalRows.Objects) != 0 || b.mainViewButtons["Events"].Text != "Events" {
		t.Fatal("resolved approval remained in Events")
	}
}
