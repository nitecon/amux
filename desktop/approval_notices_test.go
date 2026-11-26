package desktop

import (
	"context"
	"strings"
	"testing"

	"fyne.io/fyne/v2/container"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

func TestApprovalNotificationIdentifiesPendingPermission(t *testing.T) {
	notice := approvalNotification("amux", "antigravity", "Run git log")
	if notice.Title != "Approval needed: Run git log" {
		t.Fatalf("notification title = %q", notice.Title)
	}
	if notice.Content == "" || !strings.Contains(notice.Content, "amux") || !strings.Contains(notice.Content, "Open Events") {
		t.Fatalf("notification content = %q", notice.Content)
	}
}

func TestApprovalCountRemainsVisibleOnEventsNavigation(t *testing.T) {
	button := widget.NewButton("Events", nil)
	b := &Browser{
		mainViewButtons: map[string]*widget.Button{"Events": button},
		approvalBanner:  container.NewVBox(),
		approvalNotices: map[approvalKey]approvalNotice{
			{project: "demo", conversation: "one", request: "a"}: {},
			{project: "demo", conversation: "two", request: "b"}: {},
		},
	}
	b.refreshApprovalNotices()
	if button.Text != "Events · 2 approvals" || button.Importance != widget.HighImportance {
		t.Fatalf("pending approvals were not prominent in navigation: %q (%v)", button.Text, button.Importance)
	}
	if !b.approvalBanner.Visible() || findSettingsButton(b.approvalBanner, "Review approvals") == nil {
		t.Fatal("multiple pending approvals did not display an actionable banner")
	}
}

func TestApprovalWatchFailureRemainsVisibleWithoutPendingRequests(t *testing.T) {
	fynetest.NewApp()
	b := &Browser{approvalBanner: container.NewVBox(), approvalWatchUnavailable: true}
	b.refreshApprovalNotices()
	if !b.approvalBanner.Visible() || len(b.approvalBanner.Objects) != 1 {
		t.Fatal("approval stream failure was hidden")
	}
	label := b.approvalBanner.Objects[0].(*widget.Label)
	if label.Importance != widget.WarningImportance || !strings.Contains(label.Text, "Reconnecting") {
		t.Fatalf("missing reconnect explanation: %q", label.Text)
	}
	b.approvalWatchUnavailable = false
	b.refreshApprovalNotices()
	if b.approvalBanner.Visible() {
		t.Fatal("recovered approval stream still shows a warning")
	}
}

func TestProviderApprovalSurfacesInEventsBeforeOpeningChat(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	b := &Browser{
		dispatch:        func(fn func()) { fn() },
		mainViewButtons: map[string]*widget.Button{"Events": widget.NewButton("Events", nil)},
		approvalRows:    container.NewVBox(),
		approvalBanner:  container.NewVBox(),
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
	if len(b.approvalNotices) != 1 || len(b.approvalRows.Objects) != 2 || b.mainViewButtons["Events"].Text != "Events · 1 approval" {
		t.Fatal("pending approval was not shown exactly once in Events")
	}
	if !b.approvalBanner.Visible() || findSettingsButton(b.approvalBanner, "Open conversation") == nil {
		t.Fatal("pending approval did not display an actionable banner")
	}
	if err := session.ResolvePermission(ctx, "approval", "allow"); err != nil {
		t.Fatal(err)
	}
	if len(b.approvalNotices) != 0 || len(b.approvalRows.Objects) != 0 || b.mainViewButtons["Events"].Text != "Events" {
		t.Fatal("resolved approval remained in Events")
	}
	if b.approvalBanner.Visible() || len(b.approvalBanner.Objects) != 0 {
		t.Fatal("resolved approval banner remained visible")
	}
}
